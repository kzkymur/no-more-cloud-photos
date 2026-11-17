import Foundation
import SQLite3
import XCTest
@testable import NmcpCore

final class UploadStoreTests: XCTestCase {
  private var dir: TempDir!
  private var store: UploadStore!

  private let maxAttempts = 10

  override func setUpWithError() throws {
    dir = try TempDir()
    store = UploadStore(databaseURL: dir.url.appendingPathComponent("db/uploader.sqlite"))
  }

  override func tearDown() {
    store?.close()
    store = nil
    dir = nil
  }

  private func resource(_ asset: String, type: Int = 1, name: String? = nil, modified: Double) -> ScannedResource {
    ScannedResource(id: "\(asset)#\(type)", assetId: asset, resourceType: type, name: name ?? "\(asset).HEIC",
                    mime: "image/heic", modified: modified)
  }

  /// Every failed row was updated "now", so a run starting later may retry it.
  private var later: Double { Date().timeIntervalSince1970 + 60 }
  private var earlier: Double { Date().timeIntervalSince1970 - 60 }

  private func next(failedBefore: Double? = nil) -> String? {
    store.nextCandidate(failedBefore: failedBefore ?? later, maxAttempts: maxAttempts)?.id
  }

  // MARK: Scan semantics

  func testUpsertInsertsPendingAndKeepsState() throws {
    store.upsert([resource("A", modified: 1), resource("B", modified: 2)])
    XCTAssertEqual(store.counts().total, 2)
    XCTAssertEqual(store.counts().pending, 2)
    let a = try XCTUnwrap(store.item(id: "A#1"))
    XCTAssertEqual(a.state, .pending)
    XCTAssertEqual(a.attempts, 0)
    XCTAssertEqual(a.interrupts, 0)
    XCTAssertEqual(a.mime, "image/heic")

    store.markUploading(id: "A#1", taskId: 7, contentSha: "s1")
    store.markUploaded(id: "A#1", mediaId: "m1")
    // Rescan with a new modification date and name (e.g. edited in Photos):
    // uploaded rows are never re-queued and keep the name that was sent.
    store.upsert([resource("A", name: "renamed.HEIC", modified: 5)])
    let uploaded = try XCTUnwrap(store.item(id: "A#1"))
    XCTAssertEqual(uploaded.state, .uploaded)
    XCTAssertEqual(uploaded.name, "A.HEIC")
    XCTAssertEqual(uploaded.mediaId, "m1")
    XCTAssertEqual(uploaded.modified, 5)
    XCTAssertEqual(store.knownAssetModifications()["A"], 5)

    store.upsert([resource("B", name: "new.HEIC", modified: 2)])
    XCTAssertEqual(store.item(id: "B#1")?.name, "new.HEIC")
  }

  func testRemoveVanishedKeepsUploadedAndInFlight() {
    store.upsert([resource("A", modified: 1), resource("B", modified: 2), resource("C", modified: 3), resource("D", modified: 4)])
    store.markUploading(id: "A#1", taskId: 1, contentSha: "a")
    store.markUploaded(id: "A#1", mediaId: "m")
    store.markUploading(id: "B#1", taskId: 2, contentSha: "b")
    store.markFailed(id: "C#1", error: "x", permanent: false, countAttempt: true)
    // E.g. limited access now only shows D.
    let removed = store.removeVanished(keeping: ["D"])
    XCTAssertEqual(removed, 1)
    XCTAssertNotNil(store.item(id: "A#1"), "uploaded history is kept")
    XCTAssertNotNil(store.item(id: "B#1"), "in-flight row is kept")
    XCTAssertNil(store.item(id: "C#1"))
    XCTAssertNotNil(store.item(id: "D#1"))
    // Back to full access: the asset is simply inserted again.
    store.upsert([resource("C", modified: 3)])
    XCTAssertEqual(store.item(id: "C#1")?.state, .pending)
  }

  func testDeleteNeverRemovesUploaded() {
    store.upsert([resource("A", modified: 1), resource("B", modified: 2)])
    store.markUploaded(id: "A#1", mediaId: nil)
    store.delete(id: "A#1")
    store.delete(id: "B#1")
    XCTAssertNotNil(store.item(id: "A#1"))
    XCTAssertNil(store.item(id: "B#1"))
  }

  // MARK: Queue order

  func testNewestPendingFirst() {
    store.upsert([resource("old", modified: 1), resource("new", modified: 3), resource("mid", modified: 2)])
    XCTAssertEqual(next(), "new#1")
    store.markUploaded(id: "new#1", mediaId: nil)
    XCTAssertEqual(next(), "mid#1")
  }

  func testInterruptedItemsMoveBehind() {
    store.upsert([resource("big", modified: 3), resource("b", modified: 2), resource("c", modified: 1)])
    XCTAssertEqual(next(), "big#1")
    store.markUploading(id: "big#1", taskId: 1, contentSha: "s")
    store.markInterrupted(id: "big#1", taskId: 1)
    XCTAssertEqual(next(), "b#1")
    store.markInterrupted(id: "b#1")
    XCTAssertEqual(next(), "c#1")
    store.markInterrupted(id: "c#1")
    // All interrupted once: newest first again.
    XCTAssertEqual(next(), "big#1")
    store.markInterrupted(id: "big#1")
    XCTAssertEqual(next(), "b#1")
  }

  func testPendingBeforeFailedWithinSameInterruptCount() {
    store.upsert([resource("failedNew", modified: 9), resource("pendingOld", modified: 1)])
    store.markFailed(id: "failedNew#1", error: "HTTP 500", permanent: false, countAttempt: true)
    XCTAssertEqual(next(), "pendingOld#1")
    store.markInterrupted(id: "pendingOld#1")
    // Interrupts dominate state: a failure interrupted fewer times goes first.
    XCTAssertEqual(next(), "failedNew#1")
  }

  func testFailedRetriedOncePerRunAndCapped() {
    store.upsert([resource("A", modified: 1), resource("P", modified: 2)])
    store.markFailed(id: "A#1", error: "HTTP 500", permanent: false, countAttempt: true)
    store.markFailed(id: "P#1", error: "HTTP 415", permanent: true, countAttempt: true)
    // Failed during the current run (updated after run start): not again.
    XCTAssertNil(next(failedBefore: earlier))
    XCTAssertEqual(next(failedBefore: later), "A#1", "permanent failures are never retried automatically")
    for _ in 1..<maxAttempts {
      store.markFailed(id: "A#1", error: "HTTP 500", permanent: false, countAttempt: true)
    }
    XCTAssertEqual(store.item(id: "A#1")?.attempts, maxAttempts)
    XCTAssertNil(next(), "attempts exhausted")
  }

  func testUncountedFailureIsRetriedForever() {
    store.upsert([resource("cloud", modified: 1)])
    for _ in 0..<(maxAttempts + 5) {
      store.markFailed(id: "cloud#1", error: "offline", permanent: false, countAttempt: false)
    }
    XCTAssertEqual(store.item(id: "cloud#1")?.attempts, 0)
    XCTAssertEqual(next(), "cloud#1")
  }

  // MARK: Interrupts

  func testMarkInterruptedDoesNotBumpAttempts() throws {
    store.upsert([resource("A", modified: 1)])
    store.markFailed(id: "A#1", error: "HTTP 500", permanent: false, countAttempt: true)
    store.markUploading(id: "A#1", taskId: 3, contentSha: "s")
    store.markInterrupted(id: "A#1", taskId: 3)
    let item = try XCTUnwrap(store.item(id: "A#1"))
    XCTAssertEqual(item.state, .pending)
    XCTAssertEqual(item.attempts, 1)
    XCTAssertEqual(item.interrupts, 1)
    XCTAssertNil(item.taskId)
  }

  func testStagingCountsAsInterruptUntilItEnds() throws {
    store.upsert([resource("A", modified: 2), resource("B", modified: 1)])
    // Staging cut off (expiry, suspension, kill): the interrupt stays.
    store.markStagingStarted(id: "A#1")
    XCTAssertEqual(store.item(id: "A#1")?.interrupts, 1)
    XCTAssertEqual(next(), "B#1")
    // Staging that reaches a result does not count.
    store.markStagingStarted(id: "B#1")
    store.markStagingEnded(id: "B#1")
    XCTAssertEqual(store.item(id: "B#1")?.interrupts, 0)
    XCTAssertEqual(store.item(id: "B#1")?.attempts, 0)
    store.markStagingEnded(id: "B#1")
    XCTAssertEqual(store.item(id: "B#1")?.interrupts, 0, "never negative")
    // Staging an uploaded row is a no-op.
    store.markUploaded(id: "B#1", mediaId: nil)
    store.markStagingStarted(id: "B#1")
    XCTAssertEqual(store.item(id: "B#1")?.interrupts, 0)
  }

  func testContentChangeResetsInterrupts() {
    store.upsert([resource("A", modified: 1)])
    store.markUploading(id: "A#1", taskId: 1, contentSha: "s1")
    store.markInterrupted(id: "A#1", taskId: 1)
    store.markUploading(id: "A#1", taskId: 2, contentSha: "s1")
    store.markInterrupted(id: "A#1", taskId: 2)
    XCTAssertEqual(store.item(id: "A#1")?.interrupts, 2)
    store.markUploading(id: "A#1", taskId: 3, contentSha: "s1")
    XCTAssertEqual(store.item(id: "A#1")?.interrupts, 2, "same content keeps the history")
    store.markInterrupted(id: "A#1", taskId: 3)
    store.markUploading(id: "A#1", taskId: 4, contentSha: "s2")
    XCTAssertEqual(store.item(id: "A#1")?.interrupts, 0)
    XCTAssertEqual(store.item(id: "A#1")?.contentSha, "s2")
  }

  func testRetryFailedResetsEverything() throws {
    store.upsert([resource("A", modified: 1), resource("B", modified: 2), resource("C", modified: 3)])
    store.markInterrupted(id: "A#1")
    store.markInterrupted(id: "A#1")
    store.markFailed(id: "A#1", error: "HTTP 415", permanent: true, countAttempt: true)
    store.markInterrupted(id: "B#1")
    store.markFailed(id: "C#1", error: "HTTP 500", permanent: false, countAttempt: true)
    XCTAssertEqual(store.failedItems(limit: 10).count, 2)
    store.requeueAllFailed()
    let a = try XCTUnwrap(store.item(id: "A#1"))
    XCTAssertEqual(a.state, .pending)
    XCTAssertEqual(a.attempts, 0)
    XCTAssertEqual(a.interrupts, 0)
    XCTAssertFalse(a.permanent)
    XCTAssertEqual(store.item(id: "B#1")?.interrupts, 1, "pending rows are not touched")
    XCTAssertEqual(store.item(id: "C#1")?.state, .pending)
    XCTAssertTrue(store.failedItems(limit: 10).isEmpty)
  }

  // MARK: Integrity of late / stale task events

  func testStaleTaskEventsDoNotClobberNewerAttempt() throws {
    store.upsert([resource("A", modified: 1)])
    store.markUploading(id: "A#1", taskId: 10, contentSha: "s")
    // Old task 9 reports failure / cancellation: ignored.
    store.markFailed(id: "A#1", error: "late", permanent: true, countAttempt: true, taskId: 9)
    store.markInterrupted(id: "A#1", taskId: 9)
    store.markPending(id: "A#1", taskId: 9)
    var item = try XCTUnwrap(store.item(id: "A#1"))
    XCTAssertEqual(item.state, .uploading)
    XCTAssertEqual(item.taskId, 10)
    XCTAssertEqual(item.attempts, 0)
    XCTAssertEqual(item.interrupts, 0)
    // The current task's result applies.
    store.markFailed(id: "A#1", error: "HTTP 500", permanent: false, countAttempt: true, taskId: 10)
    item = try XCTUnwrap(store.item(id: "A#1"))
    XCTAssertEqual(item.state, .failed)
    XCTAssertEqual(item.attempts, 1)
  }

  func testUploadedIsNeverDowngraded() throws {
    store.upsert([resource("A", modified: 1)])
    store.markUploading(id: "A#1", taskId: 1, contentSha: "s")
    store.markUploaded(id: "A#1", mediaId: "m")
    store.markFailed(id: "A#1", error: "late", permanent: true, countAttempt: true)
    store.markPending(id: "A#1")
    store.markInterrupted(id: "A#1")
    store.markUploading(id: "A#1", taskId: 2, contentSha: "s")
    let item = try XCTUnwrap(store.item(id: "A#1"))
    XCTAssertEqual(item.state, .uploaded)
    XCTAssertEqual(item.mediaId, "m")
    XCTAssertNil(item.lastError)
    XCTAssertEqual(item.interrupts, 0)
  }

  func testItemByTaskId() {
    store.upsert([resource("A", modified: 1)])
    store.markUploading(id: "A#1", taskId: 77, contentSha: "s")
    XCTAssertEqual(store.item(taskId: 77)?.id, "A#1")
    XCTAssertNil(store.item(taskId: 78))
  }

  // MARK: Reconcile after relaunch

  func testResetUploadingRequeuesLostTasksAsInterrupted() throws {
    store.upsert([resource("lost", modified: 1), resource("alive", modified: 2), resource("done", modified: 3)])
    store.markUploading(id: "lost#1", taskId: 1, contentSha: "a")
    store.markUploading(id: "alive#1", taskId: 2, contentSha: "b")
    store.markUploading(id: "done#1", taskId: 3, contentSha: "c")
    store.markUploaded(id: "done#1", mediaId: "m")
    let reset = store.resetUploading(exceptItemIds: ["alive#1"])
    XCTAssertEqual(reset, 1)
    let lost = try XCTUnwrap(store.item(id: "lost#1"))
    XCTAssertEqual(lost.state, .pending)
    XCTAssertEqual(lost.attempts, 0)
    XCTAssertEqual(lost.interrupts, 1)
    XCTAssertNil(lost.taskId)
    XCTAssertEqual(store.item(id: "alive#1")?.state, .uploading)
    XCTAssertEqual(store.item(id: "done#1")?.state, .uploaded)
    // A completion of the lost task delivered after the reset is ignored...
    store.markInterrupted(id: "lost#1", taskId: 1)
    XCTAssertEqual(store.item(id: "lost#1")?.interrupts, 1)
    // ...but a success is always recorded.
    store.markUploaded(id: "lost#1", mediaId: "m2")
    XCTAssertEqual(store.item(id: "lost#1")?.state, .uploaded)
    // Nothing is left stuck in "uploading" except the live task.
    XCTAssertEqual(store.resetUploading(exceptItemIds: ["alive#1"]), 0)
    XCTAssertEqual(store.resetUploading(exceptItemIds: []), 1)
    XCTAssertEqual(store.item(id: "alive#1")?.state, .pending)
  }

  func testCountsTreatUploadingAsPending() {
    store.upsert([resource("A", modified: 1), resource("B", modified: 2), resource("C", modified: 3), resource("D", modified: 4)])
    store.markUploading(id: "A#1", taskId: 1, contentSha: "a")
    store.markUploaded(id: "B#1", mediaId: nil)
    store.markFailed(id: "C#1", error: "e", permanent: false, countAttempt: true)
    let counts = store.counts()
    XCTAssertEqual(counts.total, 4)
    XCTAssertEqual(counts.uploaded, 1)
    XCTAssertEqual(counts.pending, 2)
    XCTAssertEqual(counts.failed, 1)
  }

  func testDataSurvivesReopen() {
    store.upsert([resource("A", modified: 1)])
    store.markInterrupted(id: "A#1")
    store.close()
    let reopened = UploadStore(databaseURL: store.databaseURL)
    XCTAssertEqual(reopened.item(id: "A#1")?.interrupts, 1)
    XCTAssertEqual(reopened.userVersion(), UploadStore.schemaVersion)
    reopened.close()
  }

  // MARK: Schema

  func testFreshDatabaseIsCurrentVersion() {
    XCTAssertTrue(store.isAvailable())
    XCTAssertEqual(store.userVersion(), 2)
    XCTAssertEqual(UploadStore.schemaVersion, 2)
  }

  /// The exact v1 schema shipped before the `interrupts` column.
  private static let v1Schema = """
    CREATE TABLE items (
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
    );
    CREATE INDEX idx_items_state ON items (state, modified);
    CREATE INDEX idx_items_asset ON items (asset_id);
    INSERT INTO items (id, asset_id, resource_type, name, mime, modified, state, attempts, last_error, permanent,
                       content_sha, media_id, task_id, updated_at)
    VALUES ('up#1', 'up', 1, 'IMG_0001.HEIC', 'image/heic', 10, 2, 1, NULL, 0, 'sha-up', 'm-1', NULL, 1),
           ('fl#1', 'fl', 1, 'IMG_0002.HEIC', 'image/heic', 20, 3, 4, 'HTTP 500', 0, NULL, NULL, NULL, 1),
           ('go#9', 'go', 9, 'IMG_0003.MOV', 'video/quicktime', 30, 1, 0, NULL, 0, 'sha-go', NULL, 5, 1),
           ('pe#1', 'pe', 1, 'IMG_0004.JPG', 'image/jpeg', 40, 0, 0, NULL, 0, NULL, NULL, NULL, 1);
    PRAGMA user_version = 1;
    """

  private func execRaw(_ path: String, _ sql: String) throws {
    var db: OpaquePointer?
    XCTAssertEqual(sqlite3_open(path, &db), SQLITE_OK)
    defer { sqlite3_close(db) }
    var message: UnsafeMutablePointer<CChar>?
    let rc = sqlite3_exec(db, sql, nil, nil, &message)
    if rc != SQLITE_OK {
      let text = message.map { String(cString: $0) } ?? "?"
      sqlite3_free(message)
      throw SQLiteError(code: rc, message: text)
    }
  }

  func testMigrationFromV1KeepsRowsAndAddsInterrupts() throws {
    let url = dir.url.appendingPathComponent("v1/uploader.sqlite")
    try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
    try execRaw(url.path, Self.v1Schema)

    let migrated = UploadStore(databaseURL: url)
    defer { migrated.close() }
    XCTAssertTrue(migrated.isAvailable())
    XCTAssertEqual(migrated.userVersion(), 2)

    let uploaded = try XCTUnwrap(migrated.item(id: "up#1"))
    XCTAssertEqual(uploaded.state, .uploaded)
    XCTAssertEqual(uploaded.mediaId, "m-1")
    XCTAssertEqual(uploaded.contentSha, "sha-up")
    XCTAssertEqual(uploaded.interrupts, 0)
    let failed = try XCTUnwrap(migrated.item(id: "fl#1"))
    XCTAssertEqual(failed.attempts, 4)
    XCTAssertEqual(failed.lastError, "HTTP 500")
    let inFlight = try XCTUnwrap(migrated.item(id: "go#9"))
    XCTAssertEqual(inFlight.state, .uploading)
    XCTAssertEqual(inFlight.taskId, 5)
    XCTAssertEqual(inFlight.resourceType, 9)

    // New code paths work on the migrated rows.
    XCTAssertEqual(migrated.nextCandidate(failedBefore: later, maxAttempts: maxAttempts)?.id, "pe#1")
    migrated.markInterrupted(id: "pe#1")
    XCTAssertEqual(migrated.nextCandidate(failedBefore: later, maxAttempts: maxAttempts)?.id, "fl#1")
    XCTAssertEqual(migrated.resetUploading(exceptItemIds: []), 1)
    XCTAssertEqual(migrated.item(id: "go#9")?.interrupts, 1)
    let counts = migrated.counts()
    XCTAssertEqual(counts.total, 4)
    XCTAssertEqual(counts.uploaded, 1)

    // Reopening does not migrate twice.
    migrated.close()
    XCTAssertEqual(UploadStore(databaseURL: url).userVersion(), 2)
  }

  func testHalfAppliedMigrationIsCompleted() throws {
    let url = dir.url.appendingPathComponent("half/uploader.sqlite")
    try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
    try execRaw(url.path, Self.v1Schema + "ALTER TABLE items ADD COLUMN interrupts INTEGER NOT NULL DEFAULT 0;")
    let migrated = UploadStore(databaseURL: url)
    defer { migrated.close() }
    XCTAssertEqual(migrated.userVersion(), 2)
    XCTAssertEqual(migrated.item(id: "pe#1")?.interrupts, 0)
  }

  func testCorruptDatabaseIsReplaced() throws {
    let url = dir.url.appendingPathComponent("bad/uploader.sqlite")
    try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
    try Data(repeating: 0x42, count: 8192).write(to: url)
    let recovered = UploadStore(databaseURL: url)
    defer { recovered.close() }
    XCTAssertTrue(recovered.isAvailable())
    XCTAssertEqual(recovered.userVersion(), 2)
    recovered.upsert([resource("A", modified: 1)])
    XCTAssertEqual(recovered.counts().total, 1)
    let names = try FileManager.default.contentsOfDirectory(atPath: url.deletingLastPathComponent().path)
    XCTAssertTrue(names.contains { $0.hasPrefix("uploader.sqlite.corrupt-") }, "\(names)")
  }
}
