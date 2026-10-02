import SwiftUI

@main
struct CellBridgeApp: App {
    @UIApplicationDelegateAdaptor(AppDelegate.self) var appDelegate
    @StateObject private var appState = AppState()

    var body: some Scene {
        WindowGroup {
            ContentView()
                .environmentObject(appState)
                .onAppear {
                    appDelegate.appState = appState
                    appState.onAppLaunch()
                    appDelegate.scheduleBackgroundRefresh()
                }
        }
    }
}
