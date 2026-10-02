import Foundation

/// Persists call and SMS history locally and syncs new entries from the gateway
/// each time the app connects.
final class HistoryStore: ObservableObject {
    @Published var callRecords: [CallRecord] = []
    @Published var smsRecords: [SMSRecord] = []
    
    private let defaults = UserDefaults.standard
    private let callsKey = "cb.callHistory"
    private let smsKey = "cb.smsHistory"
    
    struct CallRecord: Identifiable, Codable, Equatable {
        var id: String
        var direction: String  // "in" | "out"
        var number: String
        var status: String     // "answered" | "missed" | "rejected" | "failed" | "ringing" | "dialing"
        var startedAt: String
        var endedAt: String?
        var duration: Int      // seconds
        
        enum CodingKeys: String, CodingKey {
            case id, direction, number, status
            case startedAt = "started_at"
            case endedAt = "ended_at"
            case duration
        }
        
        var isMissed: Bool { status == "missed" }
        var isIncoming: Bool { direction == "in" }
        
        var formattedDuration: String {
            guard duration > 0 else { return "" }
            let m = duration / 60
            let s = duration % 60
            return m > 0 ? "\(m)分\(s)秒" : "\(s)秒"
        }
        
        var formattedDate: String {
            guard let date = ISO8601DateFormatter().date(from: startedAt) else { return startedAt }
            let cal = Calendar.current
            let fmt = DateFormatter()
            if cal.isDateInToday(date) {
                fmt.dateFormat = "HH:mm"
            } else if cal.isDateInYesterday(date) {
                return "昨天"
            } else {
                fmt.dateFormat = "MM/dd"
            }
            return fmt.string(from: date)
        }
    }
    
    struct SMSRecord: Identifiable, Codable, Equatable {
        var id: String
        var direction: String  // "in" | "out"
        var peer: String
        var body: String
        var at: String
        var status: String     // "sent" | "failed" | "received"
        
        var isOutgoing: Bool { direction == "out" }
        
        var formattedDate: String {
            guard let date = ISO8601DateFormatter().date(from: at) else { return at }
            let cal = Calendar.current
            let fmt = DateFormatter()
            if cal.isDateInToday(date) {
                fmt.dateFormat = "HH:mm"
            } else if cal.isDateInYesterday(date) {
                return "昨天"
            } else {
                fmt.dateFormat = "MM/dd HH:mm"
            }
            return fmt.string(from: date)
        }
    }
    
    init() {
        loadLocal()
    }
    
    // MARK: - Local persistence
    
    private func loadLocal() {
        if let data = defaults.data(forKey: callsKey),
           let records = try? JSONDecoder().decode([CallRecord].self, from: data) {
            callRecords = records
        }
        if let data = defaults.data(forKey: smsKey),
           let records = try? JSONDecoder().decode([SMSRecord].self, from: data) {
            smsRecords = records
        }
    }
    
    private func saveLocal() {
        if let data = try? JSONEncoder().encode(callRecords) {
            defaults.set(data, forKey: callsKey)
        }
        if let data = try? JSONEncoder().encode(smsRecords) {
            defaults.set(data, forKey: smsKey)
        }
    }
    
    // MARK: - Local additions (from SIP events during this session)
    
    func addCall(direction: String, number: String, status: String) {
        let record = CallRecord(
            id: UUID().uuidString,
            direction: direction,
            number: number,
            status: status,
            startedAt: ISO8601DateFormatter().string(from: Date()),
            endedAt: nil,
            duration: 0
        )
        runOnMain {
            self.callRecords.append(record)
            self.saveLocal()
        }
    }
    
    func addSMS(direction: String, peer: String, body: String, status: String) {
        let record = SMSRecord(
            id: UUID().uuidString,
            direction: direction,
            peer: peer,
            body: body,
            at: ISO8601DateFormatter().string(from: Date()),
            status: status
        )
        runOnMain {
            self.smsRecords.append(record)
            self.saveLocal()
        }
    }
    
    // MARK: - Sync from gateway
    
    func syncFromGateway(host: String, port: Int) {
        guard !host.isEmpty else { return }
        
        // Use cursor-based sync: send the last known IDs
        let afterCall = callRecords.last?.id ?? ""
        let afterSMS = smsRecords.last?.id ?? ""
        
        var components = URLComponents(string: "http://\(host):\(port)/api/v1/history")
        components?.queryItems = [
            URLQueryItem(name: "after_call", value: afterCall),
            URLQueryItem(name: "after_sms", value: afterSMS),
        ]
        guard let url = components?.url else { return }
        
        var req = URLRequest(url: url)
        req.timeoutInterval = 10
        
        URLSession.shared.dataTask(with: req) { [weak self] data, _, error in
            guard let self, let data, error == nil,
                  let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any]
            else { return }
            
            DispatchQueue.main.async {
                self.mergeHistory(obj)
            }
        }.resume()
    }
    
    /// Also do a full sync (no cursors) on first connect
    func fullSync(host: String, port: Int) {
        guard !host.isEmpty,
              let url = URL(string: "http://\(host):\(port)/api/v1/history")
        else { return }
        
        var req = URLRequest(url: url)
        req.timeoutInterval = 10
        
        URLSession.shared.dataTask(with: req) { [weak self] data, _, error in
            guard let self, let data, error == nil,
                  let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any]
            else { return }
            
            DispatchQueue.main.async {
                self.mergeHistory(obj)
            }
        }.resume()
    }
    
    /// Called from the background fetch handler to merge new records and persist.
    func mergeHistoryFromBackground(_ obj: [String: Any]) {
        mergeHistory(obj)
    }

    private func mergeHistory(_ obj: [String: Any]) {
        // Merge call records
        if let callsArray = obj["calls"] as? [[String: Any]] {
            let decoder = JSONDecoder()
            for callObj in callsArray {
                if let data = try? JSONSerialization.data(withJSONObject: callObj),
                   let record = try? decoder.decode(CallRecord.self, from: data) {
                    if !callRecords.contains(where: { $0.id == record.id }) {
                        callRecords.append(record)
                    }
                }
            }
        }
        
        // Merge SMS records
        if let smsArray = obj["sms"] as? [[String: Any]] {
            let decoder = JSONDecoder()
            for smsObj in smsArray {
                if let data = try? JSONSerialization.data(withJSONObject: smsObj),
                   let record = try? decoder.decode(SMSRecord.self, from: data) {
                    if !smsRecords.contains(where: { $0.id == record.id }) {
                        smsRecords.append(record)
                    }
                }
            }
        }
        
        saveLocal()
    }
    
    // MARK: - Helpers

    /// Executes immediately if already on the main thread, otherwise dispatches async.
    /// This keeps @Published mutations safe from background callers while avoiding
    /// a one-frame delay when the caller is already on main.
    private func runOnMain(_ work: @escaping () -> Void) {
        if Thread.isMainThread { work() } else { DispatchQueue.main.async(execute: work) }
    }

    // MARK: - Convenience
    
    /// SMS grouped by peer, sorted by latest message first.
    var conversations: [Conversation] {
        var byPeer: [String: [SMSRecord]] = [:]
        for m in smsRecords {
            byPeer[m.peer, default: []].append(m)
        }
        return byPeer.map { peer, messages in
            Conversation(peer: peer, messages: messages.sorted { $0.at < $1.at })
        }.sorted { a, b in
            (a.messages.last?.at ?? "") > (b.messages.last?.at ?? "")
        }
    }
    
    struct Conversation: Identifiable {
        var id: String { peer }
        let peer: String
        let messages: [SMSRecord]
        
        var lastMessage: SMSRecord? { messages.last }
        var unreadCount: Int { 0 } // TODO: track read state
    }
}
