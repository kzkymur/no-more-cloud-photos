// Asserts ios/MultipartFormat.swift against testdata/contract-vectors.json.
// Built by the "Mobile iOS compile" workflow:
//   swiftc -o contract-test ios/MultipartFormat.swift tests/swift/main.swift
//   ./contract-test testdata/contract-vectors.json
import Foundation

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

var failures = 0
func check(_ label: String, _ want: String, _ got: String) {
  // Byte comparison: Swift String == uses canonical equivalence (would hide NFC/NFD differences).
  if Array(want.utf8) != Array(got.utf8) {
    failures += 1
    print("FAIL \(label)\n  want: \(want.debugDescription)\n  got:  \(got.debugDescription)")
  }
}

let path = CommandLine.arguments.dropFirst().first ?? "testdata/contract-vectors.json"
let vectors = try JSONDecoder().decode(Vectors.self, from: Data(contentsOf: URL(fileURLWithPath: path)))
precondition(!vectors.cases.isEmpty, "no vectors")

for (i, c) in vectors.cases.enumerated() {
  let label = "case \(i) (\(c.name))"
  let content = Data(c.contentLatin1.unicodeScalars.map { scalar -> UInt8 in
    precondition(scalar.value <= 0xFF, "contentLatin1 must be Latin-1")
    return UInt8(scalar.value)
  })
  let sha = Hex.sha256(content)
  check("\(label) sha", c.expected.contentSha256, sha)
  check("\(label) key", c.expected.idempotencyKey,
        MultipartFormat.idempotencyKey(platform: c.platform, itemId: c.itemId, contentSha256: sha, filename: c.name))
  check("\(label) fallback", c.expected.filenameFallback, MultipartFormat.asciiFallback(c.name))
  check("\(label) filename*", c.expected.filenameStar, MultipartFormat.rfc5987(c.name))
  check("\(label) content type", c.expected.contentType, MultipartFormat.contentType(c.mime))
  check("\(label) head", c.expected.head, MultipartFormat.header(boundary: c.boundary, name: c.name, mime: c.mime))
  check("\(label) tail", c.expected.tail, MultipartFormat.trailer(boundary: c.boundary))
}

if failures > 0 {
  print("\(failures) contract assertion(s) failed")
  exit(1)
}
print("OK: \(vectors.cases.count) contract vectors match")
