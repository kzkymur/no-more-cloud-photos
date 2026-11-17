package io.nmcp.uploader

import android.content.Context
import android.content.Intent
import android.net.Uri
import android.provider.Settings
import expo.modules.interfaces.permissions.PermissionsResponseListener
import expo.modules.kotlin.exception.Exceptions
import expo.modules.kotlin.functions.Coroutine
import expo.modules.kotlin.modules.Module
import expo.modules.kotlin.modules.ModuleDefinition
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.withContext
import java.net.HttpURLConnection
import java.net.URL
import kotlin.coroutines.resume

/** JS bridge. Contract: modules/nmcp-uploader/src/NmcpUploader.types.ts */
class NmcpUploaderModule : Module() {
  private val context: Context
    get() = appContext.reactContext?.applicationContext ?: throw Exceptions.ReactContextLost()

  override fun definition() = ModuleDefinition {
    Name("NmcpUploader")

    OnCreate {
      // Re-arm background scheduling every time the app starts (cheap and idempotent).
      appContext.reactContext?.applicationContext?.let { ctx ->
        runCatching { UploadScheduler.apply(ctx) }
      }
    }

    AsyncFunction("getConfig") {
      val c = UploaderConfig(context)
      mapOf("serverUrl" to c.serverUrl, "autoUpload" to c.autoUpload, "wifiOnly" to c.wifiOnly, "chargingOnly" to c.chargingOnly)
    }

    AsyncFunction("setConfig") { config: Map<String, Any?> ->
      val c = UploaderConfig(context)
      (config["serverUrl"] as? String)?.let { c.serverUrl = it }
      (config["autoUpload"] as? Boolean)?.let { c.autoUpload = it }
      (config["wifiOnly"] as? Boolean)?.let { c.wifiOnly = it }
      (config["chargingOnly"] as? Boolean)?.let { c.chargingOnly = it }
      UploadScheduler.apply(context)
    }

    AsyncFunction("checkServer") Coroutine { url: String ->
      withContext(Dispatchers.IO) { checkServer(UploaderConfig.normalizeServerUrl(url)) }
    }

    AsyncFunction("getPermissions") { permissionState() }

    AsyncFunction("requestPermissions") Coroutine { ->
      askPermissions(MediaPermissions.requested())
      UploaderConfig(context).permissionsAsked = true
      UploadScheduler.apply(context)
      permissionState()
    }

    AsyncFunction("extendPartialAccess") Coroutine { ->
      // On Android 14+ re-requesting the media permissions shows the
      // "select more photos / allow all" sheet again.
      askPermissions(MediaPermissions.requested())
      UploadScheduler.apply(context)
    }

    AsyncFunction("openSettings") {
      val intent = Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.fromParts("package", context.packageName, null))
        .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
      context.startActivity(intent)
    }

    AsyncFunction("syncNow") { UploadScheduler.syncNow(context) }

    AsyncFunction("retryFailed") Coroutine { ->
      withContext(Dispatchers.IO) { UploadStore.get(context).retryAllFailed() }
      UploadScheduler.syncNow(context)
    }

    AsyncFunction("cancel") { UploadScheduler.cancelAll(context) }

    AsyncFunction("getStats") Coroutine { ->
      withContext(Dispatchers.IO) {
        val counts = UploadStore.get(context).counts()
        val c = UploaderConfig(context)
        mapOf(
          "total" to counts.total,
          "uploaded" to counts.uploaded,
          "pending" to counts.pending,
          "failed" to counts.failed,
          "running" to LiveStatus.running,
          "currentName" to LiveStatus.currentName,
          "currentSent" to LiveStatus.currentSent.toDouble(),
          "currentTotal" to LiveStatus.currentTotal.toDouble(),
          "lastScanAt" to c.lastScanAt.toDouble(),
          "lastRunAt" to c.lastRunAt.toDouble(),
          "lastRunError" to c.lastRunError,
        )
      }
    }

    AsyncFunction("listFailed") Coroutine { limit: Int ->
      withContext(Dispatchers.IO) {
        UploadStore.get(context).failed(limit).map {
          mapOf(
            "id" to it.id,
            "name" to it.name,
            "error" to (it.lastError ?: ""),
            "attempts" to it.attempts,
            "permanent" to it.permanent,
          )
        }
      }
    }
  }

  private fun permissionState(): Map<String, Any> {
    val asked = UploaderConfig(context).permissionsAsked
    return mapOf(
      "library" to MediaPermissions.library(context, asked),
      "mediaLocation" to MediaPermissions.mediaLocation(context),
      "notifications" to MediaPermissions.notifications(context),
    )
  }

  private suspend fun askPermissions(permissions: Array<String>) {
    val manager = appContext.permissions ?: throw Exceptions.PermissionsModuleNotFound()
    withContext(Dispatchers.Main) {
      suspendCancellableCoroutine { cont ->
        manager.askForPermissions(PermissionsResponseListener { if (cont.isActive) cont.resume(Unit) }, *permissions)
      }
    }
  }

  private fun checkServer(base: String): Map<String, Any> {
    if (!base.startsWith("http://") && !base.startsWith("https://")) {
      return mapOf("ok" to false, "status" to 0, "message" to "URL must start with http:// or https://")
    }
    return try {
      val conn = (URL("$base/readyz").openConnection() as HttpURLConnection).apply {
        connectTimeout = 10_000
        readTimeout = 10_000
        setRequestProperty("Accept", "application/json")
      }
      try {
        val status = conn.responseCode
        val stream = if (status in 200..399) conn.inputStream else conn.errorStream
        val body = stream?.use { it.readBytes().toString(Charsets.UTF_8) } ?: ""
        mapOf("ok" to (status in 200..299), "status" to status, "message" to body.trim().take(200))
      } finally {
        conn.disconnect()
      }
    } catch (e: Exception) {
      mapOf("ok" to false, "status" to 0, "message" to (e.message ?: e.javaClass.simpleName))
    }
  }
}
