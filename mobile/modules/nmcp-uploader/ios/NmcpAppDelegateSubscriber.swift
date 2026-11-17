import ExpoModulesCore
import Photos
import UIKit

/// App delegate hooks (listed in expo-module.config.json `appDelegateSubscribers`).
///
/// - Registers BGTaskScheduler handlers during launch (required before
///   `didFinishLaunching` returns) and schedules them.
/// - Recreates the background URLSession when iOS relaunches the app for
///   upload events, and returns the completion handler once events are done.
/// - Kicks a scan + upload run whenever the app becomes active.
public class NmcpAppDelegateSubscriber: ExpoAppDelegateSubscriber {
  public func application(
    _ application: UIApplication,
    didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil
  ) -> Bool {
    BackgroundScheduler.registerHandlers()
    DeviceState.shared.startMonitoring()

    // Create the background session right away: when the launch was caused
    // by finished uploads, this is what re-attaches us to them.
    UploadEngine.shared.activate()
    LibraryObserver.shared.registerIfAuthorized()

    let config = ConfigStore.load()
    BackgroundScheduler.schedule(config)
    if config.isAutoUploadReady {
      // No scan here: launch must stay cheap. A foreground launch scans in
      // applicationDidBecomeActive; a BG task launch scans in its handler.
      UploadEngine.shared.requestRun(scan: false, upload: true)
    }
    return true
  }

  public func applicationDidBecomeActive(_ application: UIApplication) {
    // Simulator E2E only (launch argument `-nmcp.e2e.requestPhotoAccess YES`, which
    // real users cannot pass): PhotoKit reports .notDetermined until the app asks,
    // even when access was pre-granted in TCC by `simctl privacy`. Asking resolves
    // it without a prompt. The normal path asks from the UI button.
    if UserDefaults.standard.bool(forKey: "nmcp.e2e.requestPhotoAccess"),
       PHPhotoLibrary.authorizationStatus(for: .readWrite) == .notDetermined {
      PHPhotoLibrary.requestAuthorization(for: .readWrite) { status in
        guard status == .authorized || status == .limited else { return }
        LibraryObserver.shared.registerIfAuthorized()
        let config = ConfigStore.load()
        if config.isAutoUploadReady {
          UploadEngine.shared.requestRun(scan: true, upload: true)
        }
      }
    }
    let config = ConfigStore.load()
    BackgroundScheduler.schedule(config)
    LibraryObserver.shared.registerIfAuthorized()
    if config.isAutoUploadReady {
      UploadEngine.shared.requestRun(scan: true, upload: true)
    }
  }

  public func application(
    _ application: UIApplication,
    handleEventsForBackgroundURLSession identifier: String,
    completionHandler: @escaping () -> Void
  ) {
    // Expo aggregates this handler across all subscribers and waits for each
    // of them, so sessions that are not ours must be acknowledged at once.
    guard identifier == UploadEngine.sessionIdentifier else {
      completionHandler()
      return
    }
    UploadEngine.shared.handleBackgroundEvents(completionHandler)
  }
}
