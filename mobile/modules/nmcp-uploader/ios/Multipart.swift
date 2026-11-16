import CryptoKit
import Foundation
import Security

/// Lowercase hex helpers.
enum Hex {
  private static let digits: [UInt8] = Array("0123456789abcdef".utf8)

  static func encode<S: Sequence>(_ bytes: S) -> String where S.Element == UInt8 {
    var out = [UInt8]()
    out.reserveCapacity(64)
    for byte in bytes {
      out.append(digits[Int(byte >> 4)])
      out.append(digits[Int(byte & 0x0F)])
    }
    return String(decoding: out, as: UTF8.self)
  }

  /// Lowercase hex SHA-256 of the UTF-8 bytes of `string`.
  static func sha256(_ string: String) -> String {
    encode(SHA256.hash(data: Data(string.utf8)))
  }
}

/// A multipart/form-data body staged on disk, ready for
/// `URLSession.uploadTask(with:fromFile:)`.
struct StagedBody {
  let url: URL
  /// Total body size in bytes (header + payload + trailer).
  let size: Int64
  /// Lowercase hex SHA-256 of the payload (the original file bytes only).
  let contentSha256: String
  let boundary: String

  var contentType: String {
    "multipart/form-data; boundary=\(boundary)"
  }
}

/// Builds the upload body. The wire format must stay semantically identical
/// to the Android implementation:
///
///     --B\r\n
///     Content-Disposition: form-data; name="file"; filename="<ascii>"; filename*=UTF-8''<pct>\r\n
///     Content-Type: <mime>\r\n
///     \r\n
///     <bytes>\r\n
///     --B--\r\n
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

  /// `filename=` fallback: every UTF-16 code unit that is not printable ASCII,
  /// plus `"` and `\`, becomes `_` (one `_` per UTF-16 unit, like a Kotlin
  /// `String` char loop). Control characters are also replaced so a hostile
  /// name can never break the header.
  static func asciiFallback(_ name: String) -> String {
    var out = [UInt8]()
    out.reserveCapacity(name.utf16.count)
    for unit in name.utf16 {
      if unit >= 0x20, unit < 0x7F, unit != 0x22, unit != 0x5C {
        out.append(UInt8(unit))
      } else {
        out.append(0x5F) // "_"
      }
    }
    return String(decoding: out, as: UTF8.self)
  }

  /// RFC 5987 `ext-value` encoding of the UTF-8 bytes: `attr-char` is kept,
  /// everything else is `%XX` (uppercase hex).
  static func rfc5987(_ value: String) -> String {
    let upper: [UInt8] = Array("0123456789ABCDEF".utf8)
    var out = [UInt8]()
    out.reserveCapacity(value.utf8.count * 3)
    for byte in value.utf8 {
      if isAttrChar(byte) {
        out.append(byte)
      } else {
        out.append(0x25) // "%"
        out.append(upper[Int(byte >> 4)])
        out.append(upper[Int(byte & 0x0F)])
      }
    }
    return String(decoding: out, as: UTF8.self)
  }

  /// attr-char = ALPHA / DIGIT / "!" / "#" / "$" / "&" / "+" / "-" / "." / "^" / "_" / "`" / "|" / "~"
  private static func isAttrChar(_ b: UInt8) -> Bool {
    switch b {
    case 0x30...0x39, 0x41...0x5A, 0x61...0x7A:
      return true
    case 0x21, 0x23, 0x24, 0x26, 0x2B, 0x2D, 0x2E, 0x5E, 0x5F, 0x60, 0x7C, 0x7E:
      return true
    default:
      return false
    }
  }

  static func header(boundary: String, name: String, mime: String) -> String {
    "--\(boundary)\r\n"
      + "Content-Disposition: form-data; name=\"file\"; filename=\"\(asciiFallback(name))\"; filename*=UTF-8''\(rfc5987(name))\r\n"
      + "Content-Type: \(sanitizeHeaderValue(mime))\r\n"
      + "\r\n"
  }

  static func trailer(boundary: String) -> String {
    "\r\n--\(boundary)--\r\n"
  }

  private static func sanitizeHeaderValue(_ value: String) -> String {
    let cleaned = value.filter { !$0.isNewline && $0 != "\0" }
    return cleaned.isEmpty ? "application/octet-stream" : cleaned
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
    let headerData = Data(header(boundary: boundary, name: name, mime: mime).utf8)
    let trailerData = Data(trailer(boundary: boundary).utf8)

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
      boundary: boundary
    )
  }

  /// "nmcp1-" + hex(SHA-256("ios|<itemId>|<contentSha>")).
  static func idempotencyKey(itemId: String, contentSha256: String) -> String {
    "nmcp1-" + Hex.sha256("ios|\(itemId)|\(contentSha256)")
  }
}
