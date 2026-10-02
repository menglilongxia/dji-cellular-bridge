import Foundation
import Combine
import CallKit
import SwiftUI
import UIKit

/// Wires SIPClient <-> CallManager <-> SMSStore <-> UI, and keeps the server
/// settings around between launches.
final class AppState: ObservableObject, SIPClientDelegate {
    let sip = SIPClient()
    let calls = CallManager()
    let sms = SMSStore()
    let contacts = ContactsManager()
    let history = HistoryStore()

    private let gateway = GatewayClient()
    private let defaults = UserDefaults.standard
    private var statusTimer: Timer?
    private var cancellables = Set<AnyCancellable>()

    // MARK: - live state

    @Published var registered = false
    @Published var connecting = false
    @Published var statusLine = "未连接"
    @Published var activeCallNumber: String?
    @Published var activeCallUUID: UUID?
    @Published private(set) var gatewayStatus: GatewayStatus?

    // MARK: - persisted settings

    @Published var serverHost: String { didSet { defaults.set(serverHost, forKey: Keys.host) } }
    @Published var serverPort: Int { didSet { defaults.set(serverPort, forKey: Keys.port) } }
    @Published var sipUser: String { didSet { defaults.set(sipUser, forKey: Keys.user) } }
    @Published var sipPassword: String { didSet { defaults.set(sipPassword, forKey: Keys.password) } }
    @Published var apiPort: Int { didSet { defaults.set(apiPort, forKey: Keys.apiPort) } }
    /// Auto-connect on app launch when server settings are configured.
    @Published var autoConnect: Bool { didSet { defaults.set(autoConnect, forKey: Keys.autoConnect) } }

    private enum Keys {
        static let host = "cb.serverHost"
        static let port = "cb.serverPort"
        static let user = "cb.sipUser"
        static let password = "cb.sipPassword"
        static let apiPort = "cb.apiPort"
        static let autoConnect = "cb.autoConnect"
    }

    init() {
        let d = UserDefaults.standard
        serverHost = d.string(forKey: Keys.host) ?? "100.87.254.117"
        serverPort = d.object(forKey: Keys.port) as? Int ?? 5060
        sipUser = d.string(forKey: Keys.user) ?? "iphone"
        sipPassword = d.string(forKey: Keys.password) ?? "changeme"
        apiPort = d.object(forKey: Keys.apiPort) as? Int ?? 8787
        autoConnect = d.object(forKey: Keys.autoConnect) as? Bool ?? true

        sip.delegate = self
        sip.onAudioFrame = { [weak self] pcm in self?.calls.audio.play(pcm: pcm) }
        sip.onDiagnostic = { [weak self] message in self?.reportAudioState(message) }
        calls.sip = sip
        calls.onAudioState = { [weak self] message in
            self?.reportAudioState(message)
        }
        sms.sip = sip
        
        // Request contacts access on init
        contacts.requestAccess()

        // Forward history / contacts changes -> AppState objectWillChange
        history.objectWillChange
            .receive(on: DispatchQueue.main)
            .sink { [weak self] _ in self?.objectWillChange.send() }
            .store(in: &cancellables)
        contacts.objectWillChange
            .receive(on: DispatchQueue.main)
            .sink { [weak self] _ in self?.objectWillChange.send() }
            .store(in: &cancellables)
    }
    
    /// Call this on app launch — auto-connects if settings are available.
    func onAppLaunch() {
        if autoConnect && !serverHost.trimmingCharacters(in: .whitespaces).isEmpty {
            connect()
        }
    }

    private func reportAudioState(_ message: String) {
        guard !serverHost.isEmpty,
              let url = URL(string: "http://\(serverHost):\(apiPort)/api/v1/client/audio-state") else {
            return
        }
        var payload: [String: Any] = ["state": message]
        if !reportedInterfaces {
            reportedInterfaces = true
            payload["interfaces"] = Self.localIPv4Addresses()
        }
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try? JSONSerialization.data(withJSONObject: payload)
        req.timeoutInterval = 3
        URLSession.shared.dataTask(with: req).resume()
    }

    private var reportedInterfaces = false

    private static func localIPv4Addresses() -> [String] {
        var out: [String] = []
        var ifaddr: UnsafeMutablePointer<ifaddrs>?
        guard getifaddrs(&ifaddr) == 0, let first = ifaddr else { return out }
        defer { freeifaddrs(ifaddr) }
        for ptr in sequence(first: first, next: { $0.pointee.ifa_next }) {
            let flags = Int32(ptr.pointee.ifa_flags)
            let family = ptr.pointee.ifa_addr.pointee.sa_family
            guard family == UInt8(AF_INET), flags & IFF_UP != 0 else { continue }
            var host = [CChar](repeating: 0, count: Int(NI_MAXHOST))
            if getnameinfo(ptr.pointee.ifa_addr, socklen_t(ptr.pointee.ifa_addr.pointee.sa_len),
                           &host, socklen_t(host.count), nil, 0, NI_NUMERICHOST) == 0 {
                out.append(String(cString: host))
            }
        }
        return out
    }

    // MARK: - connection

    func connect() {
        let host = serverHost.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !host.isEmpty else {
            statusLine = "请先填写服务器地址"
            return
        }
        serverHost = host
        connecting = true
        statusLine = "连接中…"
        sip.username = sipUser
        sip.password = sipPassword
        sip.connect(host: host, port: serverPort)
        startStatusPolling()
        startUDPProbe()
        observeForegrounding()
    }

    private func observeForegrounding() {
        guard !observingForeground else { return }
        observingForeground = true
        NotificationCenter.default.addObserver(
            forName: UIApplication.didBecomeActiveNotification, object: nil, queue: .main
        ) { [weak self] _ in
            guard let self, !self.serverHost.isEmpty else { return }
            self.sip.prepareMedia(host: self.serverHost)
            // Sync history immediately on foreground — picks up calls/SMS
            // that arrived while the app was suspended.
            if self.registered {
                self.history.fullSync(host: self.serverHost, port: self.apiPort)
            }
            // Auto-reconnect if we lost registration while backgrounded
            if !self.registered && self.autoConnect {
                self.connect()
            }
        }
    }

    private var observingForeground = false

    // MARK: - UDP reachability probe

    private func startUDPProbe() {
        udpProbeTimer?.invalidate()
        udpProbeTimer = Timer.scheduledTimer(withTimeInterval: 5, repeats: true) { [weak self] _ in
            guard let self, self.registered, !self.serverHost.isEmpty else { return }
            RTPSession.echoTest(host: self.serverHost) { [weak self] message in
                self?.reportAudioState(message)
            }
        }
    }

    private var udpProbeTimer: Timer?

    func disconnect() {
        stopStatusPolling()
        sip.disconnect()
        registered = false
        connecting = false
        gatewayStatus = nil
        statusLine = "未连接"
    }

    func ensureConnected() {
        if !registered { connect() }
    }

    func refreshGatewayStatus() {
        guard !serverHost.isEmpty else { return }
        gateway.fetchStatus(host: serverHost, port: apiPort) { [weak self] status in
            DispatchQueue.main.async {
                guard let self else { return }
                self.gatewayStatus = status
                if let s = status, !s.registered, self.registered {
                    self.sip.register()
                }
            }
        }
    }

    private func startStatusPolling() {
        stopStatusPolling()
        refreshGatewayStatus()
        let timer = Timer(timeInterval: 5, repeats: true) { [weak self] _ in
            self?.refreshGatewayStatus()
            // Sync missed calls/SMS from gateway every poll cycle
            if let self, self.registered {
                self.history.syncFromGateway(host: self.serverHost, port: self.apiPort)
            }
        }
        RunLoop.main.add(timer, forMode: .common)
        statusTimer = timer
    }

    private func stopStatusPolling() {
        statusTimer?.invalidate()
        statusTimer = nil
    }

    var stateColor: Color {
        if registered { return Theme.accent }
        if connecting { return Theme.warning }
        return Theme.idle
    }

    // MARK: - SIPClientDelegate

    func sipClientDidRegister(_ client: SIPClient) {
        DispatchQueue.main.async {
            self.registered = true
            self.connecting = false
            self.statusLine = "已注册"
            self.refreshGatewayStatus()
            // Sync history from gateway on successful registration
            self.history.fullSync(host: self.serverHost, port: self.apiPort)
        }
    }

    func sipClient(_ client: SIPClient, didLoseRegistration reason: String) {
        DispatchQueue.main.async {
            self.registered = false
            self.connecting = false
            self.statusLine = "连接断开：\(reason)"
            // Auto-reconnect after a delay
            if self.autoConnect {
                DispatchQueue.main.asyncAfter(deadline: .now() + 3) { [weak self] in
                    guard let self, !self.registered, self.autoConnect else { return }
                    self.connect()
                }
            }
        }
    }

    func sipClient(_ client: SIPClient, didReceiveIncomingCall uuid: UUID, number: String) {
        DispatchQueue.main.async {
            self.activeCallUUID = uuid
            self.activeCallNumber = number
        }
        history.addCall(direction: "in", number: number, status: "ringing")
        calls.reportIncomingCall(uuid: uuid, number: number)
    }

    func sipClient(_ client: SIPClient, didActivateAudio uuid: UUID) {
        calls.ensureAudioStarted(reason: "call-active")
    }

    func sipClient(_ client: SIPClient, didEndCall uuid: UUID, reason: String) {
        DispatchQueue.main.async {
            if self.activeCallUUID == uuid {
                self.activeCallUUID = nil
                self.activeCallNumber = nil
            }
        }
        calls.endCall(uuid: uuid, reason: .remoteEnded)
    }

    func sipClient(_ client: SIPClient, didReceiveMessage from: String, body: String) {
        DispatchQueue.main.async {
            self.sms.received(from: from, body: body)
            self.history.addSMS(direction: "in", peer: from, body: body, status: "received")
        }
    }
    
    // MARK: - Outgoing actions with history tracking
    
    func placeCall(number: String) {
        history.addCall(direction: "out", number: number, status: "dialing")
        calls.startOutgoingCall(number: number)
    }
    
    func sendSMS(to: String, body: String) {
        sms.send(to: to, body: body)
        history.addSMS(direction: "out", peer: to, body: body, status: "sent")
    }
}
