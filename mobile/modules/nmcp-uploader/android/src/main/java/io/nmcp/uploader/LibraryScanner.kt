package io.nmcp.uploader

import android.content.ContentUris
import android.content.Context
import android.net.Uri
import android.os.Build
import android.provider.MediaStore

data class ScannedMedia(
  val id: String,
  val uri: String,
  val name: String,
  val mime: String,
  val size: Long,
  val modified: Long,
  val taken: Long,
)

/** Enumerates every image and video visible to the app through MediaStore. */
object LibraryScanner {
  fun scan(context: Context): List<ScannedMedia> {
    val out = ArrayList<ScannedMedia>()
    val images = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
      MediaStore.Images.Media.getContentUri(MediaStore.VOLUME_EXTERNAL)
    } else {
      MediaStore.Images.Media.EXTERNAL_CONTENT_URI
    }
    val videos = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
      MediaStore.Video.Media.getContentUri(MediaStore.VOLUME_EXTERNAL)
    } else {
      MediaStore.Video.Media.EXTERNAL_CONTENT_URI
    }
    query(context, images, "img", out)
    query(context, videos, "vid", out)
    return out
  }

  private fun query(context: Context, collection: Uri, prefix: String, out: MutableList<ScannedMedia>) {
    val projection = arrayOf(
      MediaStore.MediaColumns._ID,
      MediaStore.MediaColumns.DISPLAY_NAME,
      MediaStore.MediaColumns.MIME_TYPE,
      MediaStore.MediaColumns.SIZE,
      MediaStore.MediaColumns.DATE_MODIFIED,
      MediaStore.MediaColumns.DATE_ADDED,
    )
    // Files still being written by the camera are skipped until they are published.
    val selection = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) "${MediaStore.MediaColumns.IS_PENDING} = 0" else null
    context.contentResolver.query(collection, projection, selection, null, null)?.use { c ->
      val idCol = c.getColumnIndexOrThrow(MediaStore.MediaColumns._ID)
      val nameCol = c.getColumnIndexOrThrow(MediaStore.MediaColumns.DISPLAY_NAME)
      val mimeCol = c.getColumnIndexOrThrow(MediaStore.MediaColumns.MIME_TYPE)
      val sizeCol = c.getColumnIndexOrThrow(MediaStore.MediaColumns.SIZE)
      val modCol = c.getColumnIndexOrThrow(MediaStore.MediaColumns.DATE_MODIFIED)
      val addedCol = c.getColumnIndexOrThrow(MediaStore.MediaColumns.DATE_ADDED)
      while (c.moveToNext()) {
        val id = c.getLong(idCol)
        val size = c.getLong(sizeCol)
        if (size <= 0L) continue
        out += ScannedMedia(
          id = "$prefix:$id",
          uri = ContentUris.withAppendedId(collection, id).toString(),
          name = c.getString(nameCol) ?: "$prefix-$id",
          // Unknown type: let the server sniff the bytes rather than guessing.
          mime = c.getString(mimeCol) ?: "application/octet-stream",
          size = size,
          modified = c.getLong(modCol),
          taken = c.getLong(addedCol),
        )
      }
    }
  }
}
