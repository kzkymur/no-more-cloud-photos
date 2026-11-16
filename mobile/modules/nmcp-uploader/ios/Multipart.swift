import CryptoKit
import Foundation
import Security

/// A multipart/form-data body staged on disk, ready for
/// `URLSession.uploadTask(with:fromFile:)`.
struct StagedBody {
  let url: URL
  /// Total body size in bytes (header + payload + trailer).
  let size: Int64
  /// Lowercase hex SHA-256 of the payload (the original file bytes only).
  let contentSha256: String
  let boundary: String
  /// The filename written into the body; the Idempotency-Key must use the same one.
  let name: String

  var contentType: String {
    "multipart/form-data; boundary=\(boundary)"
  }
}

/// Stages the upload body on disk. The wire format itself lives in
/// MultipartFormat.swift (unit-tested against testdata/contract-vectors.json).
enum Multipart {
  enum StagingError: Error {
    case cancelled
    case createFailed(String)
  }

  static let chunkSize = 1 << 20

  /// "nmcp-" + 32 random lowercase hex chars.
  static func makeBoundary() -> String {
    var bytes = [UInt8](repeating: 0, count: 16)
    let status = bytes.withUnsafeMutableBytes { buffer in
      SecRandomCopyBytes(kSecRandomDefault, buffer.count, buffer.baseAddress!)
    }
    if status != errSecSuccess {
      var rng = SystemRandomNumberGenerator()
      bytes = (0..<16).map { _ in UInt8.random(in: 0...255, using: &rng) }
    }
    return "nmcp-" + Hex.encode(bytes)
  }

  /// Streams `source` into a multipart body at `destination` in 1 MiB chunks,
  /// computing the payload SHA-256 on the way. Memory use is constant
  /// regardless of the file size. `shouldContinue` is polled per chunk.
  static func stage(
    source: URL,
    destination: URL,
    name: String,
    mime: String,
    shouldContinue: () -> Bool
  ) throws -> StagedBody {
    let boundary = makeBoundary()
    let headerData = Data(MultipartFormat.header(boundary: boundary, name: name, mime: mime).utf8)
    let trailerData = Data(MultipartFormat.trailer(boundary: boundary).utf8)

    let fm = FileManager.default
    if fm.fileExists(atPath: destination.path) {
      try fm.removeItem(at: destination)
    }
    guard fm.createFile(
      atPath: destination.path,
      contents: nil,
      attributes: [.protectionKey: FileProtectionType.completeUntilFirstUserAuthentication]
    ) else {
      throw StagingError.createFailed(destination.lastPathComponent)
    }

    let reader = try FileHandle(forReadingFrom: source)
    defer { try? reader.close() }
    let writer = try FileHandle(forWritingTo: destination)
    var writerOpen = true
    defer {
      if writerOpen {
        try? writer.close()
      }
    }

    var hasher = SHA256()
    var payloadSize: Int64 = 0
    try writer.write(contentsOf: headerData)

    while true {
      guard shouldContinue() else {
        throw StagingError.cancelled
      }
      let finished: Bool = try autoreleasepool {
        guard let chunk = try reader.read(upToCount: chunkSize), !chunk.isEmpty else {
          return true
        }
        hasher.update(data: chunk)
        try writer.write(contentsOf: chunk)
        payloadSize += Int64(chunk.count)
        return false
      }
      if finished {
        break
      }
    }

    try writer.write(contentsOf: trailerData)
    writerOpen = false
    try writer.close()

    return StagedBody(
      url: destination,
      size: Int64(headerData.count) + payloadSize + Int64(trailerData.count),
      contentSha256: Hex.encode(hasher.finalize()),
      boundary: boundary,
      name: name
    )
  }
}
