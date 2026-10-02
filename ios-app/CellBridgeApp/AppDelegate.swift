import UIKit
import PushKit
import AVFoundation
import UserNotifications
import BackgroundTasks

/// Handles VoIP pushes, local notifications for missed events, and background
/// refresh to poll the gateway when the app is suspended.
final class AppDelegate: NSObject, UIApplicationDelegate {
    var appState: AppState?
    private var pushRegistry: PKPushRegistry?

    static let bgTaskID = "com.example.cellbridge.sip.refresh"

    func application(_ application: UIApplication,
                     didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil) -> Bool {
#if CELLBRIDGE_NO_PUSH
        NSLog("CellBridge: built without push entitlement, skipping PushKit registration")
#else
        let registry = PKPushRegistry(queue: .main)
        registry.delegate = self
        registry.desiredPushTypes = [.voIP]
        pushRegistry = registry
#endif
        AudioBridge.prepareSession()
        if #available(iOS 17.0, *) {
            AVAudioApplication.requestRecordPermission { granted in
                NSLog("CellBridge: microphone permission granted=\(granted)")
            }
        } else {
            AVAudioSession.sharedInstance().requestRecordPermission { granted in
                NSLog("CellBridge: microphone permission granted=\(granted)")
            }
        }

        // Request notification permission for missed call/SMS alerts
        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound, .badge]) { granted, _ in
            NSLog("CellBridge: notification permission granted=\(granted)")
        }

        // Register background task for polling the gateway
        BGTaskScheduler.shared.register(forTaskWithIdentifier: Self.bgTaskID, using: nil) { task in
            self.handleBackgroundRefresh(task as! BGAppRefreshTask)
        }

        // Enable background fetch (legacy, works on older iOS too)
        application.setMinimumBackgroundFetchInterval(UIApplication.backgroundFetchIntervalMinimum)

        return true
    }

    // MARK: - Background fetch (legacy)

    func application(_ application: UIApplication,
                     performFetchWithCompletionHandler completionHandler: @escaping (UIBackgroundFetchResult) -> Void) {
        pollGatewayForNotifications { newData in
            completionHandler(newData ? .newData : .noData)
        }
    }

    // MARK: - BGTaskScheduler (iOS 13+)

    func scheduleBackgroundRefresh() {
        let request = BGAppRefreshTaskRequest(identifier: Self.bgTaskID)
        request.earliestBeginDate = Date(timeIntervalSinceNow: 60) // at least 1 min
        do {
            try BGTaskScheduler.shared.submit(request)
        } catch {
            NSLog("CellBridge: bg task schedule failed: \(error)")
        }
    }

    private func handleBackgroundRefresh(_ task: BGAppRefreshTask) {
        // Schedule the next one
        scheduleBackgroundRefresh()

        task.expirationHandler = { task.setTaskCompleted(success: false) }

        pollGatewayForNotifications { _ in
            task.setTaskCompleted(success: true)
        }
    }

    // MARK: - Poll gateway & send local notifications

    private func pollGatewayForNotifications(completion: @escaping (Bool) -> Void) {
        guard let app = appState, !app.serverHost.isEmpty else {
            completion(false)
            return
        }
        let host = app.serverHost
        let port = app.apiPort

        guard let url = URL(string: "http://\(host):\(port)/api/v1/history") else {
            completion(false)
            return
        }

        var req = URLRequest(url: url)
        req.timeoutInterval = 10

        URLSession.shared.dataTask(with: req) { [weak self] data, _, error in
            guard let self, let data, error == nil,
                  let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any]
            else {
                completion(false)
                return
            }

            var hasNew = false

            // Check for new missed calls
            if let calls = obj["calls"] as? [[String: Any]] {
                let knownIDs = Set(self.appState?.history.callRecords.map { $0.id } ?? [])
                for call in calls {
                    guard let id = call["id"] as? String,
                          !knownIDs.contains(id),
                          let status = call["status"] as? String,
                          let number = call["number"] as? String,
                          let direction = call["direction"] as? String,
                          direction == "in"
                    else { continue }
                    hasNew = true
                    let title = status == "missed" ? "\(number) " : "\(number) "
                    self.sendLocalNotification(title: title,
                                               body: status == "missed" ? "\(number)" : "\(number)",
                                               id: "call-\(id)")
                }
            }

            // Check for new incoming SMS
            if let sms = obj["sms"] as? [[String: Any]] {
                let knownIDs = Set(self.appState?.history.smsRecords.map { $0.id } ?? [])
                for msg in sms {
                    guard let id = msg["id"] as? String,
                          !knownIDs.contains(id),
                          let direction = msg["direction"] as? String,
                          direction == "in",
                          let peer = msg["peer"] as? String,
                          let body = msg["body"] as? String
                    else { continue }
                    hasNew = true
                    self.sendLocalNotification(title: peer,
                                               body: body,
                                               id: "sms-\(id)")
                }
            }

            // Merge into local store
            DispatchQueue.main.async {
                self.appState?.history.mergeHistoryFromBackground(obj)
            }

            completion(hasNew)
        }.resume()
    }

    private func sendLocalNotification(title: String, body: String, id: String) {
        let content = UNMutableNotificationContent()
        content.title = title
        content.body = body
        content.sound = .default
        let request = UNNotificationRequest(identifier: id, content: content, trigger: nil)
        UNUserNotificationCenter.current().add(request) { error in
            if let error { NSLog("CellBridge: notification error: \(error)") }
        }
    }
}

extension AppDelegate: PKPushRegistryDelegate {
    func pushRegistry(_ registry: PKPushRegistry, didUpdate pushCredentials: PKPushCredentials, for type: PKPushType) {
        let token = pushCredentials.token.map { String(format: "%02x", $0) }.joined()
        print("VoIP push token: \(token)")
        appState?.sip.pushToken = token
        if appState?.registered == true { appState?.sip.register() }
    }

    func pushRegistry(_ registry: PKPushRegistry, didReceiveIncomingPushWith payload: PKPushPayload,
                      for type: PKPushType, completion: @escaping () -> Void) {
        appState?.ensureConnected()
        completion()
    }

    func pushRegistry(_ registry: PKPushRegistry, didInvalidatePushTokenFor type: PKPushType) {
        appState?.sip.pushToken = nil
    }
}
