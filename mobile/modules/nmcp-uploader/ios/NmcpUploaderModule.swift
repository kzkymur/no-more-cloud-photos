import ExpoModulesCore
import Photos
import PhotosUI
import UIKit
import UserNotifications

/// `setConfig` argument; mirrors `UploaderConfig` in NmcpUploader.types.ts.
struct ConfigRecord: Record {
  @Field var serverUrl: String = ""
  @Field var autoUpload: Bool = true
  @Field var wifiOnly: Bool = true
  @Field var chargingOnly: Bool = false
}

/// JS-facing API. Shapes match `NmcpUploaderApi` exactly. No events: the UI
/// polls `getStats`.
public class NmcpUploaderModule: Module {
  public func definition() -> ModuleDefinition {
    Name("NmcpUploader")

    OnCreate {
      UploadEngine.shared.activate()
    }

    AsyncFunction("getConfig") { () -> [String: Any] in
      ConfigStore.load().dictionary
    }

    AsyncFunction("setConfig") { (record: ConfigRecord) in
      let old = ConfigStore.load()
      let new = UploaderConfig(
        serverUrl: UploaderConfig.normalize(serverUrl: record.serverUrl),
        autoUpload: record.autoUpload,
        wifiOnly: record.wifiOnly,
        chargingOnly: record.chargingOnly
      )
      ConfigStore.save(new)
      UploadEngine.shared.configDidChange(from: old, to: new)
      BackgroundScheduler.schedule(new)
      if new.isAutoUploadReady, new != old {
        UploadEngine.shared.requestRun(scan: true, upload: true)
      }
    }

    AsyncFunction("checkServer") { (url: String, promise: Promise) in
      ServerCheck.run(url) { result in
        promise.resolve(result)
      }
    }

    AsyncFunction("getPermissions") { (promise: Promise) in
      Permissions.current { state in
        promise.resolve(state)
      }
    }

    AsyncFunction("requestPermissions") { (promise: Promise) in
      Permissions.request { state in
        promise.resolve(state)
      }
    }

    AsyncFunction("openSettings") { (promise: Promise) in
      MainActor.assumeIsolated {
        guard let url = URL(string: UIApplication.openSettingsURLString) else {
          promise.resolve()
          return
        }
        UIApplication.shared.open(url, options: [:]) { _ in
          promise.resolve()
        }
      }
    }
    .runOnQueue(.main)

    AsyncFunction("extendPartialAccess") { (promise: Promise) in
      MainActor.assumeIsolated {
        switch LibraryAccess.status {
        case .limited:
          guard let controller = topViewController() else {
            promise.reject("ERR_NO_VIEW_CONTROLLER", "Cannot show the photo picker: no visible screen.")
            return
          }
          PHPhotoLibrary.shared().presentLimitedLibraryPicker(from: controller) { _ in
            // The selection changed; rescan (the change observer may also fire).
            let config = ConfigStore.load()
            UploadEngine.shared.requestRun(scan: true, upload: config.isAutoUploadReady)
            promise.resolve()
          }
        case .notDetermined:
          Permissions.request { _ in
            promise.resolve()
          }
        case .denied, .restricted:
          promise.reject("ERR_PHOTO_ACCESS_DENIED",
                         "Photo access is denied. Enable it for this app in Settings > Privacy > Photos.")
        default:
          // Full access: nothing to extend.
          promise.resolve()
        }
      }
    }
    .runOnQueue(.main)

    AsyncFunction("syncNow") {
      UploadEngine.shared.requestRun(scan: true, upload: true)
    }

    AsyncFunction("retryFailed") {
      UploadStore.shared.requeueAllFailed()
      UploadEngine.shared.requestRun(scan: true, upload: true)
    }

    AsyncFunction("cancel") {
      UploadEngine.shared.cancel()
    }

    AsyncFunction("getStats") { () -> [String: Any] in
      let counts = UploadStore.shared.counts()
      let engine = UploadEngine.shared.snapshot()
      return [
        "total": counts.total,
        "uploaded": counts.uploaded,
        "pending": counts.pending,
        "failed": counts.failed,
        "running": engine.running,
        "currentName": engine.currentName.map { $0 as Any } ?? NSNull(),
        "currentSent": Double(engine.currentSent),
        "currentTotal": Double(engine.currentTotal),
        "lastScanAt": RuntimeState.lastScanAt,
        "lastRunAt": RuntimeState.lastRunAt,
        "lastRunError": RuntimeState.lastRunError.map { $0 as Any } ?? NSNull(),
      ]
    }

    AsyncFunction("listFailed") { (limit: Int) -> [[String: Any]] in
      UploadStore.shared.failedItems(limit: limit).map { item in
        [
          "id": item.id,
          "name": item.name,
          "error": item.lastError ?? "Unknown error",
          "attempts": item.attempts,
          "permanent": item.permanent,
        ]
      }
    }
  }
}

// MARK: - checkServer

private enum ServerCheck {
  private static let session: URLSession = {
    let configuration = URLSessionConfiguration.ephemeral
    configuration.timeoutIntervalForRequest = 10
    configuration.timeoutIntervalForResource = 10
    configuration.requestCachePolicy = .reloadIgnoringLocalCacheData
    configuration.waitsForConnectivity = false
    return URLSession(configuration: configuration)
  }()

  /// GET {url}/readyz, Accept: application/json, 10 s timeout. ok = 2xx.
  static func run(_ rawURL: String, completion: @escaping ([String: Any]) -> Void) {
    guard let url = UploaderConfig.endpoint(rawURL, path: "readyz") else {
      completion(ServerCheck.result(ok: false, status: 0,
                        message: "Invalid server URL. Use a full URL such as https://photos.example.ts.net"))
      return
    }
    var request = URLRequest(url: url, cachePolicy: .reloadIgnoringLocalCacheData, timeoutInterval: 10)
    request.httpMethod = "GET"
    request.setValue("application/json", forHTTPHeaderField: "Accept")

    session.dataTask(with: request) { data, response, error in
      if let error {
        let message = UploadEngine.describeTransportError(error as NSError)
        completion(ServerCheck.result(ok: false, status: 0, message: message))
        return
      }
      guard let http = response as? HTTPURLResponse else {
        completion(ServerCheck.result(ok: false, status: 0, message: "The server did not send a valid HTTP response."))
        return
      }
      let excerpt = String(decoding: (data ?? Data()).prefix(512), as: UTF8.self)
        .trimmingCharacters(in: .whitespacesAndNewlines)
      let message = excerpt.isEmpty
        ? HTTPURLResponse.localizedString(forStatusCode: http.statusCode)
        : String(excerpt.prefix(200))
      completion(ServerCheck.result(ok: (200..<300).contains(http.statusCode), status: http.statusCode, message: message))
    }.resume()
  }

  private static func result(ok: Bool, status: Int, message: String) -> [String: Any] {
    ["ok": ok, "status": status, "message": message]
  }
}

// MARK: - Permissions

private enum Permissions {
  static func current(_ completion: @escaping ([String: Any]) -> Void) {
    let library = LibraryAccess.contractValue(LibraryAccess.status)
    UNUserNotificationCenter.current().getNotificationSettings { settings in
      completion([
        "library": library,
        "mediaLocation": true,
        "notifications": Permissions.notificationsAllowed(settings.authorizationStatus),
      ])
    }
  }

  /// Photo library prompt, then (non-fatally) the notification prompt.
  static func request(_ completion: @escaping ([String: Any]) -> Void) {
    PHPhotoLibrary.requestAuthorization(for: .readWrite) { status in
      if status == .authorized || status == .limited {
        LibraryObserver.shared.registerIfAuthorized()
        let config = ConfigStore.load()
        UploadEngine.shared.requestRun(scan: true, upload: config.isAutoUploadReady)
      }
      let center = UNUserNotificationCenter.current()
      center.getNotificationSettings { settings in
        guard settings.authorizationStatus == .notDetermined else {
          Permissions.current(completion)
          return
        }
        center.requestAuthorization(options: [.alert]) { _, _ in
          // Errors are ignored on purpose: notifications are optional.
          Permissions.current(completion)
        }
      }
    }
  }

  private static func notificationsAllowed(_ status: UNAuthorizationStatus) -> Bool {
    switch status {
    case .authorized, .provisional, .ephemeral:
      return true
    default:
      return false
    }
  }
}
