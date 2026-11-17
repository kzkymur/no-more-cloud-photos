import CryptoKit
import Foundation

// The upload wire format shared with Android (MultipartFormat.kt). Pure
// Foundation + CryptoKit so tests/swift can compile it on its own and assert
// it against testdata/contract-vectors.json.
//
//     --B\r\n
//     Content-Disposition: form-data; name="file"; filename="<fallback>"; filename*=UTF-8''<rfc5987>\r\n
//     Content-Type: <type>\r\n
//     \r\n
//     <original bytes>\r\n
//     --B--\r\n

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

  static func sha256(_ data: Data) -> String {
    encode(SHA256.hash(data: data))
  }
}

enum MultipartFormat {
  static let platform = "ios"

  /// "nmcp1-" + hex(SHA-256("<platform>|<itemId>|<contentSha256>|<filename>")): 70 ASCII bytes.
  /// A changed content or name yields a new key, so the server answers 201 or
  /// 409 duplicate_media instead of 409 idempotency_conflict.
  static func idempotencyKey(platform: String = MultipartFormat.platform, itemId: String, contentSha256: String, filename: String) -> String {
    "nmcp1-" + Hex.sha256("\(platform)|\(itemId)|\(contentSha256)|\(filename)")
  }

  static func header(boundary: String, name: String, mime: String) -> String {
    "--\(boundary)\r\n"
      + "Content-Disposition: form-data; name=\"file\"; filename=\"\(asciiFallback(name))\"; filename*=UTF-8''\(rfc5987(name))\r\n"
      + "Content-Type: \(contentType(mime))\r\n"
      + "\r\n"
  }

  static func trailer(boundary: String) -> String {
    "\r\n--\(boundary)--\r\n"
  }

  /// One "_" per UTF-16 code unit that is not printable ASCII, or is `"` or `\`
  /// (the same loop as Kotlin's `for (ch in name)`).
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

  /// RFC 8187 `value-chars`: `attr-char` is kept, every other UTF-8 byte is `%XX` (uppercase).
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

  /// A syntactically valid, non-multipart `type/subtype`, else application/octet-stream.
  static func contentType(_ mime: String) -> String {
    let m = mime.trimmingCharacters(in: .whitespacesAndNewlines)
    let parts = m.split(separator: "/", omittingEmptySubsequences: false)
    let valid = parts.count == 2 && parts.allSatisfy(isMimeToken) && !m.lowercased().hasPrefix("multipart/")
    return valid ? m : "application/octet-stream"
  }

  /// `[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]*`
  private static func isMimeToken(_ part: Substring) -> Bool {
    guard let first = part.utf8.first, isAlnum(first) else { return false }
    return part.utf8.allSatisfy { b in
      isAlnum(b) || b == 0x21 || b == 0x23 || b == 0x24 || b == 0x26 || b == 0x5E || b == 0x5F
        || b == 0x2E || b == 0x2B || b == 0x2D
    }
  }

  private static func isAlnum(_ b: UInt8) -> Bool {
    (0x30...0x39).contains(b) || (0x41...0x5A).contains(b) || (0x61...0x7A).contains(b)
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
}
