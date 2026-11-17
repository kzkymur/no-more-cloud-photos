import BackgroundTasks
import Foundation
import os

private let bgLog = Logger(subsystem: "io.nmcp.uploader", category: "bgtask")

/// BGTaskScheduler integration. Both tasks do the same thing: scan the
/// library and hand the next item to the background URLSession. The upload
/// itself then continues out of process in nsurlsessiond, and each finished
/// upload relaunches the app to chain the next one.
enum BackgroundScheduler {
  static let refreshIdentifier = "io.nmcp.uploader.refresh"
  static let processingIdentifier = "io.nmcp.uploader.processing"
  private static let refreshInterval: TimeInterval = 15 * 60

  private static let lock = NSLock()
  private static var registered = Set<String>()
  private static var didAttemptRegistration = false

  /// Must run before `application(_:didFinishLaunchingWithOptions:)` returns.
  /// Identifiers missing from `BGTaskSchedulerPermittedIdentifiers` are
  /// skipped: registering them would raise an exception and crash the app.
  static func registerHandlers() {
    let shouldRegister = lock.locked { () -> Bool in
      if didAttemptRegistration {
        return false
      }
      didAttemptRegistration = true
      return true
    }
    guard shouldRegister else { return }

    let permitted = Bundle.main.object(forInfoDictionaryKey: "BGTaskSchedulerPermittedIdentifiers") as? [String] ?? []
    for identifier in [refreshIdentifier, processingIdentifier] {
      guard permitted.contains(identifier) else {
        bgLog.error("\(identifier, privacy: .public) missing from BGTaskSchedulerPermittedIdentifiers; background task disabled")
        continue
      }
      let ok = BGTaskScheduler.shared.register(forTaskWithIdentifier: identifier, using: nil) { task in
        BackgroundScheduler.handle(task)
      }
      if ok {
        lock.locked { _ = registered.insert(identifier) }
      } else {
        bgLog.error("BGTaskScheduler.register failed for \(identifier, privacy: .public)")
      }
    }
  }

  /// Submits (replaces) both requests, or cancels them when automatic upload
  /// is off / not configured.
  static func schedule(_ config: UploaderConfig) {
    let ids = lock.locked { registered }
    guard config.isAutoUploadReady else {
      for identifier in ids {
        BGTaskScheduler.shared.cancel(taskRequestWithIdentifier: identifier)
      }
      return
    }

    if ids.contains(refreshIdentifier) {
      let request = BGAppRefreshTaskRequest(identifier: refreshIdentifier)
      request.earliestBeginDate = Date(timeIntervalSinceNow: refreshInterval)
      submit(request)
    }
    if ids.contains(processingIdentifier) {
      let request = BGProcessingTaskRequest(identifier: processingIdentifier)
      request.requiresNetworkConnectivity = true
      request.requiresExternalPower = config.chargingOnly
      request.earliestBeginDate = Date(timeIntervalSinceNow: refreshInterval)
      submit(request)
    }
  }

  private static func submit(_ request: BGTaskRequest) {
    do {
      try BGTaskScheduler.shared.submit(request)
    } catch {
      // BGTaskScheduler is unavailable on the Simulator and when Background
      // App Refresh is disabled; neither is fatal.
      bgLog.error("Cannot schedule \(request.identifier, privacy: .public): \(error.localizedDescription, privacy: .public)")
    }
  }

  private static func handle(_ task: BGTask) {
    let config = ConfigStore.load()
    // Always queue the next occurrence first; a task is one-shot.
    schedule(config)

    let completion = TaskCompletion(task)
    task.expirationHandler = {
      UploadEngine.shared.backgroundTimeExpired()
      completion.finish(success: false)
    }

    guard config.isAutoUploadReady else {
      completion.finish(success: true)
      return
    }
    // `onSettled` fires once the scan is done and the next upload has been
    // handed to the background session (or the queue is empty).
    UploadEngine.shared.requestRun(scan: true, upload: true) {
      completion.finish(success: true)
    }
  }
}

/// Guarantees `setTaskCompleted` is called exactly once.
private final class TaskCompletion: @unchecked Sendable {
  private let task: BGTask
  private let lock = NSLock()
  private var done = false

  init(_ task: BGTask) {
    self.task = task
  }

  func finish(success: Bool) {
    let first = lock.locked { () -> Bool in
      if done {
        return false
      }
      done = true
      return true
    }
    if first {
      task.setTaskCompleted(success: success)
    }
  }
}
