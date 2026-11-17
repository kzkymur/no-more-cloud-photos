import Foundation
import SQLite3
import os

private let storeLog = Logger(subsystem: "io.nmcp.uploader", category: "store")

/// sqlite3 wants a destructor sentinel; SQLITE_TRANSIENT (-1) tells it to copy.
private let SQLITE_TRANSIENT_DESTRUCTOR = unsafeBitCast(-1, to: sqlite3_destructor_type.self)

enum ItemState: Int {
  case pending = 0
  case uploading = 1
  case uploaded = 2
  case failed = 3
}

struct UploadItem {
  let id: String
  let assetId: String
  let resourceType: Int
  let name: String
  let mime: String
  let modified: Double
  let state: ItemState
  let attempts: Int
  let lastError: String?
  let permanent: Bool
  let contentSha: String?
  let mediaId: String?
  let taskId: Int?
}

/// One original resource found by the library scanner.
struct ScannedResource {
  let id: String
  let assetId: String
  let resourceType: Int
  let name: String
  let mime: String
  let modified: Double
}

struct ItemCounts {
  var total = 0
  var uploaded = 0
  var pending = 0
  var failed = 0
}

struct SQLiteError: Error, CustomStringConvertible {
  let code: Int32
  let message: String
  var description: String { "SQLite error \(code): \(message)" }
}

/// SQLite-backed upload queue. Every public method hops onto one private
/// serial queue (`queue.sync`), so the single connection is never used
/// concurrently. Methods must not be called re-entrantly from that queue.
final class UploadStore: @unchecked Sendable {
  static let shared = UploadStore()

  private let queue = DispatchQueue(label: "io.nmcp.uploader.store")
  private var db: OpaquePointer?

  private static let columns =
    "id, asset_id, resource_type, name, mime, modified, state, attempts, last_error, permanent, content_sha, media_id, task_id"

  private enum Value {
    case text(String)
    case int(Int64)
    case double(Double)
    case null
  }

  // MARK: - Public API

  /// False when the database cannot be opened (e.g. data protection before
  /// the first unlock after boot).
  func isAvailable() -> Bool {
    queue.sync { openIfNeeded() != nil }
  }

  /// asset_id -> modified (seconds since 1970) for every known asset.
  func knownAssetModifications() -> [String: Double] {
    withDB([:]) { db in
      var result: [String: Double] = [:]
      try query(db, "SELECT asset_id, MAX(modified) FROM items GROUP BY asset_id", []) { stmt in
        if let asset = columnText(stmt, 0) {
          result[asset] = sqlite3_column_double(stmt, 1)
        }
      }
      return result
    }
  }

  /// Inserts new resources as pending. Existing rows keep their state; only
  /// `modified` is refreshed (and name/mime for rows not yet uploaded).
  /// Originals do not change when a photo is edited, so an uploaded row is
  /// never re-queued here.
  func upsert(_ items: [ScannedResource]) {
    guard !items.isEmpty else { return }
    let now = Date().timeIntervalSince1970
    withDB(()) { db in
      try transaction(db) {
        let sql = """
          INSERT INTO items (id, asset_id, resource_type, name, mime, modified, state, attempts, last_error,
                             permanent, content_sha, media_id, task_id, updated_at)
          VALUES (?, ?, ?, ?, ?, ?, 0, 0, NULL, 0, NULL, NULL, NULL, ?)
          ON CONFLICT(id) DO UPDATE SET
            modified = excluded.modified,
            name = CASE WHEN items.state = 2 THEN items.name ELSE excluded.name END,
            mime = CASE WHEN items.state = 2 THEN items.mime ELSE excluded.mime END
          """
        let stmt = try prepare(db, sql)
        defer { sqlite3_finalize(stmt) }
        for item in items {
          sqlite3_reset(stmt)
          sqlite3_clear_bindings(stmt)
          try bind(stmt, [
            .text(item.id), .text(item.assetId), .int(Int64(item.resourceType)), .text(item.name),
            .text(item.mime), .double(item.modified), .double(now),
          ])
          try stepDone(db, stmt)
        }
      }
    }
  }

  /// Deletes pending/failed rows whose asset was not seen by a complete scan.
  /// Uploaded rows are kept as history; in-flight rows are left alone.
  @discardableResult
  func removeVanished(keeping assetIds: Set<String>) -> Int {
    withDB(0) { db in
      var removed = 0
      try exec(db, "CREATE TEMP TABLE IF NOT EXISTS seen_assets (asset_id TEXT PRIMARY KEY)")
      try transaction(db) {
        try exec(db, "DELETE FROM seen_assets")
        let stmt = try prepare(db, "INSERT OR IGNORE INTO seen_assets (asset_id) VALUES (?)")
        defer { sqlite3_finalize(stmt) }
        for asset in assetIds {
          sqlite3_reset(stmt)
          sqlite3_clear_bindings(stmt)
          try bind(stmt, [.text(asset)])
          try stepDone(db, stmt)
        }
        try execute(db, "DELETE FROM items WHERE state IN (0, 3) AND asset_id NOT IN (SELECT asset_id FROM seen_assets)", [])
        removed = Int(sqlite3_changes(db))
        try exec(db, "DELETE FROM seen_assets")
      }
      return removed
    }
  }

  /// Next item to upload: newest pending first, then retryable failures that
  /// were not already attempted in the current run (`updated_at < failedBefore`).
  func nextCandidate(failedBefore: Double, maxAttempts: Int) -> UploadItem? {
    withDB(nil) { db in
      if let pending = try queryItems(
        db, "SELECT \(Self.columns) FROM items WHERE state = 0 ORDER BY modified DESC LIMIT 1", []
      ).first {
        return pending
      }
      return try queryItems(
        db,
        """
        SELECT \(Self.columns) FROM items
        WHERE state = 3 AND permanent = 0 AND attempts < ? AND updated_at < ?
        ORDER BY updated_at ASC LIMIT 1
        """,
        [.int(Int64(maxAttempts)), .double(failedBefore)]
      ).first
    }
  }

  func item(id: String) -> UploadItem? {
    withDB(nil) { db in
      try queryItems(db, "SELECT \(Self.columns) FROM items WHERE id = ?", [.text(id)]).first
    }
  }

  func item(taskId: Int) -> UploadItem? {
    withDB(nil) { db in
      try queryItems(
        db, "SELECT \(Self.columns) FROM items WHERE task_id = ? AND state = 1 LIMIT 1", [.int(Int64(taskId))]
      ).first
    }
  }

  func markUploading(id: String, taskId: Int, contentSha: String) {
    update("UPDATE items SET state = 1, task_id = ?, content_sha = ?, updated_at = ? WHERE id = ?",
           [.int(Int64(taskId)), .text(contentSha), .double(now()), .text(id)])
  }

  func markUploaded(id: String, mediaId: String?) {
    update(
      """
      UPDATE items SET state = 2, media_id = ?, last_error = NULL, permanent = 0, task_id = NULL, updated_at = ?
      WHERE id = ?
      """,
      [mediaId.map { Value.text($0) } ?? Value.null, .double(now()), .text(id)])
  }

  func markFailed(id: String, error: String, permanent: Bool, countAttempt: Bool) {
    update(
      """
      UPDATE items SET state = 3, last_error = ?, permanent = ?, attempts = attempts + ?, task_id = NULL, updated_at = ?
      WHERE id = ?
      """,
      [.text(error), .int(permanent ? 1 : 0), .int(countAttempt ? 1 : 0), .double(now()), .text(id)])
  }

  /// Back to pending without counting an attempt (cancellation, stale task).
  /// Uploaded rows are never downgraded.
  func markPending(id: String) {
    update("UPDATE items SET state = 0, task_id = NULL, updated_at = ? WHERE id = ? AND state <> 2",
           [.double(now()), .text(id)])
  }

  func delete(id: String) {
    update("DELETE FROM items WHERE id = ? AND state <> 2", [.text(id)])
  }

  /// Resets rows stuck in `uploading` whose URLSession task no longer exists.
  func resetUploading(exceptItemIds keep: Set<String>) {
    withDB(()) { db in
      let rows = try queryItems(db, "SELECT \(Self.columns) FROM items WHERE state = 1", [])
      for row in rows where !keep.contains(row.id) {
        try execute(db, "UPDATE items SET state = 0, task_id = NULL, updated_at = ? WHERE id = ?",
                    [.double(now()), .text(row.id)])
      }
    }
  }

  func counts() -> ItemCounts {
    withDB(ItemCounts()) { db in
      var counts = ItemCounts()
      try query(
        db,
        """
        SELECT COUNT(*),
               COALESCE(SUM(CASE WHEN state = 2 THEN 1 ELSE 0 END), 0),
               COALESCE(SUM(CASE WHEN state IN (0, 1) THEN 1 ELSE 0 END), 0),
               COALESCE(SUM(CASE WHEN state = 3 THEN 1 ELSE 0 END), 0)
        FROM items
        """,
        []
      ) { stmt in
        counts.total = Int(sqlite3_column_int64(stmt, 0))
        counts.uploaded = Int(sqlite3_column_int64(stmt, 1))
        counts.pending = Int(sqlite3_column_int64(stmt, 2))
        counts.failed = Int(sqlite3_column_int64(stmt, 3))
      }
      return counts
    }
  }

  func failedItems(limit: Int) -> [UploadItem] {
    withDB([]) { db in
      try queryItems(
        db, "SELECT \(Self.columns) FROM items WHERE state = 3 ORDER BY updated_at DESC LIMIT ?",
        [.int(Int64(max(0, limit)))])
    }
  }

  /// retryFailed(): every failed row (including permanent ones) back to pending.
  func requeueAllFailed() {
    update("UPDATE items SET state = 0, attempts = 0, permanent = 0, task_id = NULL, updated_at = ? WHERE state = 3",
           [.double(now())])
  }

  // MARK: - Connection

  private func openIfNeeded() -> OpaquePointer? {
    if let db {
      return db
    }
    do {
      try NmcpPaths.ensureDirectory(NmcpPaths.supportDirectory, excludeFromBackup: true)
    } catch {
      storeLog.error("Cannot create support directory: \(error.localizedDescription, privacy: .public)")
      return nil
    }
    var handle: OpaquePointer?
    let flags = SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE | SQLITE_OPEN_FULLMUTEX
    let rc = sqlite3_open_v2(NmcpPaths.databaseFile.path, &handle, flags, nil)
    guard rc == SQLITE_OK, let handle else {
      let message = handle.map { String(cString: sqlite3_errmsg($0)) } ?? "unknown"
      storeLog.error("sqlite3_open_v2 failed (\(rc)): \(message, privacy: .public)")
      if let handle {
        sqlite3_close(handle)
      }
      return nil
    }
    do {
      try exec(handle, "PRAGMA journal_mode = WAL")
      try exec(handle, "PRAGMA synchronous = NORMAL")
      try migrate(handle)
    } catch {
      storeLog.error("Database setup failed: \(String(describing: error), privacy: .public)")
      sqlite3_close(handle)
      return nil
    }
    db = handle
    return handle
  }

  private func migrate(_ db: OpaquePointer) throws {
    var version: Int64 = 0
    try query(db, "PRAGMA user_version", []) { stmt in
      version = sqlite3_column_int64(stmt, 0)
    }
    if version < 1 {
      try transaction(db) {
        try exec(db, """
          CREATE TABLE IF NOT EXISTS items (
            id TEXT PRIMARY KEY,
            asset_id TEXT NOT NULL,
            resource_type INTEGER NOT NULL,
            name TEXT NOT NULL,
            mime TEXT NOT NULL,
            modified REAL NOT NULL,
            state INTEGER NOT NULL DEFAULT 0,
            attempts INTEGER NOT NULL DEFAULT 0,
            last_error TEXT,
            permanent INTEGER NOT NULL DEFAULT 0,
            content_sha TEXT,
            media_id TEXT,
            task_id INTEGER,
            updated_at REAL NOT NULL
          )
          """)
        try exec(db, "CREATE INDEX IF NOT EXISTS idx_items_state ON items (state, modified)")
        try exec(db, "CREATE INDEX IF NOT EXISTS idx_items_asset ON items (asset_id)")
        try exec(db, "PRAGMA user_version = 1")
      }
    }
  }

  // MARK: - Helpers (call only on `queue`)

  private func withDB<T>(_ fallback: T, _ body: (OpaquePointer) throws -> T) -> T {
    queue.sync {
      guard let db = openIfNeeded() else {
        return fallback
      }
      do {
        return try body(db)
      } catch {
        storeLog.error("Database operation failed: \(String(describing: error), privacy: .public)")
        return fallback
      }
    }
  }

  private func update(_ sql: String, _ values: [Value]) {
    withDB(()) { db in
      try execute(db, sql, values)
    }
  }

  private func now() -> Double {
    Date().timeIntervalSince1970
  }

  private func error(_ db: OpaquePointer, _ code: Int32) -> SQLiteError {
    SQLiteError(code: code, message: String(cString: sqlite3_errmsg(db)))
  }

  private func exec(_ db: OpaquePointer, _ sql: String) throws {
    let rc = sqlite3_exec(db, sql, nil, nil, nil)
    guard rc == SQLITE_OK else {
      throw error(db, rc)
    }
  }

  private func transaction(_ db: OpaquePointer, _ body: () throws -> Void) throws {
    try exec(db, "BEGIN IMMEDIATE")
    do {
      try body()
      try exec(db, "COMMIT")
    } catch {
      _ = sqlite3_exec(db, "ROLLBACK", nil, nil, nil)
      throw error
    }
  }

  private func prepare(_ db: OpaquePointer, _ sql: String) throws -> OpaquePointer {
    var stmt: OpaquePointer?
    let rc = sqlite3_prepare_v2(db, sql, -1, &stmt, nil)
    guard rc == SQLITE_OK, let stmt else {
      throw error(db, rc)
    }
    return stmt
  }

  private func bind(_ stmt: OpaquePointer, _ values: [Value]) throws {
    for (offset, value) in values.enumerated() {
      let index = Int32(offset + 1)
      let rc: Int32
      switch value {
      case .text(let text):
        rc = sqlite3_bind_text(stmt, index, text, -1, SQLITE_TRANSIENT_DESTRUCTOR)
      case .int(let int):
        rc = sqlite3_bind_int64(stmt, index, int)
      case .double(let double):
        rc = sqlite3_bind_double(stmt, index, double)
      case .null:
        rc = sqlite3_bind_null(stmt, index)
      }
      guard rc == SQLITE_OK else {
        throw SQLiteError(code: rc, message: "bind failed at index \(index)")
      }
    }
  }

  private func stepDone(_ db: OpaquePointer, _ stmt: OpaquePointer) throws {
    let rc = sqlite3_step(stmt)
    guard rc == SQLITE_DONE else {
      throw error(db, rc)
    }
  }

  private func execute(_ db: OpaquePointer, _ sql: String, _ values: [Value]) throws {
    let stmt = try prepare(db, sql)
    defer { sqlite3_finalize(stmt) }
    try bind(stmt, values)
    try stepDone(db, stmt)
  }

  private func query(_ db: OpaquePointer, _ sql: String, _ values: [Value], row: (OpaquePointer) throws -> Void) throws {
    let stmt = try prepare(db, sql)
    defer { sqlite3_finalize(stmt) }
    try bind(stmt, values)
    while true {
      let rc = sqlite3_step(stmt)
      if rc == SQLITE_ROW {
        try row(stmt)
      } else if rc == SQLITE_DONE {
        return
      } else {
        throw error(db, rc)
      }
    }
  }

  private func queryItems(_ db: OpaquePointer, _ sql: String, _ values: [Value]) throws -> [UploadItem] {
    var items: [UploadItem] = []
    try query(db, sql, values) { stmt in
      items.append(UploadItem(
        id: columnText(stmt, 0) ?? "",
        assetId: columnText(stmt, 1) ?? "",
        resourceType: Int(sqlite3_column_int64(stmt, 2)),
        name: columnText(stmt, 3) ?? "",
        mime: columnText(stmt, 4) ?? "application/octet-stream",
        modified: sqlite3_column_double(stmt, 5),
        state: ItemState(rawValue: Int(sqlite3_column_int64(stmt, 6))) ?? .pending,
        attempts: Int(sqlite3_column_int64(stmt, 7)),
        lastError: columnText(stmt, 8),
        permanent: sqlite3_column_int64(stmt, 9) != 0,
        contentSha: columnText(stmt, 10),
        mediaId: columnText(stmt, 11),
        taskId: sqlite3_column_type(stmt, 12) == SQLITE_NULL ? nil : Int(sqlite3_column_int64(stmt, 12))
      ))
    }
    return items
  }
}

private func columnText(_ stmt: OpaquePointer, _ index: Int32) -> String? {
  guard sqlite3_column_type(stmt, index) != SQLITE_NULL, let raw = sqlite3_column_text(stmt, index) else {
    return nil
  }
  return String(cString: raw)
}
