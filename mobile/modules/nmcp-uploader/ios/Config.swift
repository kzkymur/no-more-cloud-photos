import Foundation

/// User-facing uploader settings. Field names mirror `UploaderConfig` in
/// `src/NmcpUploader.types.ts`.
struct UploaderConfig: Equatable {
  var serverUrl: String
  var autoUpload: Bool
  var wifiOnly: Bool
  var chargingOnly: Bool

  static let defaults = UploaderConfig(serverUrl: "", autoUpload: true, wifiOnly: true, chargingOnly: false)

  /// True when automatic (unattended) uploads may run.
  var isAutoUploadReady: Bool {
    autoUpload && baseURL != nil
  }

  /// Parsed server base URL, nil when empty or not an absolute http(s) URL.
  var baseURL: URL? {
    UploaderConfig.parseBaseURL(serverUrl)
  }

  /// `{serverUrl}/media`
  var mediaEndpoint: URL? {
    UploaderConfig.endpoint(serverUrl, path: "media")
  }

  var dictionary: [String: Any] {
    [
      "serverUrl": serverUrl,
      "autoUpload": autoUpload,
      "wifiOnly": wifiOnly,
      "chargingOnly": chargingOnly,
    ]
  }

  /// Trims whitespace and trailing slashes so `"{base}/media"` never contains `//`.
  static func normalize(serverUrl raw: String) -> String {
    var value = raw.trimmingCharacters(in: .whitespacesAndNewlines)
    while value.hasSuffix("/") {
      value.removeLast()
    }
    return value
  }

  static func parseBaseURL(_ raw: String) -> URL? {
    let normalized = normalize(serverUrl: raw)
    guard !normalized.isEmpty,
          let url = URL(string: normalized),
          let scheme = url.scheme?.lowercased(),
          scheme == "http" || scheme == "https",
          let host = url.host, !host.isEmpty
    else {
      return nil
    }
    return url
  }

  /// Builds `{base}/{path}` by string concatenation so a base URL with a path
  /// prefix (e.g. `https://host/photos`) is preserved.
  static func endpoint(_ raw: String, path: String) -> URL? {
    guard parseBaseURL(raw) != nil else {
      return nil
    }
    return URL(string: normalize(serverUrl: raw) + "/" + path)
  }
}

/// Persists `UploaderConfig` in the standard `UserDefaults`.
enum ConfigStore {
  private static let defaults = UserDefaults.standard
  private static let keyServerUrl = "nmcp.uploader.serverUrl"
  private static let keyAutoUpload = "nmcp.uploader.autoUpload"
  private static let keyWifiOnly = "nmcp.uploader.wifiOnly"
  private static let keyChargingOnly = "nmcp.uploader.chargingOnly"

  static func load() -> UploaderConfig {
    let fallback = UploaderConfig.defaults
    return UploaderConfig(
      serverUrl: defaults.string(forKey: keyServerUrl) ?? fallback.serverUrl,
      autoUpload: defaults.object(forKey: keyAutoUpload) as? Bool ?? fallback.autoUpload,
      wifiOnly: defaults.object(forKey: keyWifiOnly) as? Bool ?? fallback.wifiOnly,
      chargingOnly: defaults.object(forKey: keyChargingOnly) as? Bool ?? fallback.chargingOnly
    )
  }

  static func save(_ config: UploaderConfig) {
    defaults.set(UploaderConfig.normalize(serverUrl: config.serverUrl), forKey: keyServerUrl)
    defaults.set(config.autoUpload, forKey: keyAutoUpload)
    defaults.set(config.wifiOnly, forKey: keyWifiOnly)
    defaults.set(config.chargingOnly, forKey: keyChargingOnly)
  }
}

/// Run bookkeeping that must survive relaunches (the app is frequently
/// relaunched in the background by URLSession / BGTaskScheduler).
enum RuntimeState {
  private static let defaults = UserDefaults.standard
  private static let keyLastScanAt = "nmcp.uploader.lastScanAt"
  private static let keyLastRunAt = "nmcp.uploader.lastRunAt"
  private static let keyLastRunError = "nmcp.uploader.lastRunError"

  /// Epoch millis.
  static var lastScanAt: Double {
    get { defaults.double(forKey: keyLastScanAt) }
    set { defaults.set(newValue, forKey: keyLastScanAt) }
  }

  /// Epoch millis.
  static var lastRunAt: Double {
    get { defaults.double(forKey: keyLastRunAt) }
    set { defaults.set(newValue, forKey: keyLastRunAt) }
  }

  static var lastRunError: String? {
    get { defaults.string(forKey: keyLastRunError) }
    set {
      if let newValue {
        defaults.set(newValue, forKey: keyLastRunError)
      } else {
        defaults.removeObject(forKey: keyLastRunError)
      }
    }
  }

  static var nowMillis: Double {
    (Date().timeIntervalSince1970 * 1000).rounded()
  }
}

/// On-disk locations used by the uploader.
enum NmcpPaths {
  /// Application Support/nmcp — holds the SQLite database. Excluded from
  /// backups: PhotoKit local identifiers are device specific, so the DB is
  /// meaningless on a restored device.
  static var supportDirectory: URL {
    let base = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
    return base.appendingPathComponent("nmcp", isDirectory: true)
  }

  /// Caches/nmcp — transient export and multipart body files.
  static var stagingDirectory: URL {
    let base = FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask)[0]
    return base.appendingPathComponent("nmcp", isDirectory: true)
  }

  static var databaseFile: URL {
    supportDirectory.appendingPathComponent("uploader.sqlite", isDirectory: false)
  }

  /// Deterministic per item so it can be found (and deleted) after a relaunch.
  static func bodyFile(forItemId itemId: String) -> URL {
    stagingDirectory.appendingPathComponent("body-\(Hex.sha256(itemId)).bin", isDirectory: false)
  }

  /// File protection that still allows background access while the device
  /// is locked after the first unlock (uploads run with the screen locked).
  /// Empty on macOS (unit tests), where data protection classes do not apply.
  static var fileProtectionAttributes: [FileAttributeKey: Any]? {
    #if os(iOS)
    return [.protectionKey: FileProtectionType.completeUntilFirstUserAuthentication]
    #else
    return nil
    #endif
  }

  /// Applies `fileProtectionAttributes` to an existing file (no-op if absent).
  static func applyFileProtection(_ url: URL) {
    #if os(iOS)
    guard FileManager.default.fileExists(atPath: url.path) else { return }
    try? FileManager.default.setAttributes(
      [.protectionKey: FileProtectionType.completeUntilFirstUserAuthentication], ofItemAtPath: url.path)
    #endif
  }

  /// Creates `url` (and parents) with a file protection class that still
  /// allows background access while the device is locked after first unlock.
  static func ensureDirectory(_ url: URL, excludeFromBackup: Bool) throws {
    let fm = FileManager.default
    if !fm.fileExists(atPath: url.path) {
      try fm.createDirectory(at: url, withIntermediateDirectories: true, attributes: fileProtectionAttributes)
    }
    if excludeFromBackup {
      var mutableURL = url
      var values = URLResourceValues()
      values.isExcludedFromBackup = true
      try? mutableURL.setResourceValues(values)
    }
  }
}

extension NSLock {
  /// Named `locked` to avoid clashing with Foundation's `withLock` on newer SDKs.
  @inline(__always)
  func locked<T>(_ body: () throws -> T) rethrows -> T {
    lock()
    defer { unlock() }
    return try body()
  }
}
