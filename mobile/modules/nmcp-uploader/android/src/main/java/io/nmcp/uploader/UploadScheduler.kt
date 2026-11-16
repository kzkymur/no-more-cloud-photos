package io.nmcp.uploader

import android.content.Context
import android.os.Build
import android.provider.MediaStore
import androidx.work.BackoffPolicy
import androidx.work.Constraints
import androidx.work.CoroutineWorker
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.ExistingWorkPolicy
import androidx.work.NetworkType
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.WorkerParameters
import java.util.concurrent.TimeUnit

/**
 * Background scheduling:
 *  - "nmcp-periodic": every hour, safety net (also survives reboots).
 *  - "nmcp-trigger":  a content-URI trigger on MediaStore images/videos; when the
 *                     camera adds a file it enqueues "nmcp-auto" and re-arms itself.
 *  - "nmcp-auto":     the actual upload run under the user's Wi-Fi/charging constraints.
 *  - "nmcp-manual":   "Sync now" from the UI; needs any network only.
 */
object UploadScheduler {
  private const val PERIODIC = "nmcp-periodic"
  private const val TRIGGER = "nmcp-trigger"
  private const val AUTO = "nmcp-auto"
  private const val MANUAL = "nmcp-manual"

  fun apply(context: Context) {
    val config = UploaderConfig(context)
    val wm = WorkManager.getInstance(context)
    if (!config.autoUpload || !config.isConfigured) {
      wm.cancelUniqueWork(PERIODIC)
      wm.cancelUniqueWork(TRIGGER)
      wm.cancelUniqueWork(AUTO)
      return
    }
    val periodic = PeriodicWorkRequestBuilder<UploadWorker>(1, TimeUnit.HOURS)
      .setConstraints(autoConstraints(config))
      .setBackoffCriteria(BackoffPolicy.EXPONENTIAL, 5, TimeUnit.MINUTES)
      .build()
    // UPDATE keeps the schedule but applies changed constraints.
    wm.enqueueUniquePeriodicWork(PERIODIC, ExistingPeriodicWorkPolicy.UPDATE, periodic)
    armTrigger(context, ExistingWorkPolicy.REPLACE)
    enqueueAuto(context)
  }

  fun enqueueAuto(context: Context) {
    val config = UploaderConfig(context)
    if (!config.autoUpload || !config.isConfigured) return
    val request = OneTimeWorkRequestBuilder<UploadWorker>()
      .setConstraints(autoConstraints(config))
      .setBackoffCriteria(BackoffPolicy.EXPONENTIAL, 1, TimeUnit.MINUTES)
      .build()
    WorkManager.getInstance(context).enqueueUniqueWork(AUTO, ExistingWorkPolicy.KEEP, request)
  }

  fun syncNow(context: Context) {
    val request = OneTimeWorkRequestBuilder<UploadWorker>()
      .setConstraints(Constraints.Builder().setRequiredNetworkType(NetworkType.CONNECTED).build())
      .setBackoffCriteria(BackoffPolicy.EXPONENTIAL, 1, TimeUnit.MINUTES)
      .build()
    WorkManager.getInstance(context).enqueueUniqueWork(MANUAL, ExistingWorkPolicy.KEEP, request)
  }

  fun cancelAll(context: Context) {
    val wm = WorkManager.getInstance(context)
    wm.cancelUniqueWork(MANUAL)
    wm.cancelUniqueWork(AUTO)
    // Keep the trigger and periodic schedule; only the current run stops.
  }

  internal fun armTrigger(context: Context, policy: ExistingWorkPolicy) {
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
    val constraints = Constraints.Builder()
      .addContentUriTrigger(images, true)
      .addContentUriTrigger(videos, true)
      .setTriggerContentUpdateDelay(10, TimeUnit.SECONDS)
      .setTriggerContentMaxDelay(2, TimeUnit.MINUTES)
      .build()
    val request = OneTimeWorkRequestBuilder<MediaChangeWorker>().setConstraints(constraints).build()
    WorkManager.getInstance(context).enqueueUniqueWork(TRIGGER, policy, request)
  }

  private fun autoConstraints(config: UploaderConfig): Constraints =
    Constraints.Builder()
      .setRequiredNetworkType(if (config.wifiOnly) NetworkType.UNMETERED else NetworkType.CONNECTED)
      .setRequiresCharging(config.chargingOnly)
      .build()
}

/** Fired by the MediaStore content trigger: schedule an upload run and re-arm. */
class MediaChangeWorker(context: Context, params: WorkerParameters) : CoroutineWorker(context, params) {
  override suspend fun doWork(): Result {
    UploadScheduler.enqueueAuto(applicationContext)
    // APPEND_OR_REPLACE queues the next trigger behind this (finishing) one.
    UploadScheduler.armTrigger(applicationContext, ExistingWorkPolicy.APPEND_OR_REPLACE)
    return Result.success()
  }
}
