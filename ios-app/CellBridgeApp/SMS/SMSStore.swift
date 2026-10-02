import Foundation
import Combine

/// Message model for the SMS tab.
struct SMSMessage: Identifiable {
    let id = UUID()
    let peer: String
    let body: String
    let outgoing: Bool
    let date: Date
}

/// Owns SMS state. Sending goes through SIP MESSAGE (gateway -> AT+CMGS).
final class SMSStore: ObservableObject {
    @Published var messages: [SMSMessage] = []
    weak var sip: SIPClient?

    func send(to: String, body: String) {
        sip?.sendMessage(to: to, body: body)
        messages.append(SMSMessage(peer: to, body: body, outgoing: true, date: Date()))
    }

    func received(from: String, body: String) {
        messages.append(SMSMessage(peer: from, body: body, outgoing: false, date: Date()))
    }
}
