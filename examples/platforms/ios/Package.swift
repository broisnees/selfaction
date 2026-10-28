// swift-tools-version: 5.9
// A small SwiftUI package the platforms workflow builds for the iOS simulator
// on a Mac worker with Xcode: xcodebuild -scheme Hello -destination 'generic/platform=iOS Simulator' build
import PackageDescription

let package = Package(
    name: "Hello",
    platforms: [.iOS(.v17), .macOS(.v14)],
    products: [.library(name: "Hello", targets: ["Hello"])],
    targets: [.target(name: "Hello")]
)
