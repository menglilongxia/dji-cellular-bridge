import Foundation
import Network
import CryptoKit

enum CallDirection { case incoming, outgoing }
enum CallState { case ringing, active, ended }

struct PhoneCall {
    var uuid: UUID
    var direction: CallDirection
    var number: String
    var state: CallState
    var sipCallID: String
    var localTag: String
    var remoteTag: String
    var cseq: Int
    var invite: SIPMessage?   // incoming: original INVITE (to build 200 OK)
    var ok200: SIPMessage?    // last 200 OK sent (duplicate INVITE -> resend)
    var rtp: RTPSession?
    var remoteSDP: SDPInfo?
}

protocol SIPClientDelegate: AnyObject {
    func sipClientDidRegister(_ client: SIPClient)
    func sipClient(_ client: SIPClient, didLoseRegistration reason: String)
    func sipClient(_ client: SIPClient, didReceiveIncomingCall uuid: UUID, number: String)
    func sipClient(_ client: SIPClient, didActivateAudio uuid: UUID)
    func sipClient(_ client: SIPClient, didEndCall uuid: UUID, reason: String)
    func sipClient(_ client: SIPClient, didReceiveMessage from: String, body: String)
}

/// Minimal SIP user agent over UDP. Implements the same profile as the Go
/// gateway (docs/PROTOCOL.md): REGISTER + digest (no qop), INVITE/BYE/ACK,
/// MESSAGE, PCMU/8000 SDP.
final class SIPClient {
    weak var delegate: SIPClientDelegate?
    /// Incoming RTP PCM (8kHz mono) -> CallManager -> AudioBridge.
    var onAudioFrame: (([Int16]) -> Void)?
    /// Free-form diagnostics forwarded to the gateway, so a silent call on the
    /// phone side is visible from the Mac.
    var onDiagnostic: ((String) -> Void)?

    var serverHost = ""
    var serverPort = 5060
    var username = "iphone"
    var password = "changeme"
    var pushToken: String?
    private(set) var registered = false

    private var conn: NWConnection?
    private let queue = DispatchQueue(label: "sip-client")
    private var realm = "dji-cellular-bridge"
    private var nonce = ""
    private let regCallID = UUID().uuidString + "@iphone"
    private var regCSeq = 0
    private var regTimer: DispatchSourceTimer?
    private var calls: [UUID: PhoneCall] = [:]
    private var byCallID: [String: UUID] = [:]
    private var nextRTPPort: UInt16 = 50000

    /// The media socket, created once in the foreground and reused for every
    /// call.
    ///
    /// iOS denies local-network operations from a background app without any
    /// error (TN3179): a UDP socket created *during* a call — when CallKit owns
    /// the screen and this app is in the background — sends happily into the
    /// void and receives nothing. The SIP socket escapes that only because it
    /// is created when the user taps Connect. So is this one now.
    private var media: RTPSession?
    static let mediaLocalPort: UInt16 = 50000
    static let mediaRemotePort: UInt16 = 40000

    /// Network path monitor: detects WiFi<->cellular transitions and forces
    /// a full reconnect (SIP + media) so that sockets are re-bound to the new
    /// interface. Without this, switching from WiFi to 4G makes every UDP send
    /// silently go nowhere.
    private var pathMonitor: NWPathMonitor?
    private var lastPathIsExpensive: Bool?
    private var needsReconnect = false
    private var reconnectHost = ""
    private var reconnectPort = 0

    /// Fixed local SIP port. docs/PROTOCOL.md requires the app to publish a real
    /// port in its Contact so the gateway can call back with an INVITE; with an
    /// OS-assigned ephemeral port the gateway sent INVITEs to 5060 and they were
    /// never delivered (that was the "打不进来" bug).
    let localPort: UInt16 = 5060

    // MARK: - transport

    func connect(host: String, port: Int = 5060) {
        disconnect()
        serverHost = host; serverPort = port
        let params = NWParameters.udp
        params.requiredLocalEndpoint = NWEndpoint.hostPort(
            host: .ipv4(.any), port: NWEndpoint.Port(integerLiteral: localPort))
        conn = NWConnection(host: NWEndpoint.Host(host),
                            port: NWEndpoint.Port(integerLiteral: UInt16(port)), using: params)
        NSLog("SIP connect: %@:%d localPort=%d", host, port, Int(localPort))
        conn?.stateUpdateHandler = { [weak self] state in
            guard let self else { return }
            switch state {
            case .ready: self.register()
            case .failed(let e):
                self.registered = false
                self.delegate?.sipClient(self, didLoseRegistration: e.localizedDescription)
            default: break
            }
        }
        conn?.start(queue: queue)
        receive()
        // Open the media socket now, while the app is in the foreground.
        prepareMedia(host: host)
        // Watch for network path changes (WiFi <-> cellular). When the path
        // changes, every existing UDP socket is dead — iOS does not migrate
        // them. We must tear down and rebuild both SIP and media.
        startPathMonitor(host: host, port: port)
    }

    /// Creates the persistent media socket if it is missing. Must run in the
    /// foreground — see the `media` comment.
    ///
    /// It deliberately does *not* recreate a live socket: rebinding the same
    /// local port while the old socket still holds it fails with EADDRINUSE,
    /// and the connection then sits in `.waiting` with every send going
    /// nowhere — a silent call in both directions.
    func prepareMedia(host: String) {
        if media != nil {
            return
        }
        rebuildMedia(host: host)
    }

    private func rebuildMedia(host: String) {
        media?.stop()
        let m = RTPSession()
        m.onPayload = { [weak self] pcm in self?.onAudioFrame?(pcm) }
        m.onDiagnostic = { [weak self] message in self?.onDiagnostic?(message) }
        m.onFailed = { [weak self] in self?.media = nil }
        m.prepare(localPort: Self.mediaLocalPort)
        m.connect(remoteHost: host, remotePort: Self.mediaRemotePort)
        media = m
    }

    /// The local port our media socket actually owns; what the SDP advertises.
    var mediaLocalPortInUse: UInt16 {
        if let p = media?.boundPort, p != 0 { return p }
        return Self.mediaLocalPort
    }

    private func startPathMonitor(host: String, port: Int) {
        pathMonitor?.cancel()
        let monitor = NWPathMonitor()
        lastPathIsExpensive = nil
        monitor.pathUpdateHandler = { [weak self] path in
            guard let self else { return }
            let expensive = path.isExpensive  // true = cellular, false = wifi
            let satisfied = path.status == .satisfied
            NSLog("SIP: network path changed: satisfied=\(satisfied) expensive=\(expensive)")
            // Skip the first callback (initial state) and only act on real changes
            if let last = self.lastPathIsExpensive, last != expensive, satisfied {
                // During an active call, do NOT reconnect — tearing down the
                // audio session causes an instant crash (installTap on a dead
                // engine). Instead, just flag that a reconnect is needed and
                // do it when the call ends or next time prepareMedia is called.
                if self.calls.values.contains(where: { $0.state != .ended }) {
                    NSLog("SIP: interface changed during call, deferring reconnect")
                    self.needsReconnect = true
                    self.reconnectHost = host
                    self.reconnectPort = port
                } else {
                    NSLog("SIP: interface changed (expensive \(last)->\(expensive)), reconnecting")
                    self.queue.async {
                        self.forceReconnect(host: host, port: port)
                    }
                }
            }
            self.lastPathIsExpensive = expensive
        }
        monitor.start(queue: queue)
        pathMonitor = monitor
    }

    /// Tears down all sockets and reconnects from scratch on the new interface.
    private func forceReconnect(host: String, port: Int) {
        // Kill SIP connection
        conn?.cancel()
        conn = nil
        registered = false
        // Kill media socket — it is bound to the old interface
        media?.stop()
        media = nil
        // Rebuild everything
        let params = NWParameters.udp
        params.requiredLocalEndpoint = NWEndpoint.hostPort(
            host: .ipv4(.any), port: NWEndpoint.Port(integerLiteral: localPort))
        conn = NWConnection(host: NWEndpoint.Host(host),
                            port: NWEndpoint.Port(integerLiteral: UInt16(port)), using: params)
        conn?.stateUpdateHandler = { [weak self] state in
            guard let self else { return }
            switch state {
            case .ready: self.register()
            case .failed(let e):
                self.registered = false
                self.delegate?.sipClient(self, didLoseRegistration: e.localizedDescription)
            default: break
            }
        }
        conn?.start(queue: queue)
        receive()
        rebuildMedia(host: host)
        NSLog("SIP: reconnected on new network path")
    }

    func disconnect() {
        pathMonitor?.cancel(); pathMonitor = nil
        regTimer?.cancel(); regTimer = nil
        conn?.cancel(); conn = nil
        media?.stop(); media = nil
        registered = false
    }

    private func receive() {
        conn?.receiveMessage { [weak self] data, _, _, _ in
            if let data, let text = String(data: data, encoding: .utf8),
               let msg = SIPMessage.parse(text) {
                // Visible via `pymobiledevice3 syslog live` — the only way to see
                // what actually reaches the app when a call fails to ring.
                let firstLine = text.split(separator: "\r\n", maxSplits: 1).first.map(String.init) ?? ""
                if !firstLine.hasPrefix("OPTIONS") && !firstLine.hasPrefix("SIP/2.0 401") {
                    NSLog("SIP recv <<< %@", firstLine)
                }
                self?.handle(msg)
            }
            self?.receive()
        }
    }

    private func send(_ m: SIPMessage) {
        guard let data = m.serialize().data(using: .utf8) else { return }
        conn?.send(content: data, completion: .contentProcessed { _ in })
    }

    // MARK: - auth

    private func md5(_ s: String) -> String {
        Insecure.MD5.hash(data: Data(s.utf8)).map { String(format: "%02x", $0) }.joined()
    }

    private func authValue(method: String, uri: String) -> String {
        let ha1 = md5("\(username):\(realm):\(password)")
        let ha2 = md5("\(method):\(uri)")
        let resp = md5("\(ha1):\(nonce):\(ha2)")
        return "Digest username=\"\(username)\", realm=\"\(realm)\", nonce=\"\(nonce)\", " +
            "uri=\"\(uri)\", response=\"\(resp)\", algorithm=MD5"
    }

    private func parseNonce(_ wwwAuth: String) {
        for part in wwwAuth.split(separator: ",") {
            let p = part.trimmingCharacters(in: .whitespaces)
            if p.hasPrefix("realm=\""), let v = quoted(p) { realm = v }
            if p.hasPrefix("nonce=\""), let v = quoted(p) { nonce = v }
        }
    }

    private func quoted(_ s: String) -> String? {
        guard let a = s.firstIndex(of: "\""), let b = s.lastIndex(of: "\""), a != b else { return nil }
        return String(s[s.index(after: a)..<b])
    }

    // MARK: - register

    func register() {
        regCSeq += 1
        var r = SIPMessage(isRequest: true, method: "REGISTER", requestURI: "sip:\(serverHost)")
        r.setHeader("Via", "SIP/2.0/UDP \(localIP()):\(localPort);branch=z9hG4bK\(randHex(12))")
        r.setHeader("From", "<sip:\(username)@\(serverHost)>;tag=\(randHex(8))")
        r.setHeader("To", "<sip:\(username)@\(serverHost)>")
        r.setHeader("Call-ID", regCallID)
        r.setHeader("CSeq", "\(regCSeq) REGISTER")
        r.setHeader("Contact", "<sip:\(username)@\(localIP()):\(localPort)>")
        r.setHeader("Expires", "300")
        if let t = pushToken { r.setHeader("X-Push-Token", t) }
        if !nonce.isEmpty { r.setHeader("Authorization", authValue(method: "REGISTER", uri: "sip:\(serverHost)")) }
        send(r)
    }

    private func scheduleRefresh() {
        regTimer?.cancel()
        let t = DispatchSource.makeTimerSource(queue: queue)
        t.schedule(deadline: .now() + 250)
        t.setEventHandler { [weak self] in self?.register() }
        t.resume()
        regTimer = t
    }

    // MARK: - incoming dispatch

    private func handle(_ m: SIPMessage) {
        if m.isRequest {
            switch m.method {
            case "INVITE": handleInvite(m)
            case "ACK": handleAck(m)
            case "BYE": handleBye(m)
            case "CANCEL": handleCancel(m)
            case "MESSAGE": handleIncomingMessage(m)
            case "OPTIONS": respond(to: m, 200, "OK", nil)
            default: respond(to: m, 405, "Method Not Allowed", nil)
            }
        } else {
            handleResponse(m)
        }
    }

    private func respond(to req: SIPMessage, _ code: Int, _ reason: String, _ build: ((inout SIPMessage) -> Void)?) {
        var r = SIPMessage(isRequest: false, statusCode: code, reason: reason)
        r.setHeader("Via", req.header("Via") ?? "")
        r.setHeader("From", req.header("From") ?? "")
        r.setHeader("To", req.header("To") ?? "")
        r.setHeader("Call-ID", req.header("Call-ID") ?? "")
        r.setHeader("CSeq", req.header("CSeq") ?? "")
        build?(&r)
        send(r)
    }

    private func handleResponse(_ m: SIPMessage) {
        let callID = m.header("Call-ID") ?? ""
        if callID == regCallID {
            switch m.statusCode {
            case 401:
                parseNonce(m.header("WWW-Authenticate") ?? "")
                register()
            case 200:
                registered = true
                scheduleRefresh()
                delegate?.sipClientDidRegister(self)
            default: break
            }
            return
        }
        guard let uuid = byCallID[callID], var call = calls[uuid] else { return }
        switch m.statusCode {
        case 100, 180, 183: break // ringing feedback; CallKit already shows "connecting"
        case 200 where m.header("CSeq")?.hasSuffix("INVITE") == true:
            // Outgoing call answered.
            if let sdp = SDPInfo.parse(m.body) {
                call.remoteSDP = sdp
                // clamping: the port comes from remote SDP text, and a plain
                // UInt16(...) would trap on a malformed value.
                call.rtp?.connect(remoteHost: sdp.ip, remotePort: UInt16(clamping: sdp.port))
            }
            call.remoteTag = SIPMessage.tag(of: m.header("To") ?? "")
            sendAck(for: &call, to: m)
            call.state = .active
            calls[uuid] = call
            delegate?.sipClient(self, didActivateAudio: uuid)
        case 486, 603, 480, 487:
            endCall(uuid, reason: "rejected \(m.statusCode)")
        default: break
        }
    }

    private func sendAck(for call: inout PhoneCall, to ok: SIPMessage) {
        var ack = SIPMessage(isRequest: true, method: "ACK", requestURI: ok.header("Contact")?.trimmingCharacters(in: CharacterSet(charactersIn: "<> ")) ?? "")
        if ack.requestURI.isEmpty { ack.requestURI = "sip:\(call.number)@\(serverHost)" }
        ack.setHeader("Via", "SIP/2.0/UDP \(localIP()):\(localPort);branch=z9hG4bK\(randHex(12))")
        ack.setHeader("From", ok.header("From") ?? "")
        var to = ok.header("To") ?? ""
        if !to.contains("tag=") { to += ";tag=\(call.remoteTag)" }
        ack.setHeader("To", to)
        ack.setHeader("Call-ID", call.sipCallID)
        ack.setHeader("CSeq", "\(call.cseq) ACK")
        send(ack)
    }

    // MARK: - incoming call (we are UAS)

    private func handleInvite(_ m: SIPMessage) {
        let callID = m.header("Call-ID") ?? ""
        NSLog("SIP INVITE: callID=%@ from=%@", callID, m.header("From") ?? "?")
        if let uuid = byCallID[callID], let call = calls[uuid] {
            // Duplicate INVITE (server retransmit): resend last response.
            if call.state == .ringing {
                respond(to: m, 180, "Ringing") { $0.setHeader("To", ($0.header("To") ?? "") + ";tag=\(call.localTag)") }
            } else if let ok = call.ok200 {
                send(ok)
            }
            return
        }
        let number = SIPMessage.user(of: m.header("From") ?? "")
        let uuid = UUID()
        let call = PhoneCall(uuid: uuid, direction: .incoming, number: number, state: .ringing,
                             sipCallID: callID, localTag: randHex(8),
                             remoteTag: SIPMessage.tag(of: m.header("From") ?? ""),
                             cseq: 1, invite: m, ok200: nil, rtp: nil,
                             remoteSDP: SDPInfo.parse(m.body))
        calls[uuid] = call
        byCallID[callID] = uuid
        respond(to: m, 100, "Trying", nil)
        respond(to: m, 180, "Ringing") { [call] r in
            r.setHeader("To", (m.header("To") ?? "") + ";tag=\(call.localTag)")
            r.setHeader("Contact", "<sip:\(self.username)@\(self.localIP()):\(self.localPort)>")
        }
        delegate?.sipClient(self, didReceiveIncomingCall: uuid, number: number)
    }

    private func handleAck(_ m: SIPMessage) {
        let callID = m.header("Call-ID") ?? ""
        guard let uuid = byCallID[callID], var call = calls[uuid],
              call.direction == .incoming, call.state == .ringing else { return }
        call.state = .active
        calls[uuid] = call
        // The RTP sender only runs while a call is `.active`, so a missing ACK
        // silently means "the phone never transmits".
        onDiagnostic?("call active: ACK received")
        delegate?.sipClient(self, didActivateAudio: uuid)
    }

    /// Answer an incoming call (from CallKit).
    func answer(uuid: UUID) {
        guard var call = calls[uuid], call.state == .ringing, let inv = call.invite,
              let remote = call.remoteSDP else { return }
        // Reuse the foreground-created media socket instead of opening a new
        // one now (the app is in the background during a call).
        let port = mediaLocalPortInUse
        var ok = SIPMessage(isRequest: false, statusCode: 200, reason: "OK")
        ok.setHeader("Via", inv.header("Via") ?? "")
        ok.setHeader("From", inv.header("From") ?? "")
        ok.setHeader("To", (inv.header("To") ?? "") + ";tag=\(call.localTag)")
        ok.setHeader("Call-ID", call.sipCallID)
        ok.setHeader("CSeq", inv.header("CSeq") ?? "")
        ok.setHeader("Contact", "<sip:\(username)@\(localIP()):\(localPort)>")
        ok.setHeader("Content-Type", "application/sdp")
        ok.body = SDPInfo.build(ip: localIP(), port: Int(port))
        call.ok200 = ok
        call.rtp = media
        calls[uuid] = call
        onDiagnostic?("rtp ready: local \(port) -> \(remote.ip):\(remote.port)")
        send(ok)
    }

    /// Decline a ringing incoming call.
    func decline(uuid: UUID) {
        guard let call = calls[uuid], call.state == .ringing, let inv = call.invite else { return }
        respond(to: inv, 486, "Busy Here") { $0.setHeader("To", ($0.header("To") ?? "") + ";tag=\(call.localTag)") }
        endCall(uuid, reason: "declined")
    }

    private func handleBye(_ m: SIPMessage) {
        respond(to: m, 200, "OK", nil)
        let callID = m.header("Call-ID") ?? ""
        if let uuid = byCallID[callID] { endCall(uuid, reason: "remote-bye") }
    }

    private func handleCancel(_ m: SIPMessage) {
        respond(to: m, 200, "OK", nil)
        let callID = m.header("Call-ID") ?? ""
        guard let uuid = byCallID[callID], let call = calls[uuid], call.state == .ringing else { return }
        if let inv = call.invite {
            respond(to: inv, 487, "Request Terminated") {
                $0.setHeader("To", ($0.header("To") ?? "") + ";tag=\(call.localTag)")
            }
        }
        endCall(uuid, reason: "cancelled")
    }

    // MARK: - outgoing call (we are UAC)

    /// Dial. Returns the CallKit UUID.
    func call(number: String) -> UUID {
        let uuid = UUID()
        let callID = UUID().uuidString + "@iphone"
        let tag = randHex(8)
        // Same rule as for incoming calls: reuse the foreground-created socket.
        let port = mediaLocalPortInUse
        var inv = SIPMessage(isRequest: true, method: "INVITE", requestURI: "sip:\(number)@\(serverHost)")
        inv.setHeader("Via", "SIP/2.0/UDP \(localIP()):\(localPort);branch=z9hG4bK\(randHex(12))")
        inv.setHeader("From", "<sip:\(username)@\(serverHost)>;tag=\(tag)")
        inv.setHeader("To", "<sip:\(number)@\(serverHost)>")
        inv.setHeader("Call-ID", callID)
        inv.setHeader("CSeq", "1 INVITE")
        inv.setHeader("Contact", "<sip:\(username)@\(localIP()):\(localPort)>")
        inv.setHeader("Content-Type", "application/sdp")
        inv.body = SDPInfo.build(ip: localIP(), port: Int(port))
        calls[uuid] = PhoneCall(uuid: uuid, direction: .outgoing, number: number, state: .ringing,
                                sipCallID: callID, localTag: tag, remoteTag: "", cseq: 1,
                                invite: nil, ok200: nil, rtp: media, remoteSDP: nil)
        byCallID[callID] = uuid
        send(inv)
        return uuid
    }

    /// Hang up an active call (sends BYE).
    func hangup(uuid: UUID) {
        guard let call = calls[uuid], call.state != .ended else { return }
        var bye = SIPMessage(isRequest: true, method: "BYE", requestURI: "sip:\(call.number)@\(serverHost)")
        bye.setHeader("Via", "SIP/2.0/UDP \(localIP()):\(localPort);branch=z9hG4bK\(randHex(12))")
        bye.setHeader("From", "<sip:\(username)@\(serverHost)>;tag=\(call.localTag)")
        bye.setHeader("To", "<sip:\(call.number)@\(serverHost)>;tag=\(call.remoteTag)")
        bye.setHeader("Call-ID", call.sipCallID)
        bye.setHeader("CSeq", "\(call.cseq + 1) BYE")
        send(bye)
        endCall(uuid, reason: "local-hangup")
    }

    private func endCall(_ uuid: UUID, reason: String) {
        guard var call = calls.removeValue(forKey: uuid) else { return }
        call.state = .ended
        // The media socket is shared and must survive the call: it can only be
        // created while this app is in the foreground.
        call.rtp = nil
        byCallID.removeValue(forKey: call.sipCallID)
        delegate?.sipClient(self, didEndCall: uuid, reason: reason)
        // If a network change happened during the call, reconnect now.
        if needsReconnect && !calls.values.contains(where: { $0.state != .ended }) {
            needsReconnect = false
            NSLog("SIP: deferred reconnect after call ended")
            queue.async { [self] in
                self.forceReconnect(host: self.reconnectHost, port: self.reconnectPort)
            }
        }
    }

    // MARK: - SMS

    func sendMessage(to: String, body: String) {
        let cleanTo = to.trimmingCharacters(in: .whitespacesAndNewlines)
        let cleanBody = body.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !cleanTo.isEmpty, !cleanBody.isEmpty, registered else { return }
        var m = SIPMessage(isRequest: true, method: "MESSAGE", requestURI: "sip:\(cleanTo)@\(serverHost)")
        m.setHeader("Via", "SIP/2.0/UDP \(localIP()):\(localPort);branch=z9hG4bK\(randHex(12))")
        m.setHeader("From", "<sip:\(username)@\(serverHost)>;tag=\(randHex(8))")
        m.setHeader("To", "<sip:\(cleanTo)@\(serverHost)>")
        m.setHeader("Call-ID", UUID().uuidString + "@iphone")
        m.setHeader("CSeq", "1 MESSAGE")
        m.setHeader("Content-Type", "text/plain; charset=utf-8")
        m.body = cleanBody
        send(m)
    }

    private func handleIncomingMessage(_ m: SIPMessage) {
        respond(to: m, 200, "OK", nil)
        let from = SIPMessage.user(of: m.header("From") ?? "")
        delegate?.sipClient(self, didReceiveMessage: from, body: m.body)
    }

    // MARK: - audio pump

    /// Mic PCM (160 samples @8kHz) -> active call's RTP.
    func sendAudio(_ pcm: [Int16]) {
        for call in calls.values where call.state == .active {
            call.rtp?.send(pcm: pcm)
        }
    }

    /// Fire-and-forget UDP reachability probe; the result arrives through
    /// onDiagnostic and is forwarded to the gateway.
    func probeUDPEcho() {
        RTPSession.echoTest(host: serverHost) { [weak self] message in
            self?.onDiagnostic?(message)
        }
    }

    // MARK: - helpers

    private func allocRTPPort() -> UInt16 {
        let p = nextRTPPort
        nextRTPPort &+= 2
        if nextRTPPort > 60000 { nextRTPPort = 50000 }
        return p
    }

    private func randHex(_ n: Int) -> String {
        (0..<n).map { _ in "0123456789abcdef".randomElement()! }.map(String.init).joined()
    }

    private func localIP() -> String { Self.localIPAddress() }

    /// Returns the best local IPv4 address for SIP/RTP.
    /// Priority: Tailscale 100.x (works on LAN and remotely) > Wi-Fi en0 > any.
    static func localIPAddress() -> String {
        var tailscale = ""
        var wifi = ""
        var fallback = ""
        var ifaddr: UnsafeMutablePointer<ifaddrs>?
        guard getifaddrs(&ifaddr) == 0, let first = ifaddr else { return "127.0.0.1" }
        defer { freeifaddrs(ifaddr) }
        var ptr: UnsafeMutablePointer<ifaddrs>? = first
        while let p = ptr {
            let flags = Int32(p.pointee.ifa_flags)
            if (flags & IFF_UP) != 0, (flags & IFF_LOOPBACK) == 0, let sa = p.pointee.ifa_addr,
               sa.pointee.sa_family == UInt8(AF_INET) {
                var host = [CChar](repeating: 0, count: Int(NI_MAXHOST))
                let len = socklen_t(sa.pointee.sa_len)
                if getnameinfo(sa, len, &host, socklen_t(host.count), nil, 0, NI_NUMERICHOST) == 0 {
                    let ip = String(cString: host)
                    let ifname = String(cString: p.pointee.ifa_name)
                    // Skip self-assigned and iCloud Private Relay addresses.
                    if ip.hasPrefix("169.254.") || ip.hasPrefix("192.0.0.") {
                        ptr = p.pointee.ifa_next
                        continue
                    }
                    // Tailscale CGNAT range: 100.64.0.0/10
                    if ip.hasPrefix("100.") {
                        tailscale = ip
                    } else if ifname == "en0" {
                        wifi = ip
                    } else if fallback.isEmpty {
                        fallback = ip
                    }
                }
            }
            ptr = p.pointee.ifa_next
        }
        if !tailscale.isEmpty { return tailscale }
        if !wifi.isEmpty { return wifi }
        if !fallback.isEmpty { return fallback }
        return "127.0.0.1"
    }
}
