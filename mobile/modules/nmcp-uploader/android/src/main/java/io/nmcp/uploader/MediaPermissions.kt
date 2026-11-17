package io.nmcp.uploader

import android.Manifest
import android.content.Context
import android.content.pm.PackageManager
import android.os.Build
import androidx.core.app.NotificationManagerCompat
import androidx.core.content.ContextCompat

/** Maps Android's per-version media permissions onto the app's simple access model. */
object MediaPermissions {
  const val FULL = "full"
  const val PARTIAL = "partial"
  const val DENIED = "denied"
  const val UNDETERMINED = "undetermined"

  /** Everything the app asks for in one system dialog sequence. */
  fun requested(): Array<String> {
    val list = ArrayList<String>()
    if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
      list += Manifest.permission.READ_MEDIA_IMAGES
      list += Manifest.permission.READ_MEDIA_VIDEO
      if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
        list += Manifest.permission.READ_MEDIA_VISUAL_USER_SELECTED
      }
      list += Manifest.permission.POST_NOTIFICATIONS
    } else {
      list += Manifest.permission.READ_EXTERNAL_STORAGE
    }
    if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
      list += Manifest.permission.ACCESS_MEDIA_LOCATION
    }
    return list.toTypedArray()
  }

  fun library(context: Context, asked: Boolean): String {
    val full = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
      granted(context, Manifest.permission.READ_MEDIA_IMAGES) && granted(context, Manifest.permission.READ_MEDIA_VIDEO)
    } else {
      granted(context, Manifest.permission.READ_EXTERNAL_STORAGE)
    }
    if (full) return FULL
    if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU &&
      (granted(context, Manifest.permission.READ_MEDIA_IMAGES) || granted(context, Manifest.permission.READ_MEDIA_VIDEO))
    ) {
      return PARTIAL
    }
    if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE &&
      granted(context, Manifest.permission.READ_MEDIA_VISUAL_USER_SELECTED)
    ) {
      return PARTIAL
    }
    return if (asked) DENIED else UNDETERMINED
  }

  /** Without ACCESS_MEDIA_LOCATION MediaStore redacts GPS from the bytes it returns. */
  fun mediaLocation(context: Context): Boolean =
    Build.VERSION.SDK_INT < Build.VERSION_CODES.Q || granted(context, Manifest.permission.ACCESS_MEDIA_LOCATION)

  fun notifications(context: Context): Boolean = NotificationManagerCompat.from(context).areNotificationsEnabled()

  fun canRead(context: Context): Boolean = library(context, true).let { it == FULL || it == PARTIAL }

  private fun granted(context: Context, permission: String) =
    ContextCompat.checkSelfPermission(context, permission) == PackageManager.PERMISSION_GRANTED
}
