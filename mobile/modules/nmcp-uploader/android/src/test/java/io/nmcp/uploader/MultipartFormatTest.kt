package io.nmcp.uploader

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File

/** The Android wire format must match testdata/contract-vectors.json byte for byte (shared with iOS and Go). */
class MultipartFormatTest {
  private val cases = run {
    // Gradle runs unit tests with the module directory (android/) as working directory.
    val file = File("../testdata/contract-vectors.json")
    assertTrue("missing ${file.absolutePath}", file.isFile)
    JSONObject(file.readText(Charsets.UTF_8)).getJSONArray("cases")
  }

  @Test
  fun matchesSharedVectors() {
    assertTrue(cases.length() > 0)
    for (i in 0 until cases.length()) {
      val c = cases.getJSONObject(i)
      val e = c.getJSONObject("expected")
      val name = c.getString("name")
      val label = "case $i ($name)"
      val content = c.getString("contentLatin1").toByteArray(Charsets.ISO_8859_1)
      val sha = MultipartFormat.sha256Hex(content)
      assertEquals(label, e.getString("contentSha256"), sha)
      assertEquals(label, e.getString("idempotencyKey"),
        MultipartFormat.idempotencyKey(c.getString("platform"), c.getString("itemId"), sha, name))
      assertEquals(label, e.getString("filenameFallback"), MultipartFormat.asciiFallback(name))
      assertEquals(label, e.getString("filenameStar"), MultipartFormat.rfc5987(name))
      assertEquals(label, e.getString("contentType"), MultipartFormat.contentType(c.getString("mime")))
      assertEquals(label, e.getString("head"), MultipartFormat.head(c.getString("boundary"), name, c.getString("mime")))
      assertEquals(label, e.getString("tail"), MultipartFormat.tail(c.getString("boundary")))
    }
  }

  @Test
  fun keyIsValidForServer() {
    for (i in 0 until cases.length()) {
      val key = cases.getJSONObject(i).getJSONObject("expected").getString("idempotencyKey")
      // internal/upload.ValidateIdempotencyKey: 1-128 bytes of visible ASCII.
      assertEquals(70, key.length)
      assertTrue(key.all { it in '!'..'~' })
    }
  }
}
