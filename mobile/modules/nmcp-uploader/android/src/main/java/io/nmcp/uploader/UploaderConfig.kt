package io.nmcp.uploader

import android.content.Context
import android.content.SharedPreferences

/** User settings plus a little run bookkeeping, persisted so background workers can read them. */
class UploaderConfig(context: Context) {
  private val prefs: SharedPreferences =
    context.applicationContext.getSharedPreferences("nmcp_uploader", Context.MODE_PRIVATE)

  var serverUrl: String
    get() = prefs.getString(KEY_SERVER_URL, "") ?: ""
    set(value) = prefs.edit().putString(KEY_SERVER_URL, normalizeServerUrl(value)).apply()

  var autoUpload: Boolean
    get() = prefs.getBoolean(KEY_AUTO, true)
    set(value) = prefs.edit().putBoolean(KEY_AUTO, value).apply()

  var wifiOnly: Boolean
    get() = prefs.getBoolean(KEY_WIFI, true)
    set(value) = prefs.edit().putBoolean(KEY_WIFI, value).apply()

  var chargingOnly: Boolean
    get() = prefs.getBoolean(KEY_CHARGING, false)
    set(value) = prefs.edit().putBoolean(KEY_CHARGING, value).apply()

  var permissionsAsked: Boolean
    get() = prefs.getBoolean(KEY_ASKED, false)
    set(value) = prefs.edit().putBoolean(KEY_ASKED, value).apply()

  var lastScanAt: Long
    get() = prefs.getLong(KEY_LAST_SCAN, 0L)
    set(value) = prefs.edit().putLong(KEY_LAST_SCAN, value).apply()

  var lastRunAt: Long
    get() = prefs.getLong(KEY_LAST_RUN, 0L)
    set(value) = prefs.edit().putLong(KEY_LAST_RUN, value).apply()

  var lastRunError: String?
    get() = prefs.getString(KEY_LAST_ERROR, null)
    set(value) = prefs.edit().putString(KEY_LAST_ERROR, value).apply()

  val isConfigured: Boolean get() = serverUrl.isNotEmpty()

  companion object {
    private const val KEY_SERVER_URL = "server_url"
    private const val KEY_AUTO = "auto_upload"
    private const val KEY_WIFI = "wifi_only"
    private const val KEY_CHARGING = "charging_only"
    private const val KEY_ASKED = "permissions_asked"
    private const val KEY_LAST_SCAN = "last_scan_at"
    private const val KEY_LAST_RUN = "last_run_at"
    private const val KEY_LAST_ERROR = "last_run_error"

    fun normalizeServerUrl(raw: String): String = raw.trim().trimEnd('/')
  }
}
