import AVFoundation
import Foundation

/// The one microphone every hub connection shares, mirroring Android's AudioRecorder.
///
/// A session records linear PCM WAV to a temporary file. Reads start at the RIFF data chunk
/// (found by walking the chunk headers, never assumed to be byte 44) and drain the file from an
/// offset, so audio recorded while the agent processes a chunk is still there for the next read.
/// AVAudioRecorder keeps writing the data chunk while the header's size fields are placeholders;
/// stop() finalises them, but reads never depend on them.
///
/// Every session carries the connection that started it ([owner]); a departure, socket loss or
/// reset on one hub only stops that hub's session, so unrelated hub churn cannot silence another
/// agent. The delegate clears the on-screen indicator when a timed recording stops itself.
final class Audio: NSObject, AVAudioRecorderDelegate, @unchecked Sendable {
    /// Called on the main queue when recording starts or stops, for the on-screen indicator.
    var onRecording: ((Bool) -> Void)?

    private let startLock = NSLock()
    private let lock = NSLock()
    private var recorder: AVAudioRecorder?
    private var file: URL?
    private var id: String?
    private var owner: String?
    private var rate = 16000
    private var readOffset: UInt64 = 0
    /// Offset of the PCM payload, cached once the file's RIFF header can be walked.
    private var dataStart: UInt64 = 0
    /// False while a session is live and its end has not been reported; guards the one-shot
    /// notification so the delegate and a read cannot both fire it.
    private var stopNotified = true
    /// Bumped whenever [stopAll] (or `stop(owner:id:nil)`) is called for an owner, so an
    /// asynchronous startup that has not yet published its recorder sees the departure and aborts.
    private var stopCounts: [String: Int] = [:]

    static func capabilities() -> [String: Any] {
        ["version": 1, "mime": "audio/wav", "sampleRate": 16000, "samples": [16000],
         "maxOneShotMs": 60000, "chunkMs": 10000, "maxChunkMs": 30000, "maxSessionMs": 7200000,
         "levels": true]
    }

    /// Snapshot of [owner]'s departure counter, captured on the connection actor before awaiting
    /// an audio call so a departure during startup cannot open the microphone.
    func stopToken(owner: String) -> Int {
        lock.lock()
        defer { lock.unlock() }
        return stopCounts[owner, default: 0]
    }

    private func isTokenValid(owner: String, token: Int) -> Bool {
        lock.lock()
        defer { lock.unlock() }
        return stopCounts[owner, default: 0] == token
    }

    /// Starts continuous recording for [owner]. maxDurationMs 0 means until stop.
    @discardableResult
    func start(owner: String, maxDurationMs: Int, sampleRate: Int, stopToken: Int? = nil) throws -> [String: Any] {
        startLock.lock()
        defer { startLock.unlock() }
        let expectedToken = stopToken ?? self.stopToken(owner: owner)
        guard isTokenValid(owner: owner, token: expectedToken) else {
            throw LinkError("audio session cancelled")
        }
        stopAny() // a new recording preempts whatever session is live, whoever owns it
        guard isTokenValid(owner: owner, token: expectedToken) else {
            throw LinkError("audio session cancelled")
        }
        guard AVAudioApplication.shared.recordPermission == .granted else {
            throw LinkError("microphone permission denied; allow it in Settings")
        }
        let rate = min(48000, max(8000, sampleRate))
        let url = FileManager.default.temporaryDirectory.appendingPathComponent("audio-\(UUID().uuidString).wav")
        let settings: [String: Any] = [
            AVFormatIDKey: kAudioFormatLinearPCM,
            AVSampleRateKey: rate,
            AVNumberOfChannelsKey: 1,
            AVLinearPCMBitDepthKey: 16,
            AVLinearPCMIsFloatKey: false,
            AVLinearPCMIsBigEndianKey: false,
        ]
        let session = AVAudioSession.sharedInstance()
        try session.setCategory(.record, mode: .measurement, options: [])
        try session.setActive(true, options: [])
        let r: AVAudioRecorder
        do {
            r = try AVAudioRecorder(url: url, settings: settings)
        } catch {
            try? session.setActive(false, options: .notifyOthersOnDeactivation)
            throw error
        }
        guard r.prepareToRecord() else {
            try? session.setActive(false, options: .notifyOthersOnDeactivation)
            try? FileManager.default.removeItem(at: url)
            throw LinkError("microphone unavailable")
        }
        let started: Bool
        if maxDurationMs > 0 {
            started = r.record(forDuration: Double(maxDurationMs) / 1000)
        } else {
            started = r.record()
        }
        guard started else {
            try? session.setActive(false, options: .notifyOthersOnDeactivation)
            try? FileManager.default.removeItem(at: url)
            throw LinkError("microphone is busy")
        }
        let id = UUID().uuidString.lowercased().prefix(16)
        lock.lock()
        if stopCounts[owner, default: 0] != expectedToken {
            lock.unlock()
            r.stop()
            try? session.setActive(false, options: .notifyOthersOnDeactivation)
            try? FileManager.default.removeItem(at: url)
            throw LinkError("audio session cancelled")
        }
        r.delegate = self
        recorder = r
        file = url
        self.id = String(id)
        self.owner = owner
        self.rate = rate
        readOffset = 0
        dataStart = 0
        stopNotified = false
        lock.unlock()
        DispatchQueue.main.async { self.onRecording?(true) }
        return ["listenId": String(id), "sampleRate": rate, "maxDurationMs": maxDurationMs]
    }

    /// Returns the next chunk of [owner]'s session, waiting up to maxMs for it to be recorded.
    /// Each chunk is a complete WAV file; ended is true only once the session stopped and
    /// nothing is unread.
    func read(owner: String, id: String, maxMs: Int) async throws -> (meta: [String: Any], wav: Data) {
        try await readChunk(owner: owner, id: id, chunkMs: min(30000, max(100, maxMs)))
    }

    // chunkMs may exceed the protocol chunk cap for a one-shot clip: there the whole clip is
    // wanted in one reply, and the recorder stops on its own at that length.
    private func readChunk(owner: String, id: String, chunkMs: Int) async throws -> (meta: [String: Any], wav: Data) {
        let chunkMs = min(60000, max(100, chunkMs))
        let deadline = Date().addingTimeInterval(Double(chunkMs + 5000) / 1000)
        var want: UInt64 = 0
        while true {
            let s = try state(owner: owner, id: id)
            if s.autoStopped { audioFinished() }
            want = UInt64(chunkMs) * UInt64(s.rate) * 2 / 1000
            if s.available >= want || s.ended || Date() >= deadline { break }
            try await Task.sleep(nanoseconds: 20_000_000)
        }
        return try drain(owner: owner, id: id, want: want)
    }

    // The file read and the readOffset update stay out of the async path, so the NSLock is
    // never taken from an async context.
    private func drain(owner: String, id: String, want: UInt64) throws -> (meta: [String: Any], wav: Data) {
        var autoStopped = false
        var out: (meta: [String: Any], wav: Data)!
        lock.lock()
        do {
            defer { lock.unlock() }
            guard let r = recorder, self.id == id, self.owner == owner, let url = file else {
                throw LinkError("no such audio session")
            }
            let origin = pcmOrigin(url)
            if readOffset < origin { readOffset = origin }
            let size = fileSize(url)
            let available = size > readOffset ? size - readOffset : 0
            let n = Int(min(want, available))
            var pcm = Data()
            if n > 0 {
                let fh = try FileHandle(forReadingFrom: url)
                defer { try? fh.close() }
                try fh.seek(toOffset: readOffset)
                pcm = try fh.read(upToCount: n) ?? Data()
                readOffset += UInt64(pcm.count)
            }
            // The recorder may stop between the last state() poll and this drain; report it so
            // the indicator never outlives the microphone.
            if !r.isRecording && !stopNotified { stopNotified = true; autoStopped = true }
            let ended = !r.isRecording && readOffset >= size
            let dataBytes = size > origin ? size - origin : 0
            let total = Int(dataBytes * 1000 / UInt64(rate * 2))
            let meta: [String: Any] = [
                "mime": "audio/wav", "bytes": 44 + pcm.count,
                "durationMs": pcm.count * 1000 / (rate * 2), "sampleRate": rate,
                "totalMs": total, "ended": ended, "levels": Self.levels(pcm),
            ]
            out = (meta, Self.wav(pcm, rate: rate))
        }
        if autoStopped { audioFinished() }
        return out
    }

    func oneShot(owner: String, durationMs: Int, sampleRate: Int, stopToken: Int? = nil) async throws -> (meta: [String: Any], wav: Data) {
        let started = try start(owner: owner, maxDurationMs: durationMs, sampleRate: sampleRate, stopToken: stopToken)
        guard let id = started["listenId"] as? String else { throw LinkError("could not start the microphone") }
        do {
            let out = try await readChunk(owner: owner, id: id, chunkMs: min(60000, max(100, durationMs)))
            _ = try? stop(owner: owner, id: id)
            return out
        } catch {
            _ = try? stop(owner: owner, id: id)
            throw error
        }
    }

    func level(owner: String, durationMs: Int, sampleRate: Int, stopToken: Int? = nil) async throws -> [String: Any] {
        let out = try await oneShot(owner: owner, durationMs: durationMs, sampleRate: sampleRate, stopToken: stopToken)
        return ["durationMs": out.meta["durationMs"] ?? durationMs, "sampleRate": out.meta["sampleRate"] ?? 16000,
                "levels": out.meta["levels"] ?? Self.levels(Data())]
    }

    /// Stops [owner]'s session with this id, or any session it owns when nil. Another
    /// connection's session is left untouched.
    @discardableResult
    func stop(owner: String, id: String?) throws -> [String: Any] {
        lock.lock()
        if id == nil {
            stopCounts[owner, default: 0] += 1
        }
        guard let r = recorder else {
            lock.unlock()
            throw LinkError("no such audio session")
        }
        if self.owner != owner || (id != nil && id != self.id) {
            lock.unlock()
            throw LinkError("no such audio session")
        }
        let result = detachLocked(r)
        lock.unlock()
        finishDetach(result)
        return result.meta
    }

    /// Privacy: every agent departure or socket loss closes the microphone its connection owns.
    func stopAll(owner: String) {
        _ = try? stop(owner: owner, id: nil)
    }

    /// Stops whatever session is live, regardless of owner. Only for a new recording preempting
    /// the old one.
    private func stopAny() {
        lock.lock()
        guard let r = recorder else {
            lock.unlock()
            return
        }
        let result = detachLocked(r)
        lock.unlock()
        finishDetach(result)
    }

    // Detaches the live session and computes its duration; the caller holds the lock. The
    // recorder is stopped and every handle cleared, so no read can find it afterwards.
    private func detachLocked(_ r: AVAudioRecorder) -> (meta: [String: Any], url: URL?) {
        let origin = file.map(pcmOrigin) ?? 44
        let size = fileSize(file)
        let dataBytes = size > origin ? size - origin : 0
        let total = Int(dataBytes * 1000 / UInt64(rate * 2))
        stopNotified = true
        r.stop()
        recorder = nil
        let url = file
        file = nil
        id = nil
        owner = nil
        return (["durationMs": total, "totalMs": total, "ended": true], url)
    }

    // Deactivates the session and clears the indicator; safe to call more than once for the same
    // stop because [detachLocked] and the auto-stop guard both set stopNotified first.
    private func finishDetach(_ result: (meta: [String: Any], url: URL?)) {
        try? AVAudioSession.sharedInstance().setActive(false, options: .notifyOthersOnDeactivation)
        if let url = result.url { try? FileManager.default.removeItem(at: url) }
        DispatchQueue.main.async { self.onRecording?(false) }
    }

    /// The recording stopped on its own (a timed one-shot or the app losing the microphone).
    /// Deactivate and clear the indicator, but keep the file for the final drain.
    private func audioFinished() {
        try? AVAudioSession.sharedInstance().setActive(false, options: .notifyOthersOnDeactivation)
        DispatchQueue.main.async { self.onRecording?(false) }
    }

    func audioRecorderDidFinishRecording(_ recorder: AVAudioRecorder, successfully flag: Bool) {
        lock.lock()
        let mine = self.recorder === recorder && !stopNotified
        if mine { stopNotified = true }
        lock.unlock()
        if mine { audioFinished() }
    }

    private func state(owner: String, id: String) throws -> (available: UInt64, ended: Bool, rate: Int, autoStopped: Bool) {
        lock.lock()
        defer { lock.unlock() }
        guard let r = recorder, self.id == id, self.owner == owner, let url = file else {
            throw LinkError("no such audio session")
        }
        let origin = pcmOrigin(url)
        if readOffset < origin { readOffset = origin }
        let size = fileSize(url)
        let available = size > readOffset ? size - readOffset : 0
        var autoStopped = false
        if !r.isRecording && !stopNotified {
            stopNotified = true
            autoStopped = true
        }
        // No more audio will arrive once the recorder stops, so a partial final chunk is
        // delivered immediately instead of waiting out the deadline.
        return (available, !r.isRecording, rate, autoStopped)
    }

    /// Byte offset of the PCM payload, cached per session. AVAudioRecorder may put extra chunks
    /// between `fmt ` and `data` (Apple writes an "FLLR" filler), so byte 44 is not guaranteed.
    /// Falls back to 44 until the header is complete; every read refreshes the answer while the
    /// file is still being written.
    private func pcmOrigin(_ url: URL) -> UInt64 {
        if dataStart == 0, let found = Self.findDataChunk(url) { dataStart = found }
        return dataStart == 0 ? 44 : dataStart
    }

    private static func findDataChunk(_ url: URL) -> UInt64? {
        guard let fh = try? FileHandle(forReadingFrom: url) else { return nil }
        defer { try? fh.close() }
        let riff = Data("RIFF".utf8), wave = Data("WAVE".utf8), data = Data("data".utf8)
        guard let head = try? fh.read(upToCount: 12), head.count == 12,
              head.prefix(4) == riff, head.dropFirst(8).prefix(4) == wave else { return nil }
        var pos: UInt64 = 12
        while let hdr = try? fh.read(upToCount: 8), hdr.count == 8 {
            let id = hdr.prefix(4)
            var size: UInt64 = 0
            for i in 0..<4 { size |= UInt64(hdr[hdr.startIndex + 4 + i]) << (8 * UInt64(i)) }
            pos += 8
            if id == data { return pos }
            pos += size + size % 2
            guard (try? fh.seek(toOffset: pos)) != nil else { return nil }
        }
        return nil
    }

    private func fileSize(_ url: URL?) -> UInt64 {
        guard let url, let attrs = try? FileManager.default.attributesOfItem(atPath: url.path) else { return 0 }
        return (attrs[.size] as? NSNumber)?.uint64Value ?? 0
    }

    static func wav(_ pcm: Data, rate: Int) -> Data {
        var out = Data(capacity: 44 + pcm.count)
        func word(_ v: UInt32) { withUnsafeBytes(of: v.littleEndian) { out.append(contentsOf: $0) } }
        func half(_ v: UInt16) { withUnsafeBytes(of: v.littleEndian) { out.append(contentsOf: $0) } }
        out.append(Data("RIFF".utf8)); word(UInt32(36 + pcm.count)); out.append(Data("WAVE".utf8))
        out.append(Data("fmt ".utf8)); word(16); half(1); half(1)
        word(UInt32(rate)); word(UInt32(rate * 2)); half(2); half(16)
        out.append(Data("data".utf8)); word(UInt32(pcm.count)); out.append(pcm)
        return out
    }

    static func levels(_ pcm: Data) -> [String: Any] {
        let n = pcm.count / 2
        guard n > 0 else { return ["rmsDb": -100, "peakDb": -100, "clipping": false] }
        var sum = 0.0
        var peak = 0
        pcm.withUnsafeBytes { raw in
            for i in 0..<n {
                let lo = UInt16(raw[i * 2]), hi = UInt16(raw[i * 2 + 1])
                let v = Int(Int16(bitPattern: lo | (hi << 8)))
                sum += Double(v) * Double(v)
                peak = max(peak, abs(v))
            }
        }
        func db(_ v: Double) -> Double {
            guard v > 0 else { return -100 }
            return max(-100, (20 * log10(v / 32768) * 10).rounded() / 10)
        }
        return ["rmsDb": db((sum / Double(n)).squareRoot()), "peakDb": db(Double(peak)), "clipping": peak >= 32767]
    }
}
