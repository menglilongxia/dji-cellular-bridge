import Foundation

/// Minimal SIP message (RFC 3261 subset). Mirrors the Go gateway's
/// internal/sip/message.go — both sides implement docs/PROTOCOL.md.
struct SIPMessage {
    var isRequest: Bool
    var method: String = ""
    var requestURI: String = ""
    var statusCode: Int = 0
    var reason: String = ""
    var headers: [(name: String, value: String)] = []
    var body: String = ""

    static func parse(_ text: String) -> SIPMessage? {
        guard let range = text.range(of: "\r\n\r\n") else { return nil }
        let head = String(text[..<range.lowerBound])
        let body = String(text[range.upperBound...])
        var lines = head.components(separatedBy: "\r\n")
        guard !lines.isEmpty else { return nil }
        let start = lines.removeFirst()
        var m = SIPMessage(isRequest: false)
        if start.hasPrefix("SIP/2.0") {
            let parts = start.split(separator: " ", maxSplits: 2).map(String.init)
            guard parts.count >= 2, let code = Int(parts[1]) else { return nil }
            m.statusCode = code
            m.reason = parts.count == 3 ? parts[2] : ""
        } else {
            let parts = start.split(separator: " ", maxSplits: 2).map(String.init)
            guard parts.count == 3 else { return nil }
            m.isRequest = true
            m.method = parts[0]
            m.requestURI = parts[1]
        }
        for line in lines where !line.isEmpty {
            guard let idx = line.firstIndex(of: ":") else { continue }
            let name = String(line[..<idx]).trimmingCharacters(in: .whitespaces)
            let value = String(line[line.index(after: idx)...]).trimmingCharacters(in: .whitespaces)
            m.headers.append((name, value))
        }
        m.body = body
        return m
    }

    func header(_ name: String) -> String? {
        headers.first { $0.name.lowercased() == name.lowercased() }?.value
    }

    mutating func setHeader(_ name: String, _ value: String) {
        if let i = headers.firstIndex(where: { $0.name.lowercased() == name.lowercased() }) {
            headers[i] = (headers[i].name, value)
        } else {
            headers.append((name, value))
        }
    }

    func serialize() -> String {
        var m = self
        var out = ""
        if m.isRequest {
            out += "\(m.method) \(m.requestURI) SIP/2.0\r\n"
        } else {
            out += "SIP/2.0 \(m.statusCode) \(m.reason)\r\n"
        }
        m.setHeader("Content-Length", "\(m.body.utf8.count)")
        for h in m.headers { out += "\(h.name): \(h.value)\r\n" }
        out += "\r\n" + m.body
        return out
    }

    /// user part of a sip: URI ("<sip:138@x>" -> "138").
    static func user(of uri: String) -> String {
        var s = uri.trimmingCharacters(in: CharacterSet(charactersIn: "<> "))
        if let r = s.range(of: "sip:") { s = String(s[r.upperBound...]) }
        if let i = s.firstIndex(of: "@") { s = String(s[..<i]) }
        if let i = s.firstIndex(of: ";") { s = String(s[..<i]) }
        return s.trimmingCharacters(in: .whitespaces)
    }

    static func tag(of headerValue: String) -> String {
        for part in headerValue.split(separator: ";") {
            let p = part.trimmingCharacters(in: .whitespaces)
            if p.hasPrefix("tag=") { return String(p.dropFirst(4)) }
        }
        return ""
    }
}

/// Minimal SDP: just the audio endpoint.
struct SDPInfo {
    var ip: String
    var port: Int

    static func parse(_ body: String) -> SDPInfo? {
        var ip = ""
        var port = 0
        for line in body.components(separatedBy: "\n") {
            let l = line.trimmingCharacters(in: .whitespaces)
            if l.hasPrefix("c=IN IP4 ") { ip = String(l.dropFirst(8)).trimmingCharacters(in: .whitespaces) }
            if l.hasPrefix("m=audio ") {
                let f = l.split(separator: " ")
                if f.count >= 2 { port = Int(f[1]) ?? 0 }
            }
        }
        guard port != 0 else { return nil }
        if ip.isEmpty || ip == "0.0.0.0" { ip = "127.0.0.1" }
        return SDPInfo(ip: ip, port: port)
    }

    static func build(ip: String, port: Int) -> String {
        let id = String(Int.random(in: 100000...999999))
        return "v=0\r\no=- \(id) \(id) IN IP4 \(ip)\r\ns=-\r\nc=IN IP4 \(ip)\r\nt=0 0\r\n" +
            "m=audio \(port) RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"
    }
}
