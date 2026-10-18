import Foundation

/// Checks Link against the vectors pinned in internal/link/link_test.go. Debug builds run it at
/// launch; it also runs standalone on macOS (swiftc Link.swift LinkCheck.swift + a main).
enum LinkCheck {
    /// Every mismatch, or empty when this port agrees with the Go reference.
    static func failures() -> [String] {
        var out: [String] = []
        func expect(_ name: String, _ got: String, _ want: String) {
            if got != want { out.append("\(name): got \(got), want \(want)") }
        }
        let a = Data(repeating: 0x01, count: 65), p = Data(repeating: 0x02, count: 65)
        let nA = Data(repeating: 0x03, count: 16), nB = Data(repeating: 0x04, count: 16)
        let shared = Data(repeating: 0x05, count: 32)

        expect("commit", Link.hex(Link.H("commit", a, nA)), "c8f3be02f0acacde00678c4c449775ec01b7d712490e4c17e5c6c9b504e2544d")
        expect("sas", Link.SAS(agentPub: a, phonePub: p, nA: nA, nB: nB), "V7US6R")
        let key = Link.key(shared: shared, agentPub: a, phonePub: p)
        expect("key", Link.hex(key), "cf76a39ef4c45ad928dbc050ebefe7ddd0b3b6f07c73d7b62de1676b7abf9b30")
        expect("mac", Link.hex(Link.MAC(key, "agent", Data("dev".utf8), nA)), "8851d815f3c73fb55dd47226a9b3d95bae443a00f0c872cd3bb0832044e954a9")

        // The agent seals with toPhone; the phone's Seal opens it with rx, exactly once, in order.
        let ch = Link.channels(key: shared, device: "dev", nP: nA, nA: nB)
        var agentTx = Channel(key: ch.toPhone)
        let box1 = agentTx.seal(Data("hi".utf8))
        expect("box", Link.hex(box1), "0000000000000001ee841b7f6af6a77bafbac9ed833fcb93b2ba")
        var box2 = agentTx.seal(Data("again".utf8))
        let phone = Seal(key: shared, device: "dev", nP: nA, nA: nB)
        expect("skip", (try? phone.rx.open(box2)) == nil ? "refused" : "accepted", "refused")
        expect("open", (try? phone.rx.open(box1)).map { String(decoding: $0, as: UTF8.self) } ?? "refused", "hi")
        expect("replay", (try? phone.rx.open(box1)) == nil ? "refused" : "accepted", "refused")
        box2[box2.count - 1] ^= 1
        expect("tamper", (try? phone.rx.open(box2)) == nil ? "refused" : "accepted", "refused")
        box2[box2.count - 1] ^= 1
        expect("next", (try? phone.rx.open(box2)).map { String(decoding: $0, as: UTF8.self) } ?? "refused", "again")

        // The phone's replies open on the agent's side (toAgent).
        var agentRx = Channel(key: ch.toAgent)
        let reply = phone.tx.seal(Data("ok".utf8))
        expect("reply", (try? agentRx.open(reply)).map { String(decoding: $0, as: UTF8.self) } ?? "refused", "ok")

        let x = Party(), y = Party()
        expect("pub", "\(x.pub.count) \(x.pub.first ?? 0)", "65 4")
        let xy = (try? x.shared(y.pub)).map(Link.hex) ?? "error", yx = (try? y.shared(x.pub)).map(Link.hex) ?? "error"
        expect("ecdh", xy, yx)
        expect("ecdh length", "\(xy.count)", "64")
        expect("bad point", (try? x.shared(a)) == nil ? "refused" : "accepted", "refused")

        expect("unhex", Link.hex(Link.unhex("00ff7A")), "00ff7a")
        expect("unhex bad", "\(Link.unhex("0g").count) \(Link.unhex("abc").count)", "0 0")
        expect("equal", "\(Link.equal(nA, nA)) \(Link.equal(nA, nB)) \(Link.equal(nA, nA.prefix(8)))", "true false false")
        return out
    }
}
