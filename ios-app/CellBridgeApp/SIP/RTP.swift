import Foundation
import Network

/// RTP framing (RFC 3550) + G.711 mu-law. Mirrors Go internal/rtp.
enum RTP {
    static func packet(seq: UInt16, timestamp: UInt32, ssrc: UInt32, payload: Data) -> Data {
        var b = Data(count: 12 + payload.count)
        b[0] = 0x80
        b[1] = 0 // PT=0 PCMU
        // Every field is big-endian. Each UInt8(...) conversion must be masked:
        // `timestamp >> 16` is up to 65535 and UInt8() traps on anything > 255.
        b[2] = UInt8((seq >> 8) & 0xFF); b[3] = UInt8(seq & 0xFF)
        b[4] = UInt8((timestamp >> 24) & 0xFF); b[5] = UInt8((timestamp >> 16) & 0xFF)
        b[6] = UInt8((timestamp >> 8) & 0xFF); b[7] = UInt8(timestamp & 0xFF)
        b[8] = UInt8((ssrc >> 24) & 0xFF); b[9] = UInt8((ssrc >> 16) & 0xFF)
        b[10] = UInt8((ssrc >> 8) & 0xFF); b[11] = UInt8(ssrc & 0xFF)
        b.replaceSubrange(12..., with: payload)
        return b
    }

    static func parse(_ data: Data) -> (seq: UInt16, timestamp: UInt32, payload: Data)? {
        guard data.count >= 12, data[0] >> 6 == 2 else { return nil }
        let seq = UInt16(data[2]) << 8 | UInt16(data[3])
        let ts = UInt32(data[4]) << 24 | UInt32(data[5]) << 16 | UInt32(data[6]) << 8 | UInt32(data[7])
        return (seq, ts, data.dropFirst(12))
    }

    static func linearToUlaw(_ s: Int16) -> UInt8 {
        let bias = 0x84, clip = 32635
        var sign = 0, v = Int(s)
        if v < 0 { v = -v; sign = 0x80 }
        if v > clip { v = clip }
        v += bias
        var exp = 0
        var tmp = v >> 7
        while tmp > 1 { tmp >>= 1; exp += 1 }
        let mantissa = (v >> (exp + 3)) & 0x0F
        // G.711 µ-law: the sign/exponent/mantissa byte is transmitted inverted.
        return UInt8(~(sign | (exp << 4) | mantissa) & 0xFF)
    }

    static func ulawToLinear(_ u: UInt8) -> Int16 {
        let v = Int(~u) & 0xFF
        let sign = v & 0x80, exp = (v >> 4) & 0x07, mantissa = v & 0x0F
        var sample = ((mantissa << 3) + 0x84) << exp
        sample -= 0x84
        return Int16(sign != 0 ? -sample : sample)
    }

    static func encode(_ pcm: [Int16]) -> Data {
        Data(pcm.map { linearToUlaw($0) })
    }

    static func decode(_ payload: Data) -> [Int16] {
        payload.map { ulawToLinear($0) }
    }
}

/// One RTP endpoint: sends mic PCMU to the gateway, delivers received
/// PCMU to the audio bridge. 20ms frames, 8kHz.
final class RTPSession {
    var onPayload: (([Int16]) -> Void)?
    /// Diagnostics forwarded to the gateway: a silent call needs to be traceable
    /// to "the phone never sent" vs "the phone sent and nobody heard it".
    var onDiagnostic: ((String) -> Void)?

    private var sender: NWConnection?
    private let queue = DispatchQueue(label: "rtp")
    private var seq: UInt16 = UInt16.random(in: 0...UInt16.max)
    private var timestamp: UInt32 = UInt32.random(in: 0...UInt32.max)
    private let ssrc: UInt32 = UInt32.random(in: 0...UInt32.max)
    private var running = false
    private var sentPackets = 0
    private var sendErrors = 0
    private var reportedFirstSend = false
    private var localPort: UInt16 = 0
    private var receivedPackets = 0

    /// The local port the media socket actually got. The SDP must advertise
    /// this one, and it is not always the one we asked for: iOS may have the
    /// preferred port in use (our own previous socket, for instance), and a
    /// connection that cannot bind simply sits in `.waiting` while every send
    /// goes nowhere.
    private(set) var boundPort: UInt16 = 0
    private var candidatePorts: [UInt16] = []

    /// Called once the socket is ready and `boundPort` is known.
    var onReady: (() -> Void)?
    var onFailed: (() -> Void)?

    /// Bind the local UDP port (advertised in SDP). Receiving starts now.
    func prepare(localPort: UInt16) {
        stop()
        running = true
        self.localPort = localPort
        candidatePorts = stride(from: localPort, through: localPort &+ 20, by: 2).map { $0 }
    }

    /// Connect the sender to the remote RTP endpoint (from remote SDP).
    /// If a sender already exists, never rebuild it. iOS TN3179 silently
    /// kills UDP sockets created while the app is in the background (which
    /// is always the case during a CallKit call). The socket was created in
    /// the foreground by prepareMedia() and must survive all calls.
    func connect(remoteHost: String, remotePort: UInt16) {
        if sender != nil {
            onDiagnostic?("rtp: reusing existing connection (requested \(remoteHost):\(remotePort), have \(remoteHostName):\(remotePortNumber))")
            return
        }
        remoteHostName = remoteHost
        remotePortNumber = remotePort
        // Mirror the SIP transport, which is the only UDP path proven to work
        // on this phone: one NWConnection, bound to the port we advertise in
        // SDP, forced to IPv4. The old shape (a NWListener plus a second,
        // unbound NWConnection for sending) accepted every send and put the
        // datagrams nowhere — the Mac's interface counters never saw them.
        let params = NWParameters.udp
        // Pin the media path to Wi-Fi. With iCloud Private Relay (or any VPN)
        // active, a fresh UDP socket can end up on the tunnel interface: sends
        // report success, the peer's packets never arrive, and both directions
        // of the call are silent while SIP (opened earlier, on Wi-Fi) still
        // works.
        params.requiredInterfaceType = .wifi
        let wanted = candidatePorts.first ?? localPort
        if wanted != 0 {
            params.requiredLocalEndpoint = NWEndpoint.hostPort(
                host: .ipv4(.any), port: NWEndpoint.Port(integerLiteral: wanted))
        }
        sender = NWConnection(host: NWEndpoint.Host(remoteHost),
                              port: NWEndpoint.Port(integerLiteral: remotePort), using: params)
        sender?.stateUpdateHandler = { [weak self] state in
            guard let self else { return }
            switch state {
            case .ready:
                self.boundPort = wanted
                self.onDiagnostic?("rtp sender ready -> \(remoteHost):\(remotePort) (local \(wanted))")
                self.onReady?()
            case .failed(let error):
                self.onDiagnostic?("rtp sender failed: \(error)")
                // Do NOT auto-reconnect here. iOS TN3179 will silently
                // kill any socket created in the background. The only safe
                // reconnect is when the user returns to the foreground.
                self.onFailed?()
            case .waiting(let error):
                // Port in use (or not assignable yet): take the next candidate
                // instead of sitting here with a dead media path.
                self.onDiagnostic?("rtp sender waiting: \(error)")
                if self.candidatePorts.count > 1 {
                    self.candidatePorts.removeFirst()
                    self.onDiagnostic?("rtp: retrying on local port \(self.candidatePorts[0])")
                    self.reconnect(remoteHost: remoteHost, remotePort: remotePort)
                }
            default:
                break
            }
        }
        sender?.start(queue: queue)
        if let sender { receive(on: sender) }
        // Report both directions: "we sent" is not "it arrived", and a silent
        // call has to be pinned to one of them.
        DispatchQueue.main.asyncAfter(deadline: .now() + 3) { [weak self] in
            guard let self, self.running else { return }
            self.onDiagnostic?("rtp stats: sent=\(self.sentPackets) errors=\(self.sendErrors) received=\(self.receivedPackets)")
        }
        DispatchQueue.main.asyncAfter(deadline: .now() + 9) { [weak self] in
            guard let self, self.running else { return }
            self.onDiagnostic?("rtp stats: sent=\(self.sentPackets) errors=\(self.sendErrors) received=\(self.receivedPackets)")
        }
    }

    private var remoteHostName = ""
    private var remotePortNumber: UInt16 = 0

    private func reconnect(remoteHost: String, remotePort: UInt16) {
        let keepRunning = running
        sender?.cancel()
        sender = nil
        running = keepRunning
        connect(remoteHost: remoteHost, remotePort: remotePort)
    }

    private func receive(on conn: NWConnection) {
        conn.receiveMessage { [weak self] data, _, _, _ in
            guard let self, self.running else { return }
            if let data, let (_, _, payload) = RTP.parse(data) {
                self.receivedPackets += 1
                self.onPayload?(RTP.decode(payload))
            }
            self.receive(on: conn)
        }
    }

    /// 160 samples (20ms @ 8kHz) per call.
    func send(pcm: [Int16]) {
        guard running, pcm.count == 160 else { return }
        let pkt = RTP.packet(seq: seq, timestamp: timestamp, ssrc: ssrc, payload: RTP.encode(pcm))
        seq &+= 1
        timestamp &+= 160
        sentPackets += 1
        sender?.send(content: pkt, completion: .contentProcessed { [weak self] error in
            guard let self else { return }
            if let error {
                self.sendErrors += 1
                if self.sendErrors <= 2 {
                    self.onDiagnostic?("rtp send error \(self.sendErrors): \(error)")
                }
            } else if !self.reportedFirstSend {
                self.reportedFirstSend = true
                self.onDiagnostic?("rtp first packet sent (\(pkt.count) bytes)")
            }
        })
    }

    func stop() {
        running = false
        sender?.cancel(); sender = nil
        localPort = 0
    }

    /// Sends a few datagrams to the gateway's UDP echo port and reports whether
    /// anything comes back. This separates "our media socket is broken" from
    /// "this phone cannot reach the gateway over UDP at all" (a VPN or Private
    /// Relay tunnel swallows the packets while every send reports success).
    static func echoTest(host: String, port: UInt16 = 40099,
                         report: @escaping (String) -> Void) {
        let params = NWParameters.udp
        params.requiredInterfaceType = .wifi
        let conn = NWConnection(host: NWEndpoint.Host(host),
                                port: NWEndpoint.Port(integerLiteral: port), using: params)
        var replies = 0
        var sends = 0
        conn.stateUpdateHandler = { state in
            if case .ready = state {
                for _ in 0..<3 {
                    conn.send(content: Data("CB-ECHO".utf8),
                              completion: .contentProcessed { _ in sends += 1 })
                }
            }
        }
        func pump() {
            conn.receiveMessage { data, _, _, error in
                if let data, String(data: data, encoding: .utf8) == "GWMAC" { replies += 1 }
                if error == nil { pump() }
            }
        }
        conn.start(queue: DispatchQueue(label: "rtp-echo"))
        pump()
        DispatchQueue.main.asyncAfter(deadline: .now() + 3) {
            report("udp echo: sent=\(sends) replies=\(replies)")
            conn.cancel()
        }
    }
}
