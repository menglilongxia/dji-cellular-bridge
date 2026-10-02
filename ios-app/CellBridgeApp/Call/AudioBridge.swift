import Foundation
import AVFoundation

/// Bridges phone mic/speaker with RTP: mic PCM -> SIPClient.sendAudio,
/// incoming RTP -> speaker. The RTP path is 8 kHz mono Int16 (PCMU); the
/// hardware runs at 48 kHz Float32, so everything is converted here.
///
/// IMPORTANT: `AVAudioNode.installTap(onBus:bufferSize:format:)` throws an
/// Objective-C exception (which Swift cannot catch) unless the format's sample
/// rate matches the node's own output format. Installing the tap with 8 kHz
/// used to abort the whole app the moment CallKit activated the audio session —
/// that was the "call instantly crashes" bug on 2026-10-01.
final class AudioBridge {
    /// 160 samples = 20 ms @ 8 kHz, matching the RTP frame size.
    var onMicFrame: (([Int16]) -> Void)?

    private let engine = AVAudioEngine()
    private let player = AVAudioPlayerNode()

    /// The format of the SIP/RTP leg.
    private let voiceFormat = AVAudioFormat(commonFormat: .pcmFormatInt16,
                                            sampleRate: 8000,
                                            channels: 1,
                                            interleaved: true)!

    private var converter: AVAudioConverter?
    private var tapInstalled = false
    private var playerAttached = false
    private var running = false

    private let frameSamples = 160

    /// True once capture+playback are live. Callers check this instead of
    /// assuming `start()` worked: a call whose audio bridge never started sends
    /// no RTP at all and plays nothing, which looks exactly like a dead link.
    var isRunning: Bool { running }

    /// Diagnostics forwarded to the gateway (see AppState.reportAudioState).
    var onDiagnostic: ((String) -> Void)?
    private var tapBuffers = 0
    private var tapFrames = 0

    /// Prepares the audio session early. CallKit activates the session when the
    /// call is answered, and it uses whatever category is configured *then* —
    /// configuring it inside `didActivate` is too late for the system's own
    /// decision about recording. Safe to call repeatedly.
    static func prepareSession() {
        let session = AVAudioSession.sharedInstance()
        try? session.setCategory(.playAndRecord, mode: .voiceChat,
                                 options: [.allowBluetooth, .defaultToSpeaker])
    }

    func start() throws {
        guard !running else { return }

        let session = AVAudioSession.sharedInstance()
        try session.setCategory(.playAndRecord, mode: .voiceChat,
                                options: [.allowBluetooth, .defaultToSpeaker])
        try session.setActive(true, options: [])

        let input = engine.inputNode
        let inputFormat = input.outputFormat(forBus: 0)
        guard inputFormat.sampleRate > 0, inputFormat.channelCount > 0 else {
            throw NSError(domain: "CellBridge", code: 1, userInfo: [
                NSLocalizedDescriptionKey: "no audio input available",
            ])
        }

        // Attach once. AVAudioEngine keeps the node from the previous call, and
        // attaching it again is an Objective-C exception Swift cannot catch —
        // which killed audio on every call after the first one.
        if !playerAttached {
            engine.attach(player)
            playerAttached = true
        }
        engine.connect(player, to: engine.mainMixerNode, format: voiceFormat)

        converter = AVAudioConverter(from: inputFormat, to: voiceFormat)

        if !tapInstalled {
            // The tap format MUST equal the node's output format. Conversion to
            // 8 kHz happens in the callback instead.
            input.installTap(onBus: 0, bufferSize: 1024, format: inputFormat) { [weak self] buffer, _ in
                self?.handleMicBuffer(buffer)
            }
            tapInstalled = true
        }

        engine.prepare()
        try engine.start()
        player.play()
        running = true
        onDiagnostic?("audio engine started (in \(Int(inputFormat.sampleRate))Hz)")
        tapBuffers = 0
        tapFrames = 0
        // Prove the microphone actually feeds the engine: an "audio started"
        // with a silent tap looks identical to a working one from outside.
        DispatchQueue.main.asyncAfter(deadline: .now() + 2.5) { [weak self] in
            guard let self, self.running else { return }
            self.onDiagnostic?("mic tap: \(self.tapBuffers) buffers, \(self.tapFrames) frames sent")
        }
        watchTapDelivery(attempt: 1)
    }

    private var tapWatchdog: DispatchWorkItem?
    private var restartAttempts = 0

    /// True when the engine is running *and* the microphone is really feeding
    /// it. "Running" alone means nothing on iOS — the tap can be installed
    /// while the hardware never delivers a sample.
    var isDelivering: Bool { running && tapBuffers > 0 }

    /// Called when the call becomes active (the first moment the microphone is
    /// genuinely usable on an outgoing call): restarts the engine if it has
    /// been sitting there silent.
    func ensureDelivering() {
        guard running, tapBuffers == 0 else { return }
        onDiagnostic?("mic still silent when the call became active — restarting audio")
        hardRestart()
        watchTapDelivery(attempt: 1)
    }

    /// An outgoing call activates its audio session *before* the callee picks
    /// up. `start()` then reports success while the microphone tap never fires:
    /// the phone sends no RTP at all and the call is silent towards the caller.
    /// Watch for that and restart the engine.
    private func watchTapDelivery(attempt: Int) {
        tapWatchdog?.cancel()
        let work = DispatchWorkItem { [weak self] in
            guard let self, self.running, self.tapBuffers == 0 else { return }
            if attempt > 1 {
                self.onDiagnostic?("mic tap still silent after restart — not retrying further to avoid call disruption")
                return
            }
            self.onDiagnostic?("mic tap silent — restarting audio (attempt \(attempt))")
            self.hardRestart()
            self.watchTapDelivery(attempt: attempt + 1)
        }
        tapWatchdog = work
        DispatchQueue.main.asyncAfter(deadline: .now() + 1.6, execute: work)
    }

    /// Restarts the engine without deactivating the CallKit-owned session.
    private func hardRestart() {
        running = false
        if tapInstalled {
            engine.inputNode.removeTap(onBus: 0)
            tapInstalled = false
        }
        player.stop()
        engine.stop()
        tapBuffers = 0
        do {
            try start()
        } catch {
            onDiagnostic?("audio restart failed: \(error)")
        }
    }

    private func handleMicBuffer(_ buffer: AVAudioPCMBuffer) {
        guard running, let converter else { return }
        tapBuffers += 1

        let ratio = voiceFormat.sampleRate / buffer.format.sampleRate
        let capacity = AVAudioFrameCount(Double(buffer.frameLength) * ratio) + 64
        guard let out = AVAudioPCMBuffer(pcmFormat: voiceFormat, frameCapacity: capacity) else {
            return
        }

        var supplied = false
        var conversionError: NSError?
        converter.convert(to: out, error: &conversionError) { _, status in
            if supplied {
                status.pointee = .noDataNow
                return nil
            }
            supplied = true
            status.pointee = .haveData
            return buffer
        }

        guard conversionError == nil, out.frameLength > 0,
              let channel = out.int16ChannelData else { return }

        let frames = Int(out.frameLength)
        var pcm = [Int16](repeating: 0, count: frames)
        for i in 0..<frames { pcm[i] = channel[0][i] }

        var offset = 0
        while offset + frameSamples <= frames {
            onMicFrame?(Array(pcm[offset..<(offset + frameSamples)]))
            tapFrames += frameSamples
            offset += frameSamples
        }
    }

    /// Plays one 20 ms PCMU-decoded frame through the speaker.
    func play(pcm: [Int16]) {
        guard running, !pcm.isEmpty else { return }
        guard let buf = AVAudioPCMBuffer(pcmFormat: voiceFormat,
                                         frameCapacity: AVAudioFrameCount(pcm.count)) else {
            return
        }
        buf.frameLength = AVAudioFrameCount(pcm.count)
        pcm.withUnsafeBufferPointer { src in
            guard let dst = buf.int16ChannelData?[0], let base = src.baseAddress else { return }
            dst.update(from: base, count: pcm.count)
        }
        player.scheduleBuffer(buf, completionHandler: nil)
    }

    func stop() {
        guard running else { return }
        running = false
        tapWatchdog?.cancel(); tapWatchdog = nil
        if tapInstalled {
            engine.inputNode.removeTap(onBus: 0)
            tapInstalled = false
        }
        player.stop()
        engine.stop()
        converter = nil
        try? AVAudioSession.sharedInstance().setActive(false, options: [])
    }
}
