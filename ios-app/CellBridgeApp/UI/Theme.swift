import SwiftUI
import UIKit

enum Theme {
    // System-native colours that follow light/dark mode
    static let accent = Color.accentColor
    static let danger = Color.red
    static let success = Color.green
    static let warning = Color.orange
    static let idle = Color.secondary

    // SMS bubble colours
    static let bubbleOut = Color.accentColor
    static let bubbleIn = Color(uiColor: .secondarySystemGroupedBackground)
}

// MARK: - System semantic colour helpers
extension Color {
    static let systemBackground = Color(uiColor: .systemBackground)
    static let secondarySystemBackground = Color(uiColor: .secondarySystemBackground)
    static let tertiarySystemBackground = Color(uiColor: .tertiarySystemBackground)
    static let systemGroupedBackground = Color(uiColor: .systemGroupedBackground)
    static let secondarySystemGroupedBackground = Color(uiColor: .secondarySystemGroupedBackground)
    static let tertiarySystemGroupedBackground = Color(uiColor: .tertiarySystemGroupedBackground)

    static let messageBubbleReceived = Color(uiColor: .secondarySystemGroupedBackground)
    static let messageBubbleSent = Color.accentColor
}

// MARK: - Keyboard dismissal
extension View {
    /// Dismiss keyboard when tapping outside text fields.
    func dismissKeyboardOnTap() -> some View {
        self.onTapGesture {
            UIApplication.shared.sendAction(
                #selector(UIResponder.resignFirstResponder),
                to: nil, from: nil, for: nil)
        }
    }
}
