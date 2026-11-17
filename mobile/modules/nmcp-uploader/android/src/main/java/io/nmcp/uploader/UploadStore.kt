package io.nmcp.uploader

import android.content.ContentValues
import android.content.Context
import android.database.sqlite.SQLiteDatabase
import android.database.sqlite.SQLiteOpenHelper

/** One library item (a MediaStore image or video) and its upload state. */
data class UploadItem(
  val id: String,
  val uri: String,
  val name: String,
  val mime: String,
  val size: Long,
  val modified: Long,
  val state: Int,
  val attempts: Int,
  val lastError: String?,
  val permanent: Boolean,
)

data class StoreCounts(val total: Int, val uploaded: Int, val pending: Int, val failed: Int)

/**
 * SQLite ledger of everything the app has seen in the library. The ledger is the
 * source of truth for "already sent": an item in STATE_UPLOADED is never sent again
 * unless the file itself changes (size or modification time).
 */
class UploadStore private constructor(context: Context) :
  SQLiteOpenHelper(context.applicationContext, "nmcp_uploader.db", null, 2) {

  override fun onCreate(db: SQLiteDatabase) {
    db.execSQL(
      """
      CREATE TABLE items (
        id TEXT PRIMARY KEY,
        uri TEXT NOT NULL,
        name TEXT NOT NULL,
        mime TEXT NOT NULL,
        size INTEGER NOT NULL,
        modified INTEGER NOT NULL,
        taken INTEGER NOT NULL DEFAULT 0,
        state INTEGER NOT NULL,
        attempts INTEGER NOT NULL DEFAULT 0,
        last_error TEXT,
        permanent INTEGER NOT NULL DEFAULT 0,
        content_sha TEXT,
        media_id TEXT,
        seen_scan INTEGER NOT NULL DEFAULT 0,
        interrupts INTEGER NOT NULL DEFAULT 0,
        updated_at INTEGER NOT NULL
      )
      """.trimIndent()
    )
    db.execSQL("CREATE INDEX items_state ON items(state, permanent, taken)")
  }

  override fun onUpgrade(db: SQLiteDatabase, oldVersion: Int, newVersion: Int) {
    if (oldVersion < 2) db.execSQL("ALTER TABLE items ADD COLUMN interrupts INTEGER NOT NULL DEFAULT 0")
  }

  /** Snapshot of the scan-relevant columns of every row. */
  fun loadIndex(): Map<String, Triple<Long, Long, Int>> {
    val out = HashMap<String, Triple<Long, Long, Int>>()
    readableDatabase.rawQuery("SELECT id, size, modified, state FROM items", null).use { c ->
      while (c.moveToNext()) out[c.getString(0)] = Triple(c.getLong(1), c.getLong(2), c.getInt(3))
    }
    return out
  }

  /** Applies one library scan in a single transaction. */
  fun applyScan(scanId: Long, rows: List<ScannedMedia>, known: Map<String, Triple<Long, Long, Int>>) {
    val db = writableDatabase
    val now = System.currentTimeMillis()
    db.beginTransaction()
    try {
      for (row in rows) {
        val existing = known[row.id]
        if (existing == null) {
          db.insert("items", null, ContentValues().apply {
            put("id", row.id); put("uri", row.uri); put("name", row.name); put("mime", row.mime)
            put("size", row.size); put("modified", row.modified); put("taken", row.taken)
            put("state", STATE_PENDING); put("seen_scan", scanId); put("updated_at", now)
          })
          continue
        }
        val values = ContentValues().apply {
          put("uri", row.uri); put("name", row.name); put("mime", row.mime); put("seen_scan", scanId)
        }
        val (size, modified, state) = existing
        if (size != row.size || modified != row.modified) {
          // The file was rewritten (edited in place). Its content differs, so the
          // content-derived idempotency key differs and it is sent as a new original.
          values.put("size", row.size); values.put("modified", row.modified); values.put("taken", row.taken)
          if (state != STATE_UPLOADING) {
            values.put("state", STATE_PENDING); values.put("attempts", 0); values.put("permanent", 0)
            values.put("interrupts", 0)
            values.putNull("last_error"); values.put("updated_at", now)
          }
        }
        db.update("items", values, "id = ?", arrayOf(row.id))
      }
      // Rows that vanished from the library and were never uploaded are no longer actionable.
      db.delete("items", "seen_scan <> ? AND state <> ?", arrayOf(scanId.toString(), STATE_UPLOADED.toString()))
      db.setTransactionSuccessful()
    } finally {
      db.endTransaction()
    }
  }

  /**
   * Next item to send. Items interrupted fewer times go first (so one huge file
   * that keeps getting cut off cannot block the queue); then pending before
   * transient failures; then newest first. Failures are retried at most once per run.
   */
  fun nextCandidate(runStartedAt: Long): UploadItem? {
    val sql = """
      SELECT id, uri, name, mime, size, modified, state, attempts, last_error, permanent FROM items
      WHERE state = $STATE_PENDING
         OR (state = $STATE_FAILED AND permanent = 0 AND attempts < $MAX_ATTEMPTS AND updated_at < ?)
      ORDER BY interrupts ASC, state ASC, taken DESC LIMIT 1
    """.trimIndent()
    readableDatabase.rawQuery(sql, arrayOf(runStartedAt.toString())).use { c ->
      if (!c.moveToFirst()) return null
      return UploadItem(
        c.getString(0), c.getString(1), c.getString(2), c.getString(3), c.getLong(4), c.getLong(5),
        c.getInt(6), c.getInt(7), c.getString(8), c.getInt(9) != 0,
      )
    }
  }

  fun markUploading(id: String) = update(id, ContentValues().apply { put("state", STATE_UPLOADING) })

  fun markUploaded(id: String, contentSha: String, mediaId: String?) = update(id, ContentValues().apply {
    put("state", STATE_UPLOADED); put("content_sha", contentSha); put("media_id", mediaId)
    putNull("last_error"); put("permanent", 0)
  })

  fun markFailed(id: String, error: String, permanent: Boolean, contentSha: String?) {
    writableDatabase.execSQL(
      "UPDATE items SET state = ?, attempts = attempts + 1, last_error = ?, permanent = ?, " +
        "content_sha = COALESCE(?, content_sha), updated_at = ? WHERE id = ?",
      arrayOf<Any?>(STATE_FAILED, error, if (permanent) 1 else 0, contentSha, System.currentTimeMillis(), id),
    )
  }

  /** Stopped mid-transfer: back to the queue, behind items interrupted fewer times. */
  fun markInterrupted(id: String) {
    writableDatabase.execSQL(
      "UPDATE items SET state = $STATE_PENDING, interrupts = interrupts + 1, updated_at = ? WHERE id = ?",
      arrayOf<Any?>(System.currentTimeMillis(), id),
    )
  }

  /** Returns an item to the queue without counting an attempt. */
  fun markPending(id: String) = update(id, ContentValues().apply { put("state", STATE_PENDING) })

  /** Items left in STATE_UPLOADING by a killed process go back to the queue. */
  fun recoverInterrupted() {
    writableDatabase.execSQL("UPDATE items SET state = $STATE_PENDING WHERE state = $STATE_UPLOADING")
  }

  fun retryAllFailed() {
    writableDatabase.execSQL(
      "UPDATE items SET state = $STATE_PENDING, attempts = 0, permanent = 0, interrupts = 0 WHERE state = $STATE_FAILED"
    )
  }

  fun counts(): StoreCounts {
    readableDatabase.rawQuery(
      "SELECT COUNT(*), " +
        "SUM(CASE WHEN state = $STATE_UPLOADED THEN 1 ELSE 0 END), " +
        "SUM(CASE WHEN state IN ($STATE_PENDING, $STATE_UPLOADING) THEN 1 ELSE 0 END), " +
        "SUM(CASE WHEN state = $STATE_FAILED THEN 1 ELSE 0 END) FROM items",
      null,
    ).use { c ->
      c.moveToFirst()
      return StoreCounts(c.getInt(0), c.getInt(1), c.getInt(2), c.getInt(3))
    }
  }

  fun failed(limit: Int): List<UploadItem> {
    val out = ArrayList<UploadItem>()
    readableDatabase.rawQuery(
      "SELECT id, uri, name, mime, size, modified, state, attempts, last_error, permanent FROM items " +
        "WHERE state = $STATE_FAILED ORDER BY updated_at DESC LIMIT ?",
      arrayOf(limit.coerceIn(1, 1000).toString()),
    ).use { c ->
      while (c.moveToNext()) out += UploadItem(
        c.getString(0), c.getString(1), c.getString(2), c.getString(3), c.getLong(4), c.getLong(5),
        c.getInt(6), c.getInt(7), c.getString(8), c.getInt(9) != 0,
      )
    }
    return out
  }

  private fun update(id: String, values: ContentValues) {
    values.put("updated_at", System.currentTimeMillis())
    writableDatabase.update("items", values, "id = ?", arrayOf(id))
  }

  companion object {
    const val STATE_PENDING = 0
    const val STATE_UPLOADING = 1
    const val STATE_UPLOADED = 2
    const val STATE_FAILED = 3
    const val MAX_ATTEMPTS = 10

    @Volatile private var instance: UploadStore? = null

    fun get(context: Context): UploadStore =
      instance ?: synchronized(this) { instance ?: UploadStore(context).also { instance = it } }
  }
}
