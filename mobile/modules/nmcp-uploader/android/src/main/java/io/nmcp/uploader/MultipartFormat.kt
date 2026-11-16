package io.nmcp.uploader

import java.security.MessageDigest

/**
 * The upload wire format shared with iOS (Multipart.swift / MultipartFormat.swift).
 * Pure JVM code so it is unit-tested against testdata/contract-vectors.json.
 *
 *   --B\r\n
 *   Content-Disposition: form-data; name="file"; filename="<fallback>"; filename*=UTF-8''<rfc5987>\r\n
 *   Content-Type: <type>\r\n
 *   \r\n
 *   <original bytes>\r\n
 *   --B--\r\n
 */
object MultipartFormat {
  const val PLATFORM = "android"

  /**
   * "nmcp1-" + hex(SHA-256("<platform>|<itemId>|<contentSha256>|<filename>")): 70 ASCII bytes.
   * Stable for the same item, content and name, so a retry is replayed by the
   * server. A changed content or name yields a new key, so the server answers
   * 201 or 409 duplicate_media instead of 409 idempotency_conflict.
   */
  fun idempotencyKey(platform: String, itemId: String, contentSha256: String, filename: String): String =
    "nmcp1-" + sha256Hex("$platform|$itemId|$contentSha256|$filename".toByteArray(Charsets.UTF_8))

  fun head(boundary: String, name: String, mime: String): String =
    "--$boundary\r\n" +
      "Content-Disposition: form-data; name=\"file\"; filename=\"${asciiFallback(name)}\"; filename*=UTF-8''${rfc5987(name)}\r\n" +
      "Content-Type: ${contentType(mime)}\r\n\r\n"

  fun tail(boundary: String): String = "\r\n--$boundary--\r\n"

  /** One '_' per UTF-16 unit that is not printable ASCII, or is '"' or '\'. */
  fun asciiFallback(name: String): String = buildString {
    for (ch in name) append(if (ch.code in 0x20..0x7e && ch != '"' && ch != '\\') ch else '_')
  }

  /** RFC 8187 value-chars: attr-char kept, every other UTF-8 byte is %XX (uppercase). */
  fun rfc5987(name: String): String {
    val sb = StringBuilder()
    for (b in name.toByteArray(Charsets.UTF_8)) {
      val v = b.toInt() and 0xff
      if (v < 0x80 && v.toChar() in ATTR_CHARS) {
        sb.append(v.toChar())
      } else {
        sb.append('%').append(UPPER[v ushr 4]).append(UPPER[v and 0x0f])
      }
    }
    return sb.toString()
  }

  /** A syntactically valid, non-multipart media type, else application/octet-stream. */
  fun contentType(mime: String): String {
    val m = mime.trim()
    return if (MIME.matches(m) && !m.startsWith("multipart/", ignoreCase = true)) m else "application/octet-stream"
  }

  fun sha256Hex(bytes: ByteArray): String = hex(MessageDigest.getInstance("SHA-256").digest(bytes))

  fun hex(bytes: ByteArray): String {
    val sb = StringBuilder(bytes.size * 2)
    for (b in bytes) {
      val v = b.toInt() and 0xff
      sb.append(LOWER[v ushr 4]).append(LOWER[v and 0x0f])
    }
    return sb.toString()
  }

  private const val LOWER = "0123456789abcdef"
  private const val UPPER = "0123456789ABCDEF"
  private const val ATTR_CHARS =
    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789!#$&+-.^_`|~"
  private val MIME = Regex("^[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]*/[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]*$")
}
