package io.nmcp.uploader

import android.app.NotificationChannel
import android.app.NotificationManager
import android.content.Context
import android.content.pm.ServiceInfo
import android.os.Build
import android.util.Log
import androidx.core.app.NotificationCompat
import androidx.work.CoroutineWorker
import androidx.work.ForegroundInfo
import androidx.work.WorkerParameters
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import kotlin.coroutines.coroutineContext

/** In-process live state shown by the UI (the DB holds the durable part). */
object LiveStatus {
  @Volatile var running = false
  @Volatile var currentName: String? = null
  @Volatile var currentSent = 0L
  @Volatile var currentTotal = 0L
}

/**
 * Scans the library and uploads everything that is not on the server yet, one
 * item at a time. Runs are serialized process-wide; a second run that starts
 * while one is active waits and then finds nothing (or only new items) to do.
 */
class UploadWorker(context: Context, params: WorkerParameters) : CoroutineWorker(context, params) {

  override suspend fun doWork(): Result = withContext(Dispatchers.IO) {
    runLock.withLock { runOnce() }
  }

  private suspend fun runOnce(): Result {
    val config = UploaderConfig(applicationContext)
    val store = UploadStore.get(applicationContext)
    if (!config.isConfigured) return Result.success()
    if (!MediaPermissions.canRead(applicationContext)) {
      config.lastRunError = "Photo library permission is missing"
      return Result.success()
    }

    LiveStatus.running = true
    tryForeground("Checking library…", 0, 0)
    val runStartedAt = System.currentTimeMillis()
    var stopError: String? = null
    try {
      store.recoverInterrupted()
      scan(applicationContext)
      val uploader = MediaUploader(applicationContext, config.serverUrl)
      var done = 0
      var rescans = 0
      while (true) {
        coroutineContext.ensureActive()
        var next = store.nextCandidate(runStartedAt)
        if (next == null && done > 0 && rescans < MAX_RESCANS) {
          // Photos taken while this run was busy: pick them up before finishing.
          rescans++
          scan(applicationContext)
          next = store.nextCandidate(runStartedAt)
        }
        val item = next ?: break
        store.markUploading(item.id)
        LiveStatus.currentName = item.name
        LiveStatus.currentSent = 0
        LiveStatus.currentTotal = item.size
        val remaining = store.counts().pending
        tryForeground("Uploading ${item.name}", done, done + remaining)

        val outcome = try {
          var lastNotify = 0L
          uploader.upload(item) { sent, total ->
            if (isStopped) throw CancellationException("stopped")
            LiveStatus.currentSent = sent
            LiveStatus.currentTotal = total
            val now = System.currentTimeMillis()
            if (now - lastNotify > 2_000) {
              lastNotify = now
              notifyProgress("Uploading ${item.name}", done, done + remaining, sent, total)
            }
          }
        } catch (e: CancellationException) {
          store.markPending(item.id)
          throw e
        } catch (e: Exception) {
          UploadOutcome.Transient(null, "Unexpected error: ${e.javaClass.simpleName}: ${e.message}")
        }

        when (outcome) {
          is UploadOutcome.Uploaded -> { store.markUploaded(item.id, outcome.contentSha, outcome.mediaId); done++ }
          is UploadOutcome.Rejected -> store.markFailed(item.id, outcome.message, true, outcome.contentSha)
          is UploadOutcome.Transient -> store.markFailed(item.id, outcome.message, false, outcome.contentSha)
          is UploadOutcome.Unreachable -> {
            store.markPending(item.id)
            stopError = outcome.message
            break
          }
        }
      }
    } catch (e: CancellationException) {
      throw e
    } catch (e: Exception) {
      Log.e(TAG, "upload run failed", e)
      stopError = "${e.javaClass.simpleName}: ${e.message}"
    } finally {
      LiveStatus.running = false
      LiveStatus.currentName = null
      LiveStatus.currentSent = 0
      LiveStatus.currentTotal = 0
      config.lastRunAt = System.currentTimeMillis()
      config.lastRunError = stopError
      cancelNotification()
    }
    // Server unreachable: let WorkManager back off and try again.
    return if (stopError != null) Result.retry() else Result.success()
  }

  private suspend fun tryForeground(text: String, done: Int, total: Int) {
    try {
      setForeground(foregroundInfo(text, done, total, 0, 0))
    } catch (e: Exception) {
      // Android 12+ may refuse to start a foreground service from the background;
      // the work still runs, just without the guarantee.
      Log.w(TAG, "setForeground refused: ${e.message}")
    }
  }

  private fun notifyProgress(text: String, done: Int, total: Int, sent: Long, size: Long) {
    if (!MediaPermissions.notifications(applicationContext)) return
    val nm = applicationContext.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
    runCatching { nm.notify(NOTIFICATION_ID, buildNotification(text, done, total, sent, size)) }
  }

  private fun cancelNotification() {
    val nm = applicationContext.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
    nm.cancel(NOTIFICATION_ID)
  }

  private fun foregroundInfo(text: String, done: Int, total: Int, sent: Long, size: Long): ForegroundInfo {
    val notification = buildNotification(text, done, total, sent, size)
    return if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
      ForegroundInfo(NOTIFICATION_ID, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC)
    } else {
      ForegroundInfo(NOTIFICATION_ID, notification)
    }
  }

  private fun buildNotification(text: String, done: Int, total: Int, sent: Long, size: Long): android.app.Notification {
    ensureChannel(applicationContext)
    val launch = applicationContext.packageManager.getLaunchIntentForPackage(applicationContext.packageName)
    val pending = launch?.let {
      android.app.PendingIntent.getActivity(
        applicationContext, 0, it,
        android.app.PendingIntent.FLAG_IMMUTABLE or android.app.PendingIntent.FLAG_UPDATE_CURRENT,
      )
    }
    val builder = NotificationCompat.Builder(applicationContext, CHANNEL_ID)
      .setSmallIcon(android.R.drawable.stat_sys_upload)
      .setContentTitle(if (total > 0) "Uploading photos ($done/$total)" else "Photo upload")
      .setContentText(text)
      .setOnlyAlertOnce(true)
      .setOngoing(true)
      .setSilent(true)
      .setContentIntent(pending)
    if (size > 0) {
      builder.setProgress(1000, ((sent * 1000) / size).toInt(), false)
    } else {
      builder.setProgress(0, 0, true)
    }
    return builder.build()
  }

  companion object {
    private const val TAG = "NmcpUploader"
    private const val CHANNEL_ID = "nmcp_upload"
    private const val NOTIFICATION_ID = 41021
    private const val MAX_RESCANS = 5
    private val runLock = Mutex()

    /** Rescans MediaStore into the ledger. Safe to call outside a run. */
    fun scan(context: Context) {
      val store = UploadStore.get(context)
      val known = store.loadIndex()
      val scanId = System.currentTimeMillis()
      store.applyScan(scanId, LibraryScanner.scan(context), known)
      UploaderConfig(context).lastScanAt = scanId
    }

    private fun ensureChannel(context: Context) {
      if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) return
      val nm = context.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
      if (nm.getNotificationChannel(CHANNEL_ID) == null) {
        nm.createNotificationChannel(
          NotificationChannel(CHANNEL_ID, "Photo upload", NotificationManager.IMPORTANCE_LOW)
        )
      }
    }
  }
}
