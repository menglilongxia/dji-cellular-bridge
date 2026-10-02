import Foundation
import AVFoundation
import CallKit

/// Owns the CallKit provider and maps it to SIPClient calls.
/// The native iOS call UI (lock screen, recents, speaker routing) comes from here.
final class CallManager: NSObject {
    let provider: CXProvider
    let callController = CXCallController()
    let audio = AudioBridge()

    /// uuid -> number, for calls currently known to CallKit.
    private var knownCalls: [UUID: String] = [:]
    weak var sip: SIPClient?

    /// Last audio-bridge failure, surfaced to the UI and to the gateway. The
    /// old code used `try? audio.start()`, so a denied microphone or a failed
    /// session activation silently produced a call with no audio at all.
    private(set) var audioError: String?
    private var audioRetries = 0
    /// Report sink, wired by AppState to POST the state to the gateway.
    var onAudioState: ((String) -> Void)?

    override init() {
        // iOS 14 dropped the configurable call-provider name: `localizedName`
        // is now readonly and "no longer supported", so the system falls back
        // to the app's own display name. Use the plain initializer.
        let config = CXProviderConfiguration()
        config.supportsVideo = false
        config.supportedHandleTypes = [.phoneNumber]
        config.maximumCallsPerCallGroup = 1
        config.maximumCallGroups = 1
        provider = CXProvider(configuration: config)
        super.init()
        provider.setDelegate(self, queue: nil)
        audio.onMicFrame = { [weak self] pcm in self?.sip?.sendAudio(pcm) }
        audio.onDiagnostic = { [weak self] message in self?.report(message) }
    }

    /// Starts the audio bridge, retrying a few times (CallKit activation and
    /// the microphone prompt can lag the SIP answer by a moment).
    func ensureAudioStarted(reason: String) {
        if audio.isRunning {
            // Running but not delivering is the outgoing-call failure mode:
            // CallKit activated the session before the callee answered.
            audio.ensureDelivering()
            return
        }
        // Probe UDP reachability once per call: the media path and the
        // signalling path are different sockets, and only one of them may be
        // usable on this phone.
        sip?.probeUDPEcho()
        do {
            try audio.start()
            audioError = nil
            audioRetries = 0
            report("audio started (\(reason))")
        } catch {
            audioError = "\(error)"
            report("audio start failed (\(reason)): \(error)")
            guard audioRetries < 3 else { return }
            audioRetries += 1
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.7) { [weak self] in
                self?.ensureAudioStarted(reason: "retry\(self?.audioRetries ?? 0)")
            }
        }
    }

    private func report(_ message: String) {
        print("CellBridge audio: \(message)")
        onAudioState?(message)
    }

    // MARK: - incoming

    /// Report an incoming call to the system UI. Must be called within ~5s
    /// of a VoIP push, or iOS revokes VoIP push privileges.
    func reportIncomingCall(uuid: UUID, number: String) {
        knownCalls[uuid] = number
        let update = CXCallUpdate()
        update.remoteHandle = CXHandle(type: .phoneNumber, value: number)
        update.localizedCallerName = number
        update.hasVideo = false
        provider.reportNewIncomingCall(with: uuid, update: update) { [weak self] error in
            if let error {
                self?.knownCalls.removeValue(forKey: uuid)
                self?.sip?.decline(uuid: uuid)
                print("CallKit report failed: \(error)")
            }
        }
    }

    // MARK: - outgoing

    func startOutgoingCall(number: String) {
        guard let sip else { return }
        let uuid = sip.call(number: number)
        knownCalls[uuid] = number
        let handle = CXHandle(type: .phoneNumber, value: number)
        let action = CXStartCallAction(call: uuid, handle: handle)
        action.isVideo = false
        callController.request(CXTransaction(action: action)) { [weak self] error in
            if let error {
                print("CXStartCall failed: \(error)")
                self?.knownCalls.removeValue(forKey: uuid)
                sip.hangup(uuid: uuid)
            }
        }
    }

    // MARK: - teardown

    func endCall(uuid: UUID, reason: CXCallEndedReason = .remoteEnded) {
        knownCalls.removeValue(forKey: uuid)
        provider.reportCall(with: uuid, endedAt: Date(), reason: reason)
    }

    func failCall(uuid: UUID) {
        knownCalls.removeValue(forKey: uuid)
        provider.reportCall(with: uuid, endedAt: Date(), reason: .failed)
    }
}

// MARK: - CXProviderDelegate

extension CallManager: CXProviderDelegate {
    func providerDidReset(_ provider: CXProvider) {
        audio.stop()
        knownCalls.removeAll()
    }

    func provider(_ provider: CXProvider, perform action: CXAnswerCallAction) {
        guard knownCalls[action.callUUID] != nil else { action.fail(); return }
        sip?.answer(uuid: action.callUUID)
        action.fulfill()
        // Do not wait only for didActivate: if it never fires, a fallback here
        // is the difference between a working call and a completely silent one.
        ensureAudioStarted(reason: "answer")
    }

    func provider(_ provider: CXProvider, perform action: CXEndCallAction) {
        sip?.hangup(uuid: action.callUUID)
        knownCalls.removeValue(forKey: action.callUUID)
        action.fulfill()
    }

    func provider(_ provider: CXProvider, perform action: CXStartCallAction) {
        // SIP INVITE already sent in startOutgoingCall; just fulfill.
        action.fulfill()
    }

    func provider(_ provider: CXProvider, perform action: CXSetMutedCallAction) {
        // Mute = stop forwarding mic frames.
        if action.isMuted { audio.onMicFrame = nil }
        else { audio.onMicFrame = { [weak self] pcm in self?.sip?.sendAudio(pcm) } }
        action.fulfill()
    }

    func provider(_ provider: CXProvider, didActivate audioSession: AVAudioSession) {
        ensureAudioStarted(reason: "didActivate")
    }

    func provider(_ provider: CXProvider, didDeactivate audioSession: AVAudioSession) {
        audio.stop()
    }
}
