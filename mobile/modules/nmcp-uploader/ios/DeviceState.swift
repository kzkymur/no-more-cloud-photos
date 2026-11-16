import Foundation
import UIKit

/// Runs `block` on the main thread (synchronously when already there).
func runOnMain(_ block: @escaping @MainActor () -> Void) {
  if Thread.isMainThread {
    MainActor.assumeIsolated(block)
  } else {
    DispatchQueue.main.async {
      MainActor.assumeIsolated(block)
    }
  }
}

/// Caches the battery state so background queues can check "charging only"
/// without touching UIKit off the main thread.
final class DeviceState: @unchecked Sendable {
  static let shared = DeviceState()

  private let lock = NSLock()
  private var batteryState: UIDevice.BatteryState = .unknown
  private var started = false // main thread only
  private var observer: NSObjectProtocol? // main thread only

  /// Must be called early (from `didFinishLaunching`) so the cached value is
  /// valid for background launches.
  func startMonitoring() {
    runOnMain { [self] in
      guard !started else { return }
      started = true
      UIDevice.current.isBatteryMonitoringEnabled = true
      store(UIDevice.current.batteryState)
      observer = NotificationCenter.default.addObserver(
        forName: UIDevice.batteryStateDidChangeNotification,
        object: nil,
        queue: .main
      ) { [weak self] _ in
        MainActor.assumeIsolated {
          self?.batteryChanged(UIDevice.current.batteryState)
        }
      }
    }
  }

  /// `.unknown` (monitoring unavailable, e.g. Simulator) counts as not charging.
  var isCharging: Bool {
    lock.locked {
      batteryState == .charging || batteryState == .full
    }
  }

  private func store(_ state: UIDevice.BatteryState) {
    lock.locked {
      batteryState = state
    }
  }

  private func batteryChanged(_ state: UIDevice.BatteryState) {
    let wasCharging = isCharging
    store(state)
    // Plugging in resumes a run that was paused by "charging only".
    if !wasCharging, isCharging {
      let config = ConfigStore.load()
      if config.chargingOnly, config.isAutoUploadReady {
        UploadEngine.shared.requestRun(scan: false, upload: true)
      }
    }
  }
}

/// Wraps `UIApplication.beginBackgroundTask` so staging an item can finish
/// when the app is moved to / woken in the background (~30 s budget).
final class BackgroundActivity: @unchecked Sendable {
  private var identifier: UIBackgroundTaskIdentifier = .invalid // main thread only
  private var ended = false // main thread only

  static func begin(_ name: String, onExpiration: (@Sendable () -> Void)? = nil) -> BackgroundActivity {
    let activity = BackgroundActivity()
    runOnMain {
      guard !activity.ended else { return }
      activity.identifier = UIApplication.shared.beginBackgroundTask(withName: name) {
        onExpiration?()
        MainActor.assumeIsolated {
          activity.finish()
        }
      }
    }
    return activity
  }

  func end() {
    runOnMain { [self] in
      finish()
    }
  }

  @MainActor
  private func finish() {
    ended = true
    if identifier != .invalid {
      UIApplication.shared.endBackgroundTask(identifier)
      identifier = .invalid
    }
  }
}

/// Finds the view controller to present system UI from.
@MainActor
func topViewController() -> UIViewController? {
  let scenes = UIApplication.shared.connectedScenes.compactMap { $0 as? UIWindowScene }
  let activeScenes = scenes.filter { $0.activationState == .foregroundActive }
  let candidates = activeScenes.isEmpty ? scenes : activeScenes
  let windows = candidates.flatMap { $0.windows }
  let window = windows.first(where: { $0.isKeyWindow }) ?? windows.first
  var top = window?.rootViewController
  while let presented = top?.presentedViewController, !presented.isBeingDismissed {
    top = presented
  }
  return top
}
