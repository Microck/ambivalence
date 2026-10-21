import Foundation

/// One socket to one hub, with its own agent state; the phone port of internal/host/fake.go
/// (Phone.Serve). The hub is untrusted: every agent proves the link key here, and only requests
/// sealed under that session touch the camera. Messages are handled one at a time in arrival
/// order, and each sealed send seals and enqueues without suspending, so wire order is seal order.
actor HubConnection {
    enum Phase: Int, Comparable {
        case lost, connecting, waiting, refused, agentConnecting, agentLinked
        static func < (a: Phase, b: Phase) -> Bool { a.rawValue < b.rawValue }
    }

    nonisolated let id = UUID()
    /// Addresses of one hub, tried in order; a discovered multihomed hub may list several.
    nonisolated let candidates: [URL]
    private let identity: Identity
    private let camera: Camera
    private let audio: Audio
    private weak var host: CameraHost?
    private var task: URLSessionWebSocketTask?
    private var closed = false
    private var socketEnded = false
    // The reader counts agent departures as they arrive; the handler counts them as it reaches
    // them. While seen > handled, the agent being handled has already left.
    private var departuresSeen = 0
    private var departuresHandled = 0

    // Per-agent state, reset whenever an agent arrives or leaves. seal is set once the agent has
    // proven the link; only messages sealed under it may touch the camera.
    private var agentHere = false
    private var seal: Seal?
    private var challenge: Data?
    private var pairing: Pairing?
    private var agentGen = 0 // bumps on every reset, so a stale squat timer does nothing

    private struct Pairing {
        let party = Party()
        let nB = Link.nonce()
        let commit: Data
    }

    private static let unauthenticated = "agent not authenticated"

    init(candidates: [URL], identity: Identity, camera: Camera, audio: Audio, host: CameraHost) {
        self.candidates = candidates
        self.identity = identity
        self.camera = camera
        self.audio = audio
        self.host = host
    }

    /// Joins the hub and serves it until the socket ends, then tells the host. A reader reads
    /// ahead of the serial handler only to notice early that the agent left (or the socket died)
    /// while a pairing prompt is open; every message is still handled in arrival order.
    func run() async {
        guard let (task, first) = await open() else {
            closed = true
            let host = self.host, id = self.id
            DispatchQueue.main.async { host?.connectionClosed(id) }
            return
        }
        let (inbox, feed) = AsyncStream<String>.makeStream()
        Task.detached { [weak self] in
            var next = first
            if next == nil { next = try? await task.receive() }
            while let message = next {
                if case .string(let text) = message { // agents never send binary
                    if HubConnection.isDeparture(text) { await self?.agentLeft() }
                    feed.yield(text)
                }
                next = try? await task.receive()
            }
            await self?.socketDied()
            feed.finish()
        }
        HubConnection.keepAlive(task)
        for await text in inbox where !closed {
            await handle(text)
        }
        closed = true
        task.cancel(with: .goingAway, reason: nil)
        resetAgent()
        let host = self.host, id = self.id
        DispatchQueue.main.async { host?.connectionClosed(id) }
    }

    func close() {
        closed = true
        resetAgent()
        task?.cancel(with: .goingAway, reason: nil)
        cancelPrompt()
    }

    /// Connects to the first candidate that answers hello, one at a time. With several
    /// candidates each gets 3 s to deliver the hub's welcome; a lone candidate gets the system
    /// timeout. Returns the socket and its first message, which the reader handles first.
    private func open() async -> (URLSessionWebSocketTask, URLSessionWebSocketTask.Message?)? {
        for url in candidates where !closed {
            let task = URLSession.shared.webSocketTask(with: url)
            task.maximumMessageSize = 16 << 20
            self.task = task
            task.resume()
            send(["type": "hello", "role": "host", "device": identity.device, "host": ["name": "ios", "platform": "ios"]])
            guard candidates.count > 1 else { return (task, nil) }
            let first = await withCheckedContinuation { (cont: CheckedContinuation<URLSessionWebSocketTask.Message?, Never>) in
                let once = Once()
                task.receive { r in once.run { cont.resume(returning: try? r.get()) } }
                DispatchQueue.global().asyncAfter(deadline: .now() + 3) { once.run { cont.resume(returning: nil) } }
            }
            if let first, !closed {
                let host = self.host, id = self.id
                DispatchQueue.main.async { host?.reached(id, url: url.absoluteString) }
                return (task, first)
            }
            task.cancel(with: .goingAway, reason: nil)
        }
        return nil
    }

    /// Pings the hub every 20 s and drops the socket when a pong is not back within 10 s, like
    /// Android's OkHttp pingInterval. Without it a half-open socket (a tunnel or Wi-Fi that died
    /// silently) looks connected forever: the hub has dropped this phone, agents hear "offline",
    /// and the phone never redials. Cancelling ends the reader, so the host redials.
    private static func keepAlive(_ task: URLSessionWebSocketTask) {
        Task.detached {
            while true {
                try? await Task.sleep(nanoseconds: 20_000_000_000)
                guard task.state == .running else { return }
                let alive = await withCheckedContinuation { (cont: CheckedContinuation<Bool, Never>) in
                    let once = Once()
                    task.sendPing { err in once.run { cont.resume(returning: err == nil) } }
                    DispatchQueue.global().asyncAfter(deadline: .now() + 10) { once.run { cont.resume(returning: false) } }
                }
                guard alive else { return task.cancel(with: .goingAway, reason: nil) }
            }
        }
    }

    /// A status or welcome saying no agent is connected.
    private static func isDeparture(_ text: String) -> Bool {
        guard let data = text.data(using: .utf8),
              let m = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any],
              let type = m["type"] as? String, type == "status" || type == "welcome" else { return false }
        return !(m["connected"] as? Bool ?? false)
    }

    private func agentLeft() {
        departuresSeen += 1
        cancelPrompt()
    }

    private func socketDied() {
        socketEnded = true
        cancelPrompt()
    }

    /// The agent being handled can no longer complete a pairing.
    private var agentGone: Bool { closed || socketEnded || departuresSeen > departuresHandled }

    private func cancelPrompt() {
        let host = self.host, id = self.id
        DispatchQueue.main.async { host?.cancelPrompts(for: id) }
    }

    // MARK: protocol

    private func handle(_ text: String) async {
        guard let data = text.data(using: .utf8),
              let m = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] else { return }
        let id = m["id"] as? String ?? ""
        switch m["type"] as? String ?? "" {
        case "welcome":
            let code = m["code"] as? String ?? "", hubId = m["hubId"] as? String ?? ""
            let host = self.host, conn = self.id
            DispatchQueue.main.async { host?.welcome(conn, code: code, hubId: hubId) }
            departure(m)
            presence(m["connected"] as? Bool ?? false)
        case "status":
            departure(m)
            presence(m["connected"] as? Bool ?? false)
        case "ping":
            send(["type": "pong", "id": id])
        case "auth":
            auth(m)
        case "pair1":
            let p = Pairing(commit: Link.unhex(m["commit"] as? String))
            pairing = p
            send(["type": "pair2", "id": id, "pub": Link.hex(p.party.pub), "nonce": Link.hex(p.nB), "device": identity.device])
        case "pair3":
            await pair3(id, m)
        case "sealed":
            await sealed(id, m)
        case "cameras", "command", "capture", "listen", "audio_start", "audio_read", "audio_stop", "level":
            // Unsealed camera requests never count, even after auth: a hub could inject them.
            sendError(id, HubConnection.unauthenticated)
        default:
            break
        }
    }

    private func departure(_ m: [String: Any]) {
        if !(m["connected"] as? Bool ?? false) { departuresHandled += 1 }
    }

    /// An agent arrived or left. The hub repeats status, so act only on a change. A linked phone
    /// challenges every arriving agent; one that neither proves the key nor starts pairing within
    /// 15 s is dropped so it cannot squat the slot.
    private func presence(_ present: Bool) {
        if present && agentHere { return }
        resetAgent()
        agentHere = present
        report(present ? .agentConnecting : .waiting)
        guard present else { return }
        let gen = agentGen
        Task { [weak self] in
            try? await Task.sleep(nanoseconds: 15_000_000_000)
            await self?.squatExpired(gen)
        }
        if identity.key != nil {
            let nP = Link.nonce()
            challenge = nP
            send(["type": "auth", "nonce": Link.hex(nP)])
        }
    }

    private func squatExpired(_ gen: Int) {
        if gen == agentGen, !closed, seal == nil, pairing == nil { reject() }
    }

    private func auth(_ m: [String: Any]) {
        let nP = Link.unhex(m["nonce"] as? String), nA = Link.unhex(m["agentNonce"] as? String)
        let mac = Link.unhex(m["mac"] as? String), dev = Data(identity.device.utf8)
        guard let key = identity.key, let challenge, nA.count == 16, Link.equal(nP, challenge),
              Link.equal(mac, Link.MAC(key, "agent", dev, nP, nA)) else { return reject() }
        // Each challenge verifies one handshake at most.
        seal = Seal(key: key, device: identity.device, nP: nP, nA: nA)
        self.challenge = nil
        report(.agentLinked)
        send(["type": "authok", "nonce": m["nonce"] as? String ?? "", "mac": Link.hex(Link.MAC(key, "phone", dev, nP, nA))])
    }

    private func pair3(_ id: String, _ m: [String: Any]) async {
        let agentPub = Link.unhex(m["pub"] as? String), nA = Link.unhex(m["nonce"] as? String)
        guard let p = pairing, nA.count == 16, Link.equal(p.commit, Link.H("commit", agentPub, nA)) else {
            sendError(id, "pairing commitment mismatch")
            return reject()
        }
        // Prompt only for an agent still here, and store the key only if the user confirmed
        // while this same pairing is current: confirming for an agent that left would replace a
        // working link key with one the agent never received.
        var ok = false
        let gen = agentGen
        let shared = try? p.party.shared(agentPub)
        if shared != nil, !agentGone, let host {
            ok = await host.confirm(Link.SAS(agentPub: agentPub, phonePub: p.party.pub, nA: nA, nB: p.nB), from: self.id)
        }
        // The agent left or the socket went away: end quietly. A reject now could drop a newer
        // agent whose arrival is still queued behind this handler.
        guard gen == agentGen, !agentGone else {
            pairing = nil
            return
        }
        // The user cancelled, the prompt timed out, or the agent's key was not a valid point.
        guard ok, let shared else {
            sendError(id, "pairing cancelled on the phone")
            return reject()
        }
        let key = Link.key(shared: shared, agentPub: agentPub, phonePub: p.party.pub)
        do { try identity.setKey(key) } catch {
            sendError(id, error.localizedDescription)
            return reject()
        }
        seal = Seal(key: key, device: identity.device, nP: p.nB, nA: nA)
        pairing = nil
        report(.agentLinked)
        let host = self.host
        DispatchQueue.main.async { host?.linkChanged() }
        send(["type": "pair4", "id": id, "mac": Link.hex(Link.MAC(key, "confirm", Data(identity.device.utf8)))])
    }

    private func sealed(_ id: String, _ m: [String: Any]) async {
        guard let s = seal else {
            // The phone just (re)connected: make the agent re-authenticate.
            return sendError(id, HubConnection.unauthenticated)
        }
        // Anything that fails to open, or whose inner id differs, was forged or replayed by the
        // hub: ignore it and keep serving the real agent.
        guard let box = Data(base64Encoded: m["box"] as? String ?? ""), let plain = try? s.rx.open(box),
              let inner = (try? JSONSerialization.jsonObject(with: plain)) as? [String: Any],
              inner["id"] as? String ?? "" == id else { return }
        do {
            var reply: [String: Any]
            var payload: Data?
            switch inner["type"] as? String ?? "" {
            case "cameras":
                reply = try await camera.cameras()
                var caps = reply["capabilities"] as? [String: Any] ?? [:]
                caps["audioVersion"] = 1
                caps["audio"] = Audio.capabilities()
                reply["capabilities"] = caps
                reply["audioVersion"] = 1
                reply["audio"] = Audio.capabilities()
            case "command":
                let cam = (inner["camera"] as? String).flatMap { $0.isEmpty ? nil : $0 }
                reply = try await camera.command(camera: cam, torchOn: inner["torchOn"] as? Bool,
                                                 zoom: (inner["zoomLevel"] as? NSNumber)?.doubleValue,
 controls: inner["controls"] as? [String: Any])
            case "capture":
                let q = (inner["quality"] as? NSNumber)?.intValue ?? 0, w = (inner["maxWidth"] as? NSNumber)?.intValue ?? 0
                (reply, payload) = try await camera.capture(quality: q > 0 ? q : 80, maxWidth: w > 0 ? w : 1280)
            case "listen":
                let ms = (inner["durationMs"] as? NSNumber)?.intValue ?? 0
                let rate = (inner["sampleRate"] as? NSNumber)?.intValue ?? 0
                let out = try await audio.oneShot(durationMs: min(60000, max(100, ms > 0 ? ms : 5000)),
                                                  sampleRate: rate > 0 ? rate : 16000)
                reply = out.meta
                reply["type"] = "frame"
                payload = out.wav
            case "audio_start":
                let max = (inner["maxDurationMs"] as? NSNumber)?.intValue ?? 0
                let rate = (inner["sampleRate"] as? NSNumber)?.intValue ?? 0
                reply = try audio.start(maxDurationMs: min(7200000, max(0, max)), sampleRate: rate > 0 ? rate : 16000)
                reply["type"] = "audio_started"
            case "audio_read":
                let ms = (inner["durationMs"] as? NSNumber)?.intValue ?? 0
                let out = try await audio.read(id: inner["listenId"] as? String ?? "",
                                               maxMs: min(30000, max(100, ms > 0 ? ms : 10000)))
                reply = out.meta
                reply["type"] = "frame"
                payload = out.wav
            case "audio_stop":
                reply = try audio.stop(id: inner["listenId"] as? String ?? "")
                reply["type"] = "status"
            case "level":
                let ms = (inner["durationMs"] as? NSNumber)?.intValue ?? 0
                let rate = (inner["sampleRate"] as? NSNumber)?.intValue ?? 0
                reply = try await audio.level(durationMs: min(30000, max(100, ms > 0 ? ms : 1000)),
                                              sampleRate: rate > 0 ? rate : 16000)
                reply["type"] = "levels"
            default:
                throw LinkError("unknown request \(inner["type"] as? String ?? "")")
            }
            reply["id"] = id
            sendSealed(s, reply, payload: payload)
        } catch {
            sendSealed(s, ["type": "error", "id": id, "error": error.localizedDescription])
        }
    }

    private func reject() {
        send(["type": "reject"])
        // The hub frees the slot at once, so the next agent's status is a real arrival.
        resetAgent()
        report(.refused)
    }

    private func resetAgent() {
        // Privacy: no microphone survives an agent leaving, reconnecting or being replaced.
        audio.stopAll()
        agentHere = false
        seal = nil
        challenge = nil
        pairing = nil
        agentGen += 1
    }

    private func report(_ phase: Phase) {
        let host = self.host, id = self.id
        DispatchQueue.main.async { host?.update(id, phase: phase) }
    }

    // MARK: wire

    private func send(_ obj: [String: Any]) {
        guard !closed else { return }
        guard let text = HubConnection.encode(obj) else { return }
        task?.send(.string(text)) { _ in }
    }

    private func sendError(_ id: String, _ err: String) {
        send(["type": "error", "id": id, "error": err])
    }

    /// Seals a reply and, for a frame, its payload as the next box. Nothing suspends between
    /// sealing and enqueueing, and the socket sends in enqueue order, so sequence order is wire order.
    private func sendSealed(_ s: Seal, _ obj: [String: Any], payload: Data? = nil) {
        guard !closed else { return }
        guard let inner = HubConnection.encode(obj),
              let outer = HubConnection.encode(["type": "sealed", "id": obj["id"] as? String ?? "",
                                                "box": s.tx.seal(Data(inner.utf8)).base64EncodedString()]) else { return }
        task?.send(.string(outer)) { _ in }
        if let payload { task?.send(.data(s.tx.seal(payload))) { _ in } }
    }

    private static func encode(_ obj: [String: Any]) -> String? {
        var obj = obj
        obj["v"] = 1
        guard let data = try? JSONSerialization.data(withJSONObject: obj) else { return nil }
        return String(data: data, encoding: .utf8)
    }
}

/// Runs its body at most once, whichever caller comes first.
private final class Once: @unchecked Sendable {
    private let lock = NSLock()
    private var done = false

    func run(_ body: () -> Void) {
        lock.lock()
        let first = !done
        done = true
        lock.unlock()
        if first { body() }
    }
}
