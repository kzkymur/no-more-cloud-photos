import Foundation
#if canImport(FoundationNetworking)
import FoundationNetworking
#endif
import XCTest
@testable import NmcpCore

final class UploadClassifierTests: XCTestCase {
  private func response(_ status: Int) -> HTTPURLResponse {
    HTTPURLResponse(url: URL(string: "https://photos.example.ts.net/media")!, statusCode: status,
                    httpVersion: "HTTP/1.1", headerFields: ["Content-Type": "application/json"])!
  }

  private func classify(_ status: Int, _ json: String = "") -> UploadOutcome {
    UploadClassifier.classify(response: response(status), error: nil, body: Data(json.utf8))
  }

  private func classify(urlError code: Int) -> UploadOutcome {
    UploadClassifier.classify(response: nil, error: NSError(domain: NSURLErrorDomain, code: code), body: Data())
  }

  private func apiError(_ code: String, details: String = "{}") -> String {
    #"{"error":{"code":"\#(code)","message":"msg \#(code)","details":\#(details)}}"#
  }

  private func assertStopRun(_ outcome: UploadOutcome, interrupted: Bool, file: StaticString = #filePath, line: UInt = #line) {
    guard case .stopRun(let message, let itemError, let wasInterrupted) = outcome else {
      return XCTFail("expected stopRun, got \(outcome)", file: file, line: line)
    }
    XCTAssertFalse(message.isEmpty, file: file, line: line)
    XCTAssertFalse(itemError.isEmpty, file: file, line: line)
    XCTAssertEqual(wasInterrupted, interrupted, file: file, line: line)
  }

  private func assertFailed(_ outcome: UploadOutcome, permanent: Bool, file: StaticString = #filePath, line: UInt = #line) {
    guard case .failed(let message, let isPermanent) = outcome else {
      return XCTFail("expected failed, got \(outcome)", file: file, line: line)
    }
    XCTAssertFalse(message.isEmpty, file: file, line: line)
    XCTAssertEqual(isPermanent, permanent, file: file, line: line)
  }

  // MARK: 2xx / 409

  func testSuccessReturnsMediaId() {
    XCTAssertEqual(classify(201, #"{"media":{"id":"m-123"}}"#), .uploaded(mediaId: "m-123"))
    XCTAssertEqual(classify(200, #"{"media":{"id":42}}"#), .uploaded(mediaId: "42"))
    XCTAssertEqual(classify(201, "not json"), .uploaded(mediaId: nil))
    XCTAssertEqual(classify(204), .uploaded(mediaId: nil))
  }

  func testDuplicateMediaCountsAsUploaded() {
    XCTAssertEqual(classify(409, apiError("duplicate_media", details: #"{"existing_media_id":"m-old"}"#)),
                   .uploaded(mediaId: "m-old"))
    XCTAssertEqual(classify(409, apiError("duplicate_media")), .uploaded(mediaId: nil))
  }

  func testIdempotencyConflictAndOther4xxArePermanent() {
    assertFailed(classify(409, apiError("idempotency_conflict")), permanent: true)
    assertFailed(classify(409, "plain conflict"), permanent: true)
    for status in [400, 410, 413, 415, 422] {
      assertFailed(classify(status, apiError("invalid_media")), permanent: true)
    }
    guard case .failed(let message, _) = classify(415, apiError("unsupported_media_type")) else {
      return XCTFail()
    }
    XCTAssertEqual(message, "HTTP 415 unsupported_media_type: msg unsupported_media_type")
  }

  func testTransientStatuses() {
    for status in [408, 500, 501, 505, 507, 599] {
      assertFailed(classify(status), permanent: false)
    }
  }

  func testEndpointAndBusyStatusesStopTheRun() {
    for status in [401, 403, 404, 405, 429, 502, 503, 504] {
      assertStopRun(classify(status, "<html>proxy</html>"), interrupted: false)
    }
  }

  func testUnexpectedStatusIsTransient() {
    assertFailed(classify(302), permanent: false)
    assertFailed(classify(101), permanent: false)
  }

  func testMessageUsesBodyExcerptWithoutJSON() {
    guard case .failed(let message, _) = classify(500, "  boom \n") else {
      return XCTFail()
    }
    XCTAssertEqual(message, "HTTP 500: boom")
  }

  // MARK: Transport errors

  func testCancellationAndDaemonLossAreInterruptions() {
    XCTAssertEqual(classify(urlError: NSURLErrorCancelled), .interrupted)
    XCTAssertEqual(classify(urlError: NSURLErrorBackgroundSessionWasDisconnected), .interrupted)
    XCTAssertEqual(classify(urlError: NSURLErrorFileDoesNotExist), .interrupted)
  }

  func testUnreachableStopsTheRunAndMovesTheItemBack() {
    for code in [NSURLErrorCannotFindHost, NSURLErrorCannotConnectToHost, NSURLErrorDNSLookupFailed, NSURLErrorTimedOut,
                 NSURLErrorNotConnectedToInternet, NSURLErrorSecureConnectionFailed, NSURLErrorServerCertificateUntrusted,
                 NSURLErrorAppTransportSecurityRequiresSecureConnection] {
      assertStopRun(classify(urlError: code), interrupted: true)
    }
  }

  func testConnectionDropMidTransferIsTransient() {
    assertFailed(classify(urlError: NSURLErrorNetworkConnectionLost), permanent: false)
    assertFailed(UploadClassifier.classify(response: nil, error: NSError(domain: "Other", code: 1), body: Data()),
                 permanent: false)
  }

  func testMissingHTTPResponseIsTransient() {
    assertFailed(UploadClassifier.classify(response: nil, error: nil, body: Data()), permanent: false)
  }

  func testTransportMessages() {
    let ats = UploadClassifier.describeTransportError(
      NSError(domain: NSURLErrorDomain, code: NSURLErrorAppTransportSecurityRequiresSecureConnection))
    XCTAssertTrue(ats.contains("https://"))
  }

  // MARK: Export failures

  func testExportFailureKinds() {
    XCTAssertEqual(ExportFailureKind.classify(NSError(domain: NSPOSIXErrorDomain, code: Int(ENOSPC))), .outOfSpace)
    XCTAssertEqual(ExportFailureKind.classify(NSError(domain: NSCocoaErrorDomain, code: NSFileWriteOutOfSpaceError)), .outOfSpace)
    XCTAssertEqual(ExportFailureKind.classify(NSError(domain: "PHPhotosErrorDomain", code: 3305)), .outOfSpace)
    XCTAssertEqual(ExportFailureKind.classify(NSError(domain: "PHPhotosErrorDomain", code: 3164)), .network)
    XCTAssertEqual(ExportFailureKind.classify(NSError(domain: "PHPhotosErrorDomain", code: 3169)), .network)
    XCTAssertEqual(ExportFailureKind.classify(NSError(domain: "CloudPhotoLibraryErrorDomain", code: 1005)), .network)
    XCTAssertEqual(ExportFailureKind.classify(NSError(domain: NSURLErrorDomain, code: NSURLErrorNotConnectedToInternet)), .network)
    XCTAssertEqual(ExportFailureKind.classify(NSError(domain: "PHPhotosErrorDomain", code: 3303)), .other)
    XCTAssertEqual(ExportFailureKind.classify(NSError(domain: "Other", code: 1)), .other)
    // Wrapped errors are unwrapped; space beats network.
    let wrapped = NSError(domain: "PHPhotosErrorDomain", code: -1, userInfo: [
      NSUnderlyingErrorKey: NSError(domain: NSURLErrorDomain, code: NSURLErrorTimedOut),
    ])
    XCTAssertEqual(ExportFailureKind.classify(wrapped), .network)
    let wrappedSpace = NSError(domain: NSURLErrorDomain, code: -1, userInfo: [
      NSUnderlyingErrorKey: NSError(domain: NSPOSIXErrorDomain, code: Int(ENOSPC)),
    ])
    XCTAssertEqual(ExportFailureKind.classify(wrappedSpace), .outOfSpace)
  }

  func testResourceFailureStreak() {
    var streak = ResourceFailureStreak()
    for _ in 0..<(ResourceFailureStreak.limit - 1) {
      XCTAssertFalse(streak.record(.network))
    }
    XCTAssertFalse(streak.record(nil)) // a good item resets
    XCTAssertFalse(streak.record(.other))
    for _ in 0..<(ResourceFailureStreak.limit - 1) {
      XCTAssertFalse(streak.record(.outOfSpace))
    }
    XCTAssertTrue(streak.record(.network))
  }
}
