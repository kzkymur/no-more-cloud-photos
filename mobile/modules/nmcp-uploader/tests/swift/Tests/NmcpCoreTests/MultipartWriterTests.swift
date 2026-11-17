import Foundation
import XCTest
@testable import NmcpCore

/// Temporary directory removed on deinit.
final class TempDir {
  let url: URL

  init() throws {
    url = FileManager.default.temporaryDirectory
      .appendingPathComponent("nmcp-tests-\(UUID().uuidString)", isDirectory: true)
    try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
  }

  deinit {
    try? FileManager.default.removeItem(at: url)
  }
}

final class MultipartWriterTests: XCTestCase {
  func testBoundaryFormat() {
    let boundary = Multipart.makeBoundary()
    XCTAssertTrue(boundary.hasPrefix("nmcp-"))
    XCTAssertEqual(boundary.utf8.count, 5 + 32)
    XCTAssertTrue(boundary.dropFirst(5).allSatisfy { "0123456789abcdef".contains($0) })
    XCTAssertNotEqual(boundary, Multipart.makeBoundary())
  }

  func testStageFromFileStreamsOriginalBytesUnaltered() throws {
    let dir = try TempDir()
    // Larger than one 1 MiB chunk, with bytes that look like a boundary.
    var original = Data((0..<(3 * Multipart.chunkSize + 17)).map { UInt8(truncatingIfNeeded: $0 &* 31) })
    original.append(Data("\r\n--nmcp-0000\r\n".utf8))
    let source = dir.url.appendingPathComponent("IMG_0001.HEIC")
    try original.write(to: source)
    let destination = dir.url.appendingPathComponent("body.bin")

    let body = try Multipart.stage(source: source, destination: destination, name: "IMG_0001.HEIC",
                                   mime: "image/heic", shouldContinue: { true })
    let written = try Data(contentsOf: destination)
    let head = Data(MultipartFormat.header(boundary: body.boundary, name: "IMG_0001.HEIC", mime: "image/heic").utf8)
    let tail = Data(MultipartFormat.trailer(boundary: body.boundary).utf8)
    XCTAssertEqual(written.prefix(head.count), head)
    XCTAssertEqual(written.suffix(tail.count), tail)
    XCTAssertEqual(written.subdata(in: head.count..<(written.count - tail.count)), original)
    XCTAssertEqual(body.contentSha256, Hex.sha256(original))
    XCTAssertEqual(body.size, Int64(written.count))
    XCTAssertEqual(body.payloadSize, Int64(original.count))
  }

  func testCancelledStagingDeletesPartialBody() throws {
    let dir = try TempDir()
    let source = dir.url.appendingPathComponent("src")
    try Data(repeating: 7, count: 2 * Multipart.chunkSize).write(to: source)
    let destination = dir.url.appendingPathComponent("body.bin")
    var calls = 0
    XCTAssertThrowsError(try Multipart.stage(source: source, destination: destination, name: "a.jpg", mime: "image/jpeg",
                                             shouldContinue: { calls += 1; return calls < 2 })) { error in
      guard case Multipart.StagingError.cancelled? = error as? Multipart.StagingError else {
        return XCTFail("unexpected \(error)")
      }
    }
    XCTAssertFalse(FileManager.default.fileExists(atPath: destination.path))
  }

  func testEmptyPayloadIsRefusedAndDeleted() throws {
    let dir = try TempDir()
    let destination = dir.url.appendingPathComponent("body.bin")
    let writer = try MultipartBodyWriter(destination: destination, name: "a.jpg", mime: "image/jpeg")
    XCTAssertThrowsError(try writer.finish()) { error in
      guard case Multipart.StagingError.emptyPayload? = error as? Multipart.StagingError else {
        return XCTFail("unexpected \(error)")
      }
    }
    XCTAssertFalse(FileManager.default.fileExists(atPath: destination.path))
  }

  func testDiscardDeletesAndBlocksFurtherWrites() throws {
    let dir = try TempDir()
    let destination = dir.url.appendingPathComponent("body.bin")
    let writer = try MultipartBodyWriter(destination: destination, name: "a.jpg", mime: "image/jpeg")
    try writer.append(Data([1, 2, 3]))
    writer.discard()
    XCTAssertFalse(FileManager.default.fileExists(atPath: destination.path))
    XCTAssertThrowsError(try writer.append(Data([4])))
    XCTAssertThrowsError(try writer.finish())
    writer.discard() // idempotent
  }

  func testWriterReplacesStaleBodyFromPreviousRun() throws {
    let dir = try TempDir()
    let destination = dir.url.appendingPathComponent("body.bin")
    try Data(repeating: 0xEE, count: 4096).write(to: destination)
    let writer = try MultipartBodyWriter(destination: destination, name: "b.png", mime: "image/png", boundary: "nmcp-x")
    try writer.append(Data([9]))
    let body = try writer.finish()
    let expected = Data((MultipartFormat.header(boundary: "nmcp-x", name: "b.png", mime: "image/png")).utf8)
      + Data([9]) + Data(MultipartFormat.trailer(boundary: "nmcp-x").utf8)
    XCTAssertEqual(try Data(contentsOf: destination), expected)
    XCTAssertEqual(body.size, Int64(expected.count))
  }

  func testBodyFileIsPerItem() {
    XCTAssertEqual(NmcpPaths.bodyFile(forItemId: "A/L0/001#1"), NmcpPaths.bodyFile(forItemId: "A/L0/001#1"))
    XCTAssertNotEqual(NmcpPaths.bodyFile(forItemId: "A/L0/001#1"), NmcpPaths.bodyFile(forItemId: "A/L0/001#9"))
  }
}
