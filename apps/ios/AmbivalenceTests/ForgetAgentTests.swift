import XCTest
@testable import Ambivalence

/// These tests use the simulator's real Keychain, with no substituted storage or sessions.
final class ForgetAgentTests: XCTestCase {
    // CI uses an erased simulator and loopback hubs without Bonjour. The hosted UI starts with
    // no saved hub, so only this shared test model connects and owns the device's hub sockets.
    @MainActor private static var runningHost: CameraHost?

    override func setUpWithError() throws {
        try Identity().clearKey()
    }

    override func tearDown() async throws {
        if let host = await MainActor.run(body: { Self.runningHost }) {
            await host.forgetAgent()
        }
        try Identity().clearKey()
    }

    func testClearKeyRemovesCachedAndPersistedPairing() throws {
        let identity = Identity()
        let key = Link.random(32)
        try identity.setKey(key)
        XCTAssertEqual(identity.key, key)
        XCTAssertEqual(Identity().key, key)

        try identity.clearKey()

        XCTAssertNil(identity.key)
        XCTAssertNil(Identity().key)
        XCTAssertEqual(Identity().device, identity.device)
    }

    func testClearKeyIsIdempotentAndAllowsPairingAgain() throws {
        let identity = Identity()
        try identity.clearKey()
        try identity.clearKey()

        let replacement = Link.random(32)
        try identity.setKey(replacement)

        XCTAssertEqual(identity.key, replacement)
        XCTAssertEqual(Identity().key, replacement)
    }

    func testAudioStopAllInvalidatesPendingStartupToken() async throws {
        let audio = Audio()
        let owner = "conn-1"
        let token = audio.stopToken(owner: owner)
        // Simulate agentLeft() / stopAll() running while HubConnection is suspended awaiting
        // audio.oneShot before Audio.start publishes a recorder.
        audio.stopAll(owner: owner)
        XCTAssertNotEqual(audio.stopToken(owner: owner), token)

        do {
            _ = try await audio.oneShot(owner: owner, durationMs: 1000, sampleRate: 16000, stopToken: token)
            XCTFail("expected stale stopToken to cancel audio startup")
        } catch {
            XCTAssertTrue(error.localizedDescription.contains("cancelled"), "unexpected error: \(error)")
        }
    }

    @MainActor
    func testStaleAudioCleanupDoesNotDeactivateNewerSessionGeneration() async throws {
        let audio = Audio()
        var events: [Bool] = []
        audio.onRecording = { events.append($0) }

        let oldGen = audio.bumpSessionGenForTesting()
        let newGen = audio.bumpSessionGenForTesting()

        // Cleanup from oldGen arriving after newGen started must be ignored.
        audio.audioFinished(gen: oldGen)
        try await Task.sleep(nanoseconds: 50_000_000)
        XCTAssertEqual(events, [])

        // Cleanup for the current generation still fires.
        audio.audioFinished(gen: newGen)
        try await Task.sleep(nanoseconds: 50_000_000)
        XCTAssertEqual(events, [false])
    }

    @MainActor
    func testForgetAgentRestoresUnlinkedStateAndKeepsHub() async throws {
        let defaults = UserDefaults.standard
        let previousHub = defaults.object(forKey: "hub")
        defer { defaults.set(previousHub, forKey: "hub") }
        let savedHub = "wss://demo.invalid/v1"
        defaults.set(savedHub, forKey: "hub")
        let identity = Identity()
        let device = identity.device
        try identity.setKey(Link.random(32))
        let host = CameraHost()
        XCTAssertTrue(host.linked)

        await host.forgetAgent()

        XCTAssertFalse(host.linked)
        XCTAssertFalse(host.forgettingAgent)
        XCTAssertNil(host.sas)
        XCTAssertNil(Identity().key)
        XCTAssertEqual(Identity().device, device)
        XCTAssertEqual(host.hubURL, savedHub)
        XCTAssertEqual(defaults.string(forKey: "hub"), savedHub)
        XCTAssertEqual(host.code, "------")
        XCTAssertFalse(CameraHost().linked)
    }

    @MainActor
    func testForgetAgentRevokesAnActiveSession() async throws {
        let (host, agent) = try await connectedHost()
        defer { agent.cancel(with: .goingAway, reason: nil) }
        let (key, nA, reply) = try await beginPairing(host, agent)
        host.answer(true)
        _ = try await receive("pair4", from: agent)
        try await waitUntil { host.linked }
        let previousCode = host.code

        await host.forgetAgent()
        try await waitUntil { host.code != "------" && host.code != previousCode }

        XCTAssertFalse(host.linked)
        XCTAssertNil(Identity().key)
        // The hub carries a waiting agent into the phone's replacement room. Its old session
        // key must no longer open camera requests, even though its socket remains connected.
        let keys = Link.channels(key: key, device: Identity().device,
                                 nP: Link.unhex(reply["nonce"] as? String), nA: nA)
        var channel = Channel(key: keys.toPhone)
        let request = try JSONSerialization.data(withJSONObject: ["v": 1, "type": "cameras", "id": "old-session"])
        try await send(["type": "sealed", "id": "old-session", "box": channel.seal(request).base64EncodedString()], to: agent)
        let refusal = try await receive("error", from: agent)
        XCTAssertEqual(refusal["error"] as? String, "agent not authenticated")
    }

    @MainActor
    func testForgetAgentCancelsPendingPairing() async throws {
        let (host, agent) = try await connectedHost()
        defer { agent.cancel(with: .goingAway, reason: nil) }
        _ = try await beginPairing(host, agent)

        await host.forgetAgent()

        XCTAssertNil(host.sas)
        XCTAssertFalse(host.linked)
        XCTAssertNil(Identity().key)
    }

    @MainActor
    func testForgetAgentWaitsForAConfirmedRetiringConnection() async throws {
        let (host, agent) = try await connectedHost()
        defer { agent.cancel(with: .goingAway, reason: nil) }
        _ = try await beginPairing(host, agent)
        // Retire this hub in the same main-actor turn that approves pairing. Its actor may
        // still be resuming the confirmation when forgetting starts.
        host.answer(true)
        host.hubURL = "ws://127.0.0.1:17843/v1"
        host.connect()

        await host.forgetAgent()
        try await waitUntil { host.code != "------" }

        XCTAssertFalse(host.linked)
        XCTAssertNil(host.sas)
        XCTAssertNil(Identity().key)
    }

    @MainActor
    private func connectedHost() async throws -> (CameraHost, URLSessionWebSocketTask) {
        UserDefaults.standard.set("ws://127.0.0.1:17842/v1", forKey: "hub")
        let host: CameraHost
        if let running = Self.runningHost {
            host = running
            host.hubURL = "ws://127.0.0.1:17842/v1"
            host.connect()
            await host.forgetAgent()
        } else {
            host = CameraHost()
            Self.runningHost = host
            host.start()
        }
        try await waitUntil { host.code != "------" }
        let agent = URLSession.shared.webSocketTask(with: URL(string: host.hubURL)!)
        agent.resume()
        try await send(["type": "hello", "role": "agent", "code": host.code], to: agent)
        _ = try await receive("welcome", from: agent)
        return (host, agent)
    }

    @MainActor
    private func beginPairing(_ host: CameraHost, _ agent: URLSessionWebSocketTask) async throws -> (Data, Data, [String: Any]) {
        let party = Party(), nA = Link.nonce()
        try await send(["type": "pair1", "id": "pair1", "commit": Link.hex(Link.H("commit", party.pub, nA))], to: agent)
        let reply = try await receive("pair2", from: agent)
        let phonePub = Link.unhex(reply["pub"] as? String)
        try await send(["type": "pair3", "id": "pair3", "pub": Link.hex(party.pub), "nonce": Link.hex(nA)], to: agent)
        try await waitUntil { host.sas != nil }
        XCTAssertEqual(host.sas, Link.SAS(agentPub: party.pub, phonePub: phonePub,
                                        nA: nA, nB: Link.unhex(reply["nonce"] as? String)))
        return (Link.key(shared: try party.shared(phonePub), agentPub: party.pub, phonePub: phonePub), nA, reply)
    }

    private func send(_ message: [String: Any], to agent: URLSessionWebSocketTask) async throws {
        var message = message
        message["v"] = 1
        let bytes = try JSONSerialization.data(withJSONObject: message)
        try await agent.send(.string(String(decoding: bytes, as: UTF8.self)))
    }

    private func receive(_ type: String, from agent: URLSessionWebSocketTask) async throws -> [String: Any] {
        let deadline = Task {
            do { try await Task.sleep(nanoseconds: 15_000_000_000) } catch { return }
            agent.cancel(with: .goingAway, reason: nil)
        }
        defer { deadline.cancel() }
        while true {
            let frame = try await agent.receive()
            guard case .string(let text) = frame,
                  let message = try JSONSerialization.jsonObject(with: Data(text.utf8)) as? [String: Any] else { continue }
            if message["type"] as? String == type { return message }
        }
    }

    @MainActor
    private func waitUntil(_ condition: () -> Bool) async throws {
        let deadline = Date().addingTimeInterval(15)
        while !condition() {
            guard Date() < deadline else { throw LinkError("timed out waiting for the phone state") }
            try await Task.sleep(nanoseconds: 20_000_000)
        }
    }
}
