import AVFoundation
import Foundation

/// The one microphone every hub connection shares, mirroring Android's AudioRecorder.
///
/// A session records linear PCM WAV to a temporary file. Reads seek past the 44-byte header and
/// drain the file from an offset, so audio recorded while the agent processes a chunk is still
/// there for the next read. AVAudioRecorder keeps writing the data chunk while the header's
/// size fields are placeholders; stop() finalises them, but reads never depend on them.
final class Audio: @unchecked Sendable {
    /// Called on the main queue when recording starts or stops, for the on-screen indicator.
    var onRecording: ((Bool) -> Void)?

    private let lock = NSLock()
    private var recorder: AVAudioRecorder?
    private var file: URL?
    private var id: String?
    private var rate = 16000
    private var readOffset: UInt64 = 44

    static func capabilities() -> [String: Any] {
        ["version": 1, "mime": "audio/wav", "sampleRate": 16000, "samples": [16000],
         "maxOneShotMs": 60000, "chunkMs": 10000, "maxChunkMs": 30000, "maxSessionMs": 7200000,
         "levels": true]
    }

    /// Starts continuous recording. maxDurationMs 0 means until stop.
    @discardableResult
    func start(maxDurationMs: Int, sampleRate: Int) throws -> [String: Any] {
        stopAll()
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
        recorder = r
        file = url
        self.id = String(id)
        self.rate = rate
        readOffset = 44
        lock.unlock()
        DispatchQueue.main.async { self.onRecording?(true) }
        return ["listenId": String(id), "sampleRate": rate, "maxDurationMs": maxDurationMs]
    }

    /// Returns the next chunk, waiting up to maxMs for it to be recorded. Each chunk is a
    /// complete WAV file; ended is true only once the session stopped and nothing is unread.
    func read(id: String, maxMs: Int) async throws -> (meta: [String: Any], wav: Data) {
        try await readChunk(id: id, chunkMs: min(30000, max(100, maxMs)))
    }

    // chunkMs may exceed the protocol chunk cap for a one-shot clip: there the whole clip is
    // wanted in one reply, and the recorder stops on its own at that length.
    private func readChunk(id: String, chunkMs: Int) async throws -> (meta: [String: Any], wav: Data) {
        let chunkMs = min(60000, max(100, chunkMs))
        let deadline = Date().addingTimeInterval(Double(chunkMs + 5000) / 1000)
        var want: UInt64 = 0
        while true {
            let s = try state(id)
            want = UInt64(chunkMs) * UInt64(s.rate) * 2 / 1000
            if s.available >= want || s.ended || Date() >= deadline { break }
            try await Task.sleep(nanoseconds: 20_000_000)
        }
        lock.lock()
        defer { lock.unlock() }
        guard let r = recorder, self.id == id, let url = file else { throw LinkError("no such audio session") }
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
        let ended = !r.isRecording && readOffset >= size
        let dataBytes = size > 44 ? size - 44 : 0
        let total = Int(dataBytes * 1000 / UInt64(rate * 2))
        let meta: [String: Any] = [
            "mime": "audio/wav", "bytes": 44 + pcm.count,
            "durationMs": pcm.count * 1000 / (rate * 2), "sampleRate": rate,
            "totalMs": total, "ended": ended, "levels": Self.levels(pcm),
        ]
        return (meta, Self.wav(pcm, rate: rate))
    }

    func oneShot(durationMs: Int, sampleRate: Int) async throws -> (meta: [String: Any], wav: Data) {
        let started = try start(maxDurationMs: durationMs, sampleRate: sampleRate)
        guard let id = started["listenId"] as? String else { throw LinkError("could not start the microphone") }
        do {
            let out = try await readChunk(id: id, chunkMs: min(60000, max(100, durationMs)))
            _ = try? stop(id: id)
            return out
        } catch {
            _ = try? stop(id: id)
            throw error
        }
    }

    func level(durationMs: Int, sampleRate: Int) async throws -> [String: Any] {
        let out = try await oneShot(durationMs: durationMs, sampleRate: sampleRate)
        return ["durationMs": out.meta["durationMs"] ?? durationMs, "sampleRate": out.meta["sampleRate"] ?? 16000,
                "levels": out.meta["levels"] ?? Self.levels(Data())]
    }

    /// Stops the session with this id, or any session when nil (the agent left).
    @discardableResult
    func stop(id: String?) throws -> [String: Any] {
        lock.lock()
        defer { lock.unlock() }
        guard let r = recorder else { throw LinkError("no such audio session") }
        if let id, id != self.id { throw LinkError("no such audio session") }
        let size = fileSize(file)
        let dataBytes = size > 44 ? size - 44 : 0
        let total = Int(dataBytes * 1000 / UInt64(rate * 2))
        r.stop()
        recorder = nil
        let url = file
        file = nil
        self.id = nil
        try? AVAudioSession.sharedInstance().setActive(false, options: .notifyOthersOnDeactivation)
        if let url { try? FileManager.default.removeItem(at: url) }
        DispatchQueue.main.async { self.onRecording?(false) }
        return ["durationMs": total, "totalMs": total, "ended": true]
    }

    /// Privacy: every agent departure or socket loss closes the microphone.
    func stopAll() {
        _ = try? stop(id: nil)
    }

    private func state(_ id: String) throws -> (available: UInt64, ended: Bool, rate: Int) {
        lock.lock()
        defer { lock.unlock() }
        guard let r = recorder, self.id == id, let url = file else { throw LinkError("no such audio session") }
        let size = fileSize(url)
        let available = size > readOffset ? size - readOffset : 0
        // No more audio will arrive once the recorder stops, so a partial final chunk is
        // delivered immediately instead of waiting out the deadline.
        return (available, !r.isRecording, rate)
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
