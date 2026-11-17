// swift-tools-version:5.9
// Unit tests for the Foundation-only part of the iOS uploader core
// (`cd mobile/modules/nmcp-uploader/tests/swift && swift test`, macOS).
// Sources/NmcpCore/*.swift are relative symlinks to ../../ios/*.swift:
// SwiftPM does not allow target paths outside the package root.
import PackageDescription

let package = Package(
  name: "NmcpCore",
  platforms: [.macOS(.v13)],
  targets: [
    .target(
      name: "NmcpCore",
      path: "Sources/NmcpCore",
      linkerSettings: [.linkedLibrary("sqlite3")]
    ),
    .testTarget(
      name: "NmcpCoreTests",
      dependencies: ["NmcpCore"],
      path: "Tests/NmcpCoreTests"
    ),
  ]
)
