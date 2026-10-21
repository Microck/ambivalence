import AVFoundation
import Foundation

/// The app model: one identity and one camera, served to every known hub at once. Hubs are the
/// saved URL (typed or from a QR link) plus those found on this Wi-Fi, at most `maxHubs` in all;
/// each socket is a HubConnection with its own agent state. Discovery never overwrites the saved URL.
/// All state here lives on the main queue (hence @unchecked Sendable).
final class CameraHost: NSObject, ObservableObject, @unchecked Sendable {
    @Published var code = "------"
    @Published var status = "starting camera"
    @Published var hubURL: String
    @Published var torchOn = false
    @Published var linked: Bool
    @Published var recording = false
    @Published private(set) var forgettingAgent = false
    /// The SAS of a pairing waiting for the user to confirm or cancel.
    @Published var sas: String?

    let camera = Camera()
    let audio = Audio()
    var session: AVCaptureSession { camera.session }

    private static let maxHubs = 4
    private let identity: Identity
    private var saved: String
    private var started = false
    private let browser = NetServiceBrowser()
    private var resolving: [NetService] = []
    private var found: [(name: String, urls: [String])] = [] // discovered hubs, in discovery order
    private var slots: [String: Slot] = [:] // by hub key: the saved URL, or "bonjour:" + service name
    private var retiring: [UUID: HubConnection] = [:] // retained until their actor has closed
    private var dials = 0
    private var prompts: [Prompt] = []

    /// One wanted hub. hubId is the hub's self-reported process id from welcome; it is only
    /// used to avoid two sockets to the same hub evicting each other, never as a trust signal.
    private struct Slot {
        var candidates: [String] // ws URLs of this hub, tried in order on each dial
        var reached: String? // the candidate that last worked; dialed first next time
        var conn: HubConnection?
        var phase = HubConnection.Phase.connecting
        var welcomed = false // the live connection got its welcome
        var code: String?
        var hubId: String?
        var since = 0 // dial order of the live connection
        var parkedBehind: String? // same hub as this key; do not dial while it holds the hub

        init(candidates: [String]) { self.candidates = candidates }

        var order: [String] {
            guard let r = reached, candidates.contains(r) else { return candidates }
            return [r] + candidates.filter { $0 != r }
        }
    }

    private struct Prompt {
        let id = UUID()
        let conn: UUID
        let sas: String
        let done: CheckedContinuation<Bool, Never>
    }

    override init() {
        #if DEBUG
        let failures = LinkCheck.failures()
        assert(failures.isEmpty, "link vectors: \(failures)")
        #endif
        identity = Identity()
        linked = identity.key != nil
        saved = UserDefaults.standard.string(forKey: "hub") ?? ""
        hubURL = saved
        super.init()
        audio.onRecording = { [weak self] on in
            DispatchQueue.main.async { self?.recording = on }
        }
    }

    func start() {
        guard !started else { return }
        started = true
        camera.onTorch = { [weak self] in self?.torchOn = $0 }
        AVAudioApplication.requestRecordPermission { _ in }
        AVCaptureDevice.requestAccess(for: .video) { ok in
            DispatchQueue.main.async {
                guard ok else {
                    self.status = "camera permission denied"
                    return
                }
                Task {
                    if let err = await self.camera.start() { DispatchQueue.main.async { self.status = err } }
                }
                self.browser.delegate = self
                self.browser.searchForServices(ofType: "_ambivalence._tcp.", inDomain: "local.")
                self.reconcile()
            }
        }
    }

    /// Saves the typed (or QR) URL as the saved hub and connects to it now.
    func connect() {
        let raw = hubURL.trimmingCharacters(in: .whitespaces)
        guard raw.hasPrefix("ws://") || raw.hasPrefix("wss://"), URL(string: raw) != nil else {
            status = "hub URL must start with ws:// or wss://"
            return
        }
        hubURL = raw
        saved = raw
        UserDefaults.standard.set(raw, forKey: "hub")
        guard started else { return }
        slots[raw]?.parkedBehind = nil // the saved hub is never parked
        reconcile()
        dial(raw) // retry at once if it was down
    }

    func flip() {
        Task {
            do { try await camera.flip() } catch { DispatchQueue.main.async { self.status = error.localizedDescription } }
        }
    }

    func toggleTorch() {
        Task {
            do { try await camera.toggleTorch() } catch { DispatchQueue.main.async { self.status = error.localizedDescription } }
        }
    }

    /// Close all sessions before deleting their shared key. Old actors and queued prompts must
    /// finish closing before discovery may open replacements or another pairing may save a key.
    @MainActor
    func forgetAgent() async {
        guard !forgettingAgent else { return }
        forgettingAgent = true
        let connections = slots.values.compactMap(\.conn) + Array(retiring.values)
        slots.removeAll()
        resolve { _ in true }
        refresh()
        for connection in connections { await connection.close() }

        var deletionError: Error?
        do { try identity.clearKey() } catch { deletionError = error }
        linked = identity.key != nil
        // Discovery can change while close() suspends. Rebuild from the current wanted hubs.
        slots.removeAll()
        forgettingAgent = false
        if started { reconcile() } else { refresh() }
        if let deletionError { status = "could not forget agent: \(deletionError.localizedDescription)" }
    }

    /// The user's answer on the pairing screen.
    func answer(_ ok: Bool) {
        guard !prompts.isEmpty else { return }
        let p = prompts.removeFirst()
        sas = prompts.first?.sas
        p.done.resume(returning: ok)
    }

    // MARK: called by connections

    /// Asks the user to compare codes; true on Confirm. Prompts from several hubs queue up, and
    /// each counts as Cancel after 120 s or once its connection's agent leaves.
    func confirm(_ sas: String, from conn: UUID) async -> Bool {
        await withCheckedContinuation { cont in
            DispatchQueue.main.async {
                guard self.slots.values.contains(where: { $0.conn?.id == conn }) else { return cont.resume(returning: false) }
                let p = Prompt(conn: conn, sas: sas, done: cont)
                self.prompts.append(p)
                self.sas = self.prompts.first?.sas
                DispatchQueue.main.asyncAfter(deadline: .now() + 120) { self.resolve { $0.id == p.id } }
            }
        }
    }

    /// Cancels any open pairing prompt from this connection.
    func cancelPrompts(for conn: UUID) {
        resolve { $0.conn == conn }
    }

    /// A multi-address connection found a working candidate.
    func reached(_ conn: UUID, url: String) {
        guard let key = key(of: conn) else { return }
        slots[key]?.reached = url
    }

    func welcome(_ conn: UUID, code: String, hubId: String) {
        guard let url = key(of: conn) else { return }
        slots[url]?.welcomed = true
        slots[url]?.code = code.isEmpty ? nil : code
        if !hubId.isEmpty {
            slots[url]?.hubId = hubId
            dedupe(url, hubId)
        }
        refresh()
    }

    func update(_ conn: UUID, phase: HubConnection.Phase) {
        guard let url = key(of: conn) else { return }
        slots[url]?.phase = phase
        refresh()
    }

    func linkChanged() {
        linked = identity.key != nil
        refresh()
    }

    func connectionClosed(_ conn: UUID) {
        cancelPrompts(for: conn)
        guard let url = key(of: conn), var s = slots[url] else { return }
        let hadHub = s.welcomed
        s.conn = nil
        s.welcomed = false
        s.code = nil
        s.phase = .lost
        slots[url] = s
        // A discovered URL that reaches the same hub as a live connection stays parked.
        if url != saved, let h = s.hubId,
           let keeper = slots.first(where: { $0.key != url && $0.value.welcomed && $0.value.hubId == h })?.key {
            park(url, behind: keeper)
        } else {
            redial(url)
            // URLs parked behind this one wait while it redials after a working session; if it
            // never reached the hub this time, they may try instead.
            if !hadHub { release(behind: url) }
        }
        refresh()
    }

    // MARK: connections

    /// The saved URL first, then discovered hubs, each URL used once, at most maxHubs, as
    /// (key, candidate URLs).
    private var wanted: [(key: String, urls: [String])] {
        var out: [(key: String, urls: [String])] = saved.isEmpty ? [] : [(saved, [saved])]
        var used = Set(out.flatMap(\.urls))
        for f in found {
            let urls = f.urls.filter { !used.contains($0) }
            guard !urls.isEmpty else { continue }
            used.formUnion(urls)
            out.append(("bonjour:" + f.name, urls))
        }
        return Array(out.prefix(CameraHost.maxHubs))
    }

    private func reconcile() {
        let want = wanted
        for key in slots.keys where !want.contains(where: { $0.key == key }) {
            if let c = slots[key]?.conn { drop(c) }
            slots[key] = nil
            release(behind: key)
        }
        for w in want {
            if slots[w.key] == nil {
                slots[w.key] = Slot(candidates: w.urls)
                dial(w.key)
            } else {
                slots[w.key]?.candidates = w.urls // used from the next dial
            }
        }
        refresh()
    }

    private func dial(_ key: String) {
        guard started, !forgettingAgent, var s = slots[key], s.conn == nil, s.parkedBehind == nil else { return }
        let urls = s.order.compactMap(URL.init(string:))
        guard !urls.isEmpty else { return }
        dials += 1
        let c = HubConnection(candidates: urls, identity: identity, camera: camera, audio: audio, host: self)
        s.conn = c
        s.welcomed = false
        s.code = nil
        s.phase = .connecting
        s.since = dials
        slots[key] = s
        Task { await c.run() }
    }

    /// Stay connected across network blips: retry until the URL is no longer wanted.
    private func redial(_ url: String) {
        DispatchQueue.main.asyncAfter(deadline: .now() + 3) { self.dial(url) }
    }

    /// Keeps one live connection per hubId. The saved URL wins (a rogue hub can claim any id, so
    /// the saved hub is never given up for it); otherwise the connection already there wins.
    private func dedupe(_ url: String, _ hubId: String) {
        for (other, o) in slots where other != url && o.hubId == hubId && o.parkedBehind == nil {
            if o.welcomed {
                if url == saved { park(other, behind: url) } else { park(url, behind: other); return }
            } else if other != saved {
                park(other, behind: url) // waiting to redial: it would only evict this one
            }
        }
    }

    private func park(_ url: String, behind keeper: String) {
        guard var s = slots[url] else { return }
        if let c = s.conn { drop(c) }
        s.conn = nil
        s.welcomed = false
        s.code = nil
        s.phase = .lost
        s.parkedBehind = keeper
        slots[url] = s
        for (u, o) in slots where o.parkedBehind == url { slots[u]?.parkedBehind = keeper }
    }

    private func release(behind url: String) {
        for (u, o) in slots where o.parkedBehind == url {
            slots[u]?.parkedBehind = nil
            redial(u)
        }
    }

    private func drop(_ c: HubConnection) {
        cancelPrompts(for: c.id)
        // A removed slot can still have a confirmed pairing waiting to resume on its actor.
        // Forgetting must wait for that actor too, so it cannot save a key after deletion.
        retiring[c.id] = c
        Task { @MainActor in
            await c.close()
            retiring[c.id] = nil
        }
    }

    private func resolve(cancelling match: (Prompt) -> Bool) {
        let gone = prompts.filter(match)
        guard !gone.isEmpty else { return }
        prompts.removeAll(where: match)
        sas = prompts.first?.sas
        for p in gone { p.done.resume(returning: false) }
    }

    private func key(of conn: UUID) -> String? {
        slots.first(where: { $0.value.conn?.id == conn })?.key
    }

    /// Shows the most useful state across hubs and the pair code of the saved hub, else of the
    /// first hub that answered.
    private func refresh() {
        guard !forgettingAgent else {
            code = "------"
            status = "forgetting agent"
            return
        }
        let live = slots.filter { $0.value.parkedBehind == nil }
        let shown = live[saved].flatMap { $0.code != nil ? $0 : nil }
            ?? live.values.filter { $0.code != nil }.min { $0.since < $1.since }
        code = shown?.code ?? "------"
        guard let best = live.values.map(\.phase).max() else {
            status = saved.isEmpty ? "looking for your agent on this Wi-Fi" : "connecting"
            return
        }
        let hubs = live.values.filter(\.welcomed).count
        switch best {
        case .agentLinked: status = "paired, agent can see this camera and hear this microphone"
        case .agentConnecting: status = "agent connecting"
        case .refused: status = "refused an agent that could not prove the link"
        case .waiting: status = hubs > 1 ? "connected to \(hubs) hubs, waiting for the agent" : "connected, waiting for the agent"
        case .connecting: status = "connecting to " + (live.count == 1 ? live.values.first?.order.first.flatMap { URL(string: $0)?.host } ?? "the hub" : "\(live.count) hubs")
        case .lost: status = "lost the hub, reconnecting"
        }
    }
}

/// Same-Wi-Fi discovery: hubs advertise `_ambivalence._tcp`. Each found hub gets its own
/// connection next to the saved one; discovery confers no trust and never replaces the saved URL.
extension CameraHost: NetServiceBrowserDelegate, NetServiceDelegate {
    func netServiceBrowser(_ browser: NetServiceBrowser, didFind service: NetService, moreComing: Bool) {
        resolving.append(service) // NetService must be retained while it resolves
        service.delegate = self
        service.resolve(withTimeout: 5)
    }

    func netServiceBrowser(_ browser: NetServiceBrowser, didRemove service: NetService, moreComing: Bool) {
        found.removeAll { $0.name == service.name }
        if !moreComing { reconcile() }
    }

    // Connect by IP, never the advertised .local hostname (it may not resolve; see hub advertise).
    // Every address is a candidate for this one hub, in order: the hub's own addrs= TXT list,
    // then whatever Bonjour resolved. A multihomed hub may list addresses this phone cannot reach.
    func netServiceDidResolveAddress(_ sender: NetService) {
        resolving.removeAll { $0 === sender }
        guard sender.port > 0 else { return }
        let txt = sender.txtRecordData().map(NetService.dictionary(fromTXTRecord:)) ?? [:]
        let listed = txt["addrs"].flatMap { String(data: $0, encoding: .utf8) }?.split(separator: ",").map(String.init) ?? []
        var urls: [String] = []
        for ip in listed + (sender.addresses?.compactMap(CameraHost.ipv4) ?? []) {
            let url = "ws://\(ip):\(sender.port)/v1"
            if !ip.isEmpty, !urls.contains(url) { urls.append(url) }
        }
        guard !urls.isEmpty else { return }
        if let i = found.firstIndex(where: { $0.name == sender.name }) {
            found[i].urls = urls
        } else {
            found.append((sender.name, urls))
        }
        reconcile()
    }

    func netService(_ sender: NetService, didNotResolve errorDict: [String: NSNumber]) {
        resolving.removeAll { $0 === sender }
    }

    /// The dotted IPv4 address inside a resolved sockaddr, skipping link-local 169.254/16.
    static func ipv4(_ data: Data) -> String? {
        data.withUnsafeBytes { raw -> String? in
            guard raw.count >= MemoryLayout<sockaddr_in>.size,
                  raw.load(as: sockaddr.self).sa_family == sa_family_t(AF_INET) else { return nil }
            var addr = raw.load(as: sockaddr_in.self).sin_addr
            var buf = [CChar](repeating: 0, count: Int(INET_ADDRSTRLEN))
            guard inet_ntop(AF_INET, &addr, &buf, socklen_t(buf.count)) != nil else { return nil }
            let s = String(cString: buf)
            return s.hasPrefix("169.254.") ? nil : s
        }
    }
}
