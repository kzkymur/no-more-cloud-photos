import Foundation
import XCTest
@testable import NmcpCore

/// Asserts ios/MultipartFormat.swift against testdata/contract-vectors.json
/// (shared with the Go server and the Kotlin client).
final class ContractVectorTests: XCTestCase {
  struct Vectors: Decodable {
    struct Case: Decodable {
      struct Expected: Decodable {
        let contentSha256, idempotencyKey, filenameFallback, filenameStar, contentType, head, tail: String
      }
      let platform, itemId, name, mime, contentLatin1, boundary: String
      let expected: Expected
    }
    let cases: [Case]
  }

  /// tests/swift/Tests/NmcpCoreTests/<this file> -> nmcp-uploader/testdata/contract-vectors.json
  static var vectorsURL: URL {
    URL(fileURLWithPath: #filePath)
      .deletingLastPathComponent() // -> Tests/NmcpCoreTests
      .deletingLastPathComponent() // -> Tests
      .deletingLastPathComponent() // -> swift
      .deletingLastPathComponent() // -> tests
      .deletingLastPathComponent() // -> nmcp-uploader
      .appendingPathComponent("testdata/contract-vectors.json")
  }

  static func loadVectors() throws -> Vectors {
    try JSONDecoder().decode(Vectors.self, from: Data(contentsOf: vectorsURL))
  }

  static func latin1(_ string: String) -> Data {
    Data(string.unicodeScalars.map { scalar -> UInt8 in
      precondition(scalar.value <= 0xFF, "contentLatin1 must be Latin-1")
      return UInt8(scalar.value)
    })
  }

  /// Byte comparison: Swift `String ==` uses canonical equivalence and would
  /// hide NFC/NFD differences.
  func assertBytes(_ want: String, _ got: String, _ label: String, file: StaticString = #filePath, line: UInt = #line) {
    XCTAssertEqual(Array(want.utf8), Array(got.utf8),
                   "\(label)\n  want: \(want.debugDescription)\n  got:  \(got.debugDescription)", file: file, line: line)
  }

  func testVectorsMatch() throws {
    let vectors = try Self.loadVectors()
    XCTAssertFalse(vectors.cases.isEmpty, "no vectors")
    for (i, c) in vectors.cases.enumerated() {
      let label = "case \(i) (\(c.name))"
      let sha = Hex.sha256(Self.latin1(c.contentLatin1))
      assertBytes(c.expected.contentSha256, sha, "\(label) sha")
      assertBytes(c.expected.idempotencyKey,
                  MultipartFormat.idempotencyKey(platform: c.platform, itemId: c.itemId, contentSha256: sha, filename: c.name),
                  "\(label) key")
      assertBytes(c.expected.filenameFallback, MultipartFormat.asciiFallback(c.name), "\(label) fallback")
      assertBytes(c.expected.filenameStar, MultipartFormat.rfc5987(c.name), "\(label) filename*")
      assertBytes(c.expected.contentType, MultipartFormat.contentType(c.mime), "\(label) content type")
      assertBytes(c.expected.head, MultipartFormat.header(boundary: c.boundary, name: c.name, mime: c.mime), "\(label) head")
      assertBytes(c.expected.tail, MultipartFormat.trailer(boundary: c.boundary), "\(label) tail")
      XCTAssertEqual(c.expected.idempotencyKey.utf8.count, 70, "\(label) key length")
    }
  }

  /// The staged body is exactly head + original bytes + tail, its hash is the
  /// payload hash, and the key derived from it matches the vector.
  func testWriterProducesVectorBody() throws {
    let vectors = try Self.loadVectors()
    let dir = try TempDir()
    for (i, c) in vectors.cases.enumerated() {
      let content = Self.latin1(c.contentLatin1)
      let url = dir.url.appendingPathComponent("body-\(i).bin")
      let writer = try MultipartBodyWriter(destination: url, name: c.name, mime: c.mime, boundary: c.boundary)
      // Uneven chunks, like PhotoKit's dataReceivedHandler.
      var offset = 0
      var step = 1
      while offset < content.count {
        let end = min(content.count, offset + step)
        try writer.append(content.subdata(in: offset..<end))
        offset = end
        step += 2
      }
      let body = try writer.finish()

      var expected = Data(c.expected.head.utf8)
      expected.append(content)
      expected.append(Data(c.expected.tail.utf8))
      XCTAssertEqual(try Data(contentsOf: url), expected, "case \(i) body bytes")
      XCTAssertEqual(body.size, Int64(expected.count))
      XCTAssertEqual(body.payloadSize, Int64(content.count))
      XCTAssertEqual(body.contentSha256, c.expected.contentSha256)
      XCTAssertEqual(body.name, c.name)
      XCTAssertEqual(body.contentType, "multipart/form-data; boundary=\(c.boundary)")
      if c.platform == MultipartFormat.platform {
        // What UploadEngine.submit sends: key over the name written in the body.
        assertBytes(c.expected.idempotencyKey,
                    MultipartFormat.idempotencyKey(itemId: c.itemId, contentSha256: body.contentSha256, filename: body.name),
                    "case \(i) engine key")
      }
    }
  }
}
