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
  /// Payload (original file) size in bytes.
  let payloadSize: Int64
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
    case closed
    case emptyPayload
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

  /// Streams the file at `source` into a multipart body at `destination` in
  /// 1 MiB chunks. Used by tests; the engine streams PhotoKit data straight
  /// into a `MultipartBodyWriter` instead (single copy).
  static func stage(
    source: URL,
    destination: URL,
    name: String,
    mime: String,
    shouldContinue: () -> Bool
  ) throws -> StagedBody {
    let writer = try MultipartBodyWriter(destination: destination, name: name, mime: mime)
    do {
      let reader = try FileHandle(forReadingFrom: source)
      defer { try? reader.close() }
      while true {
        guard shouldContinue() else {
          throw StagingError.cancelled
        }
        let finished: Bool = try autoreleasepool {
          guard let chunk = try reader.read(upToCount: chunkSize), !chunk.isEmpty else {
            return true
          }
          try writer.append(chunk)
          return false
        }
        if finished {
          break
        }
      }
      return try writer.finish()
    } catch {
      writer.discard()
      throw error
    }
  }
}

/// Writes one multipart body file incrementally: header on creation, each
/// payload chunk as it arrives (hashing it on the way), trailer on `finish`.
/// The payload SHA-256 is computed over exactly the bytes written, so the
/// Idempotency-Key always describes the body that is sent.
///
/// Not thread-safe; the caller serializes access.
final class MultipartBodyWriter {
  let url: URL
  let name: String
  let boundary: String
  private let headerSize: Int64
  private let trailer: Data
  private var handle: FileHandle?
  private var hasher = SHA256()
  private(set) var payloadSize: Int64 = 0

  /// Creates (replacing) `destination` and writes the part header.
  init(destination: URL, name: String, mime: String, boundary: String = Multipart.makeBoundary()) throws {
    url = destination
    self.name = name
    self.boundary = boundary
    let header = Data(MultipartFormat.header(boundary: boundary, name: name, mime: mime).utf8)
    headerSize = Int64(header.count)
    trailer = Data(MultipartFormat.trailer(boundary: boundary).utf8)

    let fm = FileManager.default
    if fm.fileExists(atPath: destination.path) {
      try fm.removeItem(at: destination)
    }
    guard fm.createFile(atPath: destination.path, contents: nil, attributes: NmcpPaths.fileProtectionAttributes) else {
      throw Multipart.StagingError.createFailed(destination.lastPathComponent)
    }
    let handle = try FileHandle(forWritingTo: destination)
    self.handle = handle
    do {
      try handle.write(contentsOf: header)
    } catch {
      discard()
      throw error
    }
  }

  deinit {
    try? handle?.close()
  }

  func append(_ chunk: Data) throws {
    guard let handle else {
      throw Multipart.StagingError.closed
    }
    guard !chunk.isEmpty else { return }
    hasher.update(data: chunk)
    try handle.write(contentsOf: chunk)
    payloadSize += Int64(chunk.count)
  }

  /// Writes the trailer and closes the file. An empty payload is refused: no
  /// photo or video original is zero bytes, so it means a broken export.
  func finish() throws -> StagedBody {
    guard let handle else {
      throw Multipart.StagingError.closed
    }
    guard payloadSize > 0 else {
      discard()
      throw Multipart.StagingError.emptyPayload
    }
    try handle.write(contentsOf: trailer)
    self.handle = nil
    try handle.close()
    return StagedBody(
      url: url,
      size: headerSize + payloadSize + Int64(trailer.count),
      contentSha256: Hex.encode(hasher.finalize()),
      payloadSize: payloadSize,
      boundary: boundary,
      name: name
    )
  }

  /// Closes and deletes the partial file. Idempotent.
  func discard() {
    if let handle {
      try? handle.close()
      self.handle = nil
    }
    try? FileManager.default.removeItem(at: url)
  }
}
