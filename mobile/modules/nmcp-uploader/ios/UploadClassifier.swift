import Foundation
#if canImport(FoundationNetworking)
import FoundationNetworking // Linux; Apple platforms have it in Foundation
#endif

// Pure Foundation decision logic of the upload engine, kept out of
// UploadEngine.swift (Photos/UIKit) so tests/swift can unit-test it on macOS.

/// What to do with an item after its upload task finished.
enum UploadOutcome: Equatable {
  case uploaded(mediaId: String?)
  /// Cut off before a response (cancelled by us or by iOS, background daemon
  /// disconnected, body file gone): back to pending, `interrupts + 1`, no
  /// attempt counted.
  case interrupted
  /// Counted as an attempt. `permanent` stops automatic retries.
  case failed(message: String, permanent: Bool)
  /// Server-wide problem: the item goes back to pending without an attempt
  /// and the run ends with `message`. `interrupted` is true for transport
  /// errors (the item is then also moved back in the queue, so one file that
  /// always times out cannot block the others), false for HTTP statuses.
  case stopRun(message: String, itemError: String, interrupted: Bool)
}

enum UploadClassifier {
  static func classify(response: URLResponse?, error: Error?, body: Data) -> UploadOutcome {
    if let error {
      let nsError = error as NSError
      if nsError.domain == NSURLErrorDomain, interruptedCodes.contains(nsError.code) {
        return .interrupted
      }
      let message = describeTransportError(nsError)
      if nsError.domain == NSURLErrorDomain, unreachableCodes.contains(nsError.code) {
        return .stopRun(message: "Server unreachable: \(message)", itemError: message, interrupted: true)
      }
      return .failed(message: message, permanent: false)
    }
    guard let http = response as? HTTPURLResponse else {
      return .failed(message: "The server did not send a valid HTTP response.", permanent: false)
    }

    let status = http.statusCode
    let json = (try? JSONSerialization.jsonObject(with: body)) as? [String: Any]
    let apiError = json?["error"] as? [String: Any]
    let code = apiError?["code"] as? String
    let apiMessage = apiError?["message"] as? String
    let message = httpMessage(status: status, code: code, apiMessage: apiMessage, body: body)

    switch status {
    case 200..<300:
      let media = json?["media"] as? [String: Any]
      return .uploaded(mediaId: stringValue(media?["id"]))
    case 409 where code == "duplicate_media":
      let details = apiError?["details"] as? [String: Any]
      return .uploaded(mediaId: stringValue(details?["existing_media_id"]))
    case 429, 502, 503, 504:
      // Server-wide back-pressure / outage: retry later instead of burning
      // an attempt on every queued item.
      return .stopRun(message: "The server is busy or unavailable (\(message)). Uploads will resume later.",
                      itemError: message, interrupted: false)
    case 408, 500...599:
      return .failed(message: message, permanent: false)
    case 401, 403, 404, 405:
      // These describe the endpoint/credentials, not the file: marking every
      // item permanently failed because of a wrong URL or proxy would be wrong.
      return .stopRun(message: "The server rejected the upload endpoint (\(message)). Check the server URL.",
                      itemError: message, interrupted: false)
    case 400..<500:
      return .failed(message: message, permanent: true)
    default:
      return .failed(message: "Unexpected response: \(message)", permanent: false)
    }
  }

  /// "HTTP <status> <code>: <message>" when the server sent a JSON error,
  /// otherwise "HTTP <status>: <body excerpt or reason phrase>".
  static func httpMessage(status: Int, code: String?, apiMessage: String?, body: Data) -> String {
    if let code {
      return "HTTP \(status) \(code): \(apiMessage ?? HTTPURLResponse.localizedString(forStatusCode: status))"
    }
    let excerpt = String(decoding: body.prefix(200), as: UTF8.self)
      .trimmingCharacters(in: .whitespacesAndNewlines)
    return "HTTP \(status): \(excerpt.isEmpty ? HTTPURLResponse.localizedString(forStatusCode: status) : excerpt)"
  }

  private static func stringValue(_ value: Any?) -> String? {
    switch value {
    case let string as String:
      return string
    case let number as NSNumber:
      return number.stringValue
    default:
      return nil
    }
  }

  /// Not the file's or the server's fault: re-queue without an attempt.
  static let interruptedCodes: Set<Int> = [
    NSURLErrorCancelled,
    NSURLErrorBackgroundSessionWasDisconnected,
    // The staged body vanished (e.g. Caches purged by iOS under storage
    // pressure): it is simply staged again.
    NSURLErrorFileDoesNotExist,
    NSURLErrorRequestBodyStreamExhausted,
  ]

  static let unreachableCodes: Set<Int> = [
    NSURLErrorCannotFindHost,
    NSURLErrorCannotConnectToHost,
    NSURLErrorDNSLookupFailed,
    NSURLErrorTimedOut,
    NSURLErrorNotConnectedToInternet,
    NSURLErrorSecureConnectionFailed,
    NSURLErrorServerCertificateUntrusted,
    NSURLErrorServerCertificateHasBadDate,
    NSURLErrorServerCertificateNotYetValid,
    NSURLErrorServerCertificateHasUnknownRoot,
    NSURLErrorAppTransportSecurityRequiresSecureConnection,
  ]

  static func describeTransportError(_ error: NSError) -> String {
    guard error.domain == NSURLErrorDomain else {
      return error.localizedDescription
    }
    switch error.code {
    case NSURLErrorAppTransportSecurityRequiresSecureConnection:
      return "Plain HTTP is blocked by iOS App Transport Security. Use an https:// server URL."
    case NSURLErrorServerCertificateUntrusted, NSURLErrorServerCertificateHasUnknownRoot,
         NSURLErrorServerCertificateHasBadDate, NSURLErrorServerCertificateNotYetValid:
      return "The server's TLS certificate is not trusted (\(error.localizedDescription))"
    case NSURLErrorCannotFindHost, NSURLErrorDNSLookupFailed:
      return "Server name could not be resolved (\(error.localizedDescription))"
    case NSURLErrorCannotConnectToHost:
      return "Could not connect to the server (\(error.localizedDescription))"
    case NSURLErrorTimedOut:
      return "The server did not respond in time (\(error.localizedDescription))"
    case NSURLErrorNotConnectedToInternet:
      return "No network connection (\(error.localizedDescription))"
    default:
      return "Network error: \(error.localizedDescription)"
    }
  }
}

/// Why reading an original from PhotoKit failed.
enum ExportFailureKind: Equatable {
  /// Device storage full (export or body write hit ENOSPC, or the margin check).
  case outOfSpace
  /// The original must come from iCloud and could not be downloaded right now
  /// (offline, iCloud unreachable, download stalled). Not counted as an attempt.
  case network
  /// Anything else; counted as an attempt.
  case other

  /// PHPhotosErrorDomain codes (PhotosTypes.h). Literals so this file stays
  /// Foundation-only; a mismatch would only degrade to `.other`.
  static let photosErrorDomain = "PHPhotosErrorDomain"
  static let photosNetworkAccessRequired = 3164
  static let photosNetworkError = 3169
  static let photosNotEnoughSpace = 3305
  static let cloudPhotoLibraryErrorDomain = "CloudPhotoLibraryErrorDomain"

  static func classify(_ error: Error) -> ExportFailureKind {
    var network = false
    var current: NSError? = error as NSError
    var depth = 0
    while let nsError = current, depth < 8 {
      switch nsError.domain {
      case NSCocoaErrorDomain where nsError.code == NSFileWriteOutOfSpaceError:
        return .outOfSpace
      case NSPOSIXErrorDomain where nsError.code == Int(ENOSPC):
        return .outOfSpace
      case photosErrorDomain where nsError.code == photosNotEnoughSpace:
        return .outOfSpace
      case photosErrorDomain where nsError.code == photosNetworkAccessRequired || nsError.code == photosNetworkError:
        network = true
      case NSURLErrorDomain, cloudPhotoLibraryErrorDomain:
        network = true
      default:
        break
      }
      current = nsError.userInfo[NSUnderlyingErrorKey] as? NSError
      depth += 1
    }
    return network ? .network : .other
  }
}

/// Stops a run that keeps failing on device-wide resource problems (offline
/// iCloud, full storage) instead of marching through the whole library,
/// while a single bad item still never blocks the queue.
struct ResourceFailureStreak {
  static let limit = 5
  private(set) var count = 0

  /// Returns true when the run should stop.
  mutating func record(_ kind: ExportFailureKind?) -> Bool {
    switch kind {
    case .network?, .outOfSpace?:
      count += 1
    default:
      count = 0
    }
    return count >= Self.limit
  }
}
