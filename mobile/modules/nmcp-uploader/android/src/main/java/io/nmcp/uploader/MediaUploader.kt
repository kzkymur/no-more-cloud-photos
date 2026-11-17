package io.nmcp.uploader

import android.content.Context
import android.net.Uri
import android.os.Build
import android.provider.MediaStore
import org.json.JSONObject
import java.io.IOException
import java.io.InputStream
import java.net.HttpURLConnection
import java.net.URL
import java.security.MessageDigest
import java.security.SecureRandom

/** Result of one POST /media attempt. */
sealed class UploadOutcome {
  data class Uploaded(val contentSha: String, val mediaId: String?, val replayed: Boolean) : UploadOutcome()

  /** The server answered and will keep answering the same way: no automatic retry. */
  data class Rejected(val contentSha: String?, val message: String) : UploadOutcome()

  /** Worth retrying later (timeouts, 5xx, 429). */
  data class Transient(val contentSha: String?, val message: String) : UploadOutcome()

  /** The server could not be reached at all; the whole run should stop and back off. */
  data class Unreachable(val message: String) : UploadOutcome()

  /**
   * Nothing will work until the user changes something (server URL, permission):
   * stop the run without backoff retries; the next trigger or the UI starts again.
   */
  data class NeedsUser(val message: String) : UploadOutcome()
}

/**
 * Sends one original to the Core API. The bytes are streamed unmodified from
 * MediaStore through MediaStore.setRequireOriginal (so GPS EXIF is never
 * redacted) inside the single-part body described in [MultipartFormat].
 */
class MediaUploader(private val context: Context, private val serverUrl: String) {

  fun interface Progress {
    /** Called while streaming. Throw (e.g. CancellationException) to abort. */
    fun onProgress(sent: Long, total: Long)
  }

  fun upload(item: UploadItem, progress: Progress): UploadOutcome {
    // Never send location-redacted bytes: they would be stored as the "original"
    // and the ledger would never send the real one.
    val source = sourceUri(item) ?: return UploadOutcome.NeedsUser(LOCATION_REQUIRED)

    // Pass 1: exact length and content hash of the bytes we are going to send.
    val (contentSha, length) = try {
      hashOf(source) { progress.onProgress(0, item.size) }
    } catch (e: SecurityException) {
      return UploadOutcome.Rejected(null, "No permission to read this item: ${e.message}")
    } catch (e: IOException) {
      return UploadOutcome.Transient(null, "Could not read file: ${e.message}")
    }
    if (length == 0L) return UploadOutcome.Rejected(contentSha, "File is empty")

    val key = MultipartFormat.idempotencyKey(MultipartFormat.PLATFORM, item.id, contentSha, item.name)
    val boundary = "nmcp-" + randomHex(16)
    val head = MultipartFormat.head(boundary, item.name, item.mime).toByteArray(Charsets.UTF_8)
    val tail = MultipartFormat.tail(boundary).toByteArray(Charsets.US_ASCII)
    val total = head.size + length + tail.size

    val connection: HttpURLConnection
    try {
      connection = (URL("$serverUrl/media").openConnection() as HttpURLConnection).apply {
        requestMethod = "POST"
        doOutput = true
        connectTimeout = 20_000
        // The server answers after it has durably stored the original; large videos take a while.
        readTimeout = 300_000
        setFixedLengthStreamingMode(total)
        setRequestProperty("Content-Type", "multipart/form-data; boundary=$boundary")
        setRequestProperty("Accept", "application/json")
        setRequestProperty("Idempotency-Key", key)
      }
    } catch (e: IOException) {
      return UploadOutcome.NeedsUser("Invalid server URL: ${e.message}")
    } catch (e: IllegalArgumentException) {
      return UploadOutcome.NeedsUser("Invalid server URL: ${e.message}")
    }

    try {
      try {
        connection.connect()
      } catch (e: IOException) {
        return UploadOutcome.Unreachable("Server unreachable: ${e.message ?: e.javaClass.simpleName}")
      }
      try {
        connection.outputStream.use { out ->
          out.write(head)
          var sent = 0L
          val digest = MessageDigest.getInstance("SHA-256")
          open(source).use { input ->
            val buffer = ByteArray(BUFFER)
            var lastReport = 0L
            while (true) {
              val n = input.read(buffer)
              if (n < 0) break
              if (sent + n > length) throw IOException("File grew while uploading")
              digest.update(buffer, 0, n)
              out.write(buffer, 0, n)
              sent += n
              if (sent - lastReport >= REPORT_EVERY) {
                lastReport = sent
                progress.onProgress(sent, length)
              }
            }
          }
          if (sent != length || hex(digest.digest()) != contentSha) {
            throw IOException("File changed while uploading")
          }
          out.write(tail)
          progress.onProgress(length, length)
        }
      } catch (e: IOException) {
        // The server may still have answered (e.g. 413 before reading everything).
        val early = runCatching { classify(connection, contentSha) }.getOrNull()
        return early ?: UploadOutcome.Transient(contentSha, "Upload interrupted: ${e.message}")
      }
      return try {
        classify(connection, contentSha)
      } catch (e: IOException) {
        UploadOutcome.Transient(contentSha, "No response from server: ${e.message}")
      }
    } finally {
      connection.disconnect()
    }
  }

  private fun classify(connection: HttpURLConnection, contentSha: String): UploadOutcome {
    val status = connection.responseCode
    val stream = if (status in 200..399) connection.inputStream else connection.errorStream
    val body = stream?.use { it.readBytes().toString(Charsets.UTF_8) } ?: ""
    val json = runCatching { JSONObject(body) }.getOrNull()
    if (status in 200..299) {
      val mediaId = json?.optJSONObject("media")?.optString("id")?.takeIf { it.isNotEmpty() }
      val replayed = connection.getHeaderField("Idempotency-Replayed") == "true"
      return UploadOutcome.Uploaded(contentSha, mediaId, replayed)
    }
    val error = json?.optJSONObject("error")
    val code = error?.optString("code").orEmpty()
    val message = error?.optString("message").orEmpty().ifEmpty { body.take(200) }
    if (status == 409 && code == "duplicate_media") {
      // Same bytes are already on the server (sent earlier, maybe from another device): done.
      val existing = error?.optJSONObject("details")?.optString("existing_media_id")?.takeIf { it.isNotEmpty() }
      return UploadOutcome.Uploaded(contentSha, existing, true)
    }
    val text = "HTTP $status${if (code.isNotEmpty()) " $code" else ""}: $message"
    return when {
      // Not about this file: wrong URL / proxy in front of the API. Stop instead of failing every item.
      status == 401 || status == 403 || status == 404 || status == 405 ->
        UploadOutcome.NeedsUser("Check the server URL ($text)")
      // Server busy or down: stop the run so queued items do not burn attempts.
      status == 429 || status == 502 || status == 503 || status == 504 ->
        UploadOutcome.Unreachable("Server busy or unavailable ($text)")
      status == 408 || status >= 500 -> UploadOutcome.Transient(contentSha, text)
      else -> UploadOutcome.Rejected(contentSha, text)
    }
  }

  /** Unredacted URI, or null when Android would redact location (no ACCESS_MEDIA_LOCATION). */
  private fun sourceUri(item: UploadItem): Uri? {
    val uri = Uri.parse(item.uri)
    if (Build.VERSION.SDK_INT < Build.VERSION_CODES.Q) return uri // no redaction before Android 10
    if (!MediaPermissions.mediaLocation(context)) return null
    return MediaStore.setRequireOriginal(uri)
  }

  private fun open(uri: Uri): InputStream =
    context.contentResolver.openInputStream(uri) ?: throw IOException("MediaStore returned no stream")

  private fun hashOf(uri: Uri, tick: () -> Unit): Pair<String, Long> {
    val digest = MessageDigest.getInstance("SHA-256")
    var length = 0L
    open(uri).use { input ->
      val buffer = ByteArray(BUFFER)
      var sinceTick = 0L
      while (true) {
        val n = input.read(buffer)
        if (n < 0) break
        digest.update(buffer, 0, n)
        length += n
        sinceTick += n
        if (sinceTick >= REPORT_EVERY) { sinceTick = 0; tick() }
      }
    }
    return hex(digest.digest()) to length
  }

  companion object {
    const val LOCATION_REQUIRED =
      "Uploads paused: allow access to photo locations, otherwise Android removes GPS data from the originals"
    private const val BUFFER = 256 * 1024
    private const val REPORT_EVERY = 1L shl 20
    private val random = SecureRandom()

    private fun hex(bytes: ByteArray): String = MultipartFormat.hex(bytes)

    private fun randomHex(bytes: Int): String = ByteArray(bytes).also { random.nextBytes(it) }.let(::hex)
  }
}
