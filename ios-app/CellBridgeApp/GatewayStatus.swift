import Foundation

/// Mirrors the gateway's `GET /api/v1/status` payload (see docs/PROTOCOL.md §5).
/// Used by the Status tab so the phone can show what the modem is doing without
/// having to look at the Mac.
struct GatewayStatus: Equatable {
    var modem: String = "?"
    var registered: Bool = false
    var signal: Int = 99
    var activeCalls: Int = 0

    /// RSSI 0–31 as reported by AT+CSQ; 99 means "unknown".
    var signalPercent: Int {
        guard signal != 99, signal > 0 else { return 0 }
        return min(100, Int(round(Double(signal) / 31.0 * 100)))
    }

    var signalText: String { signal == 99 ? "未知" : "\(signal)/31" }
}

/// Minimal client for the gateway's HTTP API. Only the status endpoint is used.
final class GatewayClient {
    private let session: URLSession

    init() {
        let cfg = URLSessionConfiguration.ephemeral
        cfg.timeoutIntervalForRequest = 4
        cfg.waitsForConnectivity = false
        session = URLSession(configuration: cfg)
    }

    func fetchStatus(host: String, port: Int, completion: @escaping (GatewayStatus?) -> Void) {
        guard !host.isEmpty, let url = URL(string: "http://\(host):\(port)/api/v1/status") else {
            completion(nil)
            return
        }
        session.dataTask(with: url) { data, _, _ in
            guard let data,
                  let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any]
            else {
                completion(nil)
                return
            }
            var s = GatewayStatus()
            if let m = obj["modem"] as? String { s.modem = m }
            if let r = obj["registered"] as? Bool { s.registered = r }
            if let sig = obj["signal"] as? Int { s.signal = sig }
            if let calls = obj["calls"] as? [Any] { s.activeCalls = calls.count }
            completion(s)
        }.resume()
    }
}
