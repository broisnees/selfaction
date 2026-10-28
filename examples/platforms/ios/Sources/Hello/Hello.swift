import SwiftUI

/// The greeting every sample in examples/platforms shows.
public enum Greeting {
    public static let text = "Hello from selfaction"
}

/// A view an iOS app target would put on screen.
public struct HelloView: View {
    public init() {}

    public var body: some View {
        Text(Greeting.text)
            .font(.title)
            .padding()
    }
}
