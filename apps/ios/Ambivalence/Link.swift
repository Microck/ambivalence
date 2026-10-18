import CryptoKit
import Foundation
import Security

/// The end-to-end link between this phone and its agent; a Swift port of internal/link (see
/// docs/protocol.md). Hubs only relay: they never see the link key, so any hub may be rogue.
/// Keep this in sync with the Go, Android and web implementations.
enum Link {
    private static let alphabet = Array("23456789ABCDEFGHJKLMNPQRSTUVWXYZ")

    /// SHA-256 over "ambivalence-v1/"+label, then each field as a 4-byte big-endian length and its bytes.
    static func H(_ label: String, _ fields: Data...) -> Data { hash(label, fields) }

    /// HMAC-SHA256(key, H(label, fields...)).
    static func MAC(_ key: Data, _ label: String, _ fields: Data...) -> Data {
        Data(HMAC<SHA256>.authenticationCode(for: hash(label, fields), using: SymmetricKey(data: key)))
    }

    /// The first 30 bits of H("sas", ...) as 6 characters of the pair-code alphabet.
    static func SAS(agentPub: Data, phonePub: Data, nA: Data, nB: Data) -> String {
        let d = [UInt8](H("sas", agentPub, phonePub, nA, nB))
        let v = UInt32(d[0]) << 24 | UInt32(d[1]) << 16 | UInt32(d[2]) << 8 | UInt32(d[3])
        return String((0..<6).map { alphabet[Int((v >> (27 - 5 * UInt32($0))) & 31)] })
    }

    /// The long-term link key from the ECDH secret and both public keys.
    static func key(shared: Data, agentPub: Data, phonePub: Data) -> Data { H("key", shared, agentPub, phonePub) }

    /// The agent->phone and phone->agent sealing keys for one session.
    static func channels(key: Data, device: String, nP: Data, nA: Data) -> (toPhone: Data, toAgent: Data) {
        let sk = H("session", key, Data(device.utf8), nP, nA)
        return (H("seal-agent", sk), H("seal-phone", sk))
    }

    static func nonce() -> Data { random(16) }

    static func random(_ n: Int) -> Data {
        var b = [UInt8](repeating: 0, count: n)
        // The system RNG failing is unrecoverable; continuing would reuse predictable nonces.
        guard SecRandomCopyBytes(kSecRandomDefault, n, &b) == errSecSuccess else { fatalError("system RNG failed") }
        return Data(b)
    }

    /// Constant-time comparison; different lengths never match.
    static func equal(_ a: Data, _ b: Data) -> Bool {
        guard a.count == b.count else { return false }
        return zip(a, b).reduce(UInt8(0)) { $0 | ($1.0 ^ $1.1) } == 0
    }

    static func hex(_ d: Data) -> String { d.map { String(format: "%02x", $0) }.joined() }

    /// Decodes a hex field; anything malformed decodes to empty bytes, which never verifies.
    static func unhex(_ s: String?) -> Data {
        guard let s, s.count % 2 == 0 else { return Data() }
        var out = Data(capacity: s.count / 2)
        var it = s.utf8.makeIterator()
        while let hi = it.next(), let lo = it.next() {
            guard let h = nibble(hi), let l = nibble(lo) else { return Data() }
            out.append(h << 4 | l)
        }
        return out
    }

    private static func nibble(_ c: UInt8) -> UInt8? {
        switch c {
        case UInt8(ascii: "0")...UInt8(ascii: "9"): return c - UInt8(ascii: "0")
        case UInt8(ascii: "a")...UInt8(ascii: "f"): return c - UInt8(ascii: "a") + 10
        case UInt8(ascii: "A")...UInt8(ascii: "F"): return c - UInt8(ascii: "A") + 10
        default: return nil
        }
    }

    private static func hash(_ label: String, _ fields: [Data]) -> Data {
        var h = SHA256()
        h.update(data: Data(("ambivalence-v1/" + label).utf8))
        for f in fields {
            withUnsafeBytes(of: UInt32(f.count).bigEndian) { h.update(bufferPointer: $0) }
            h.update(data: f)
        }
        return Data(h.finalize())
    }
}

/// One side's ephemeral P-256 key for a pairing.
struct Party {
    private let priv = P256.KeyAgreement.PrivateKey()
    /// Uncompressed point, 65 bytes.
    var pub: Data { priv.publicKey.x963Representation }

    /// ECDH with the peer's uncompressed public key; throws on a point not on the curve.
    func shared(_ peerPub: Data) throws -> Data {
        let peer = try P256.KeyAgreement.PublicKey(x963Representation: peerPub)
        return try priv.sharedSecretFromKeyAgreement(with: peer).withUnsafeBytes { Data($0) }
    }
}

struct LinkError: Error, LocalizedError {
    let message: String
    init(_ message: String) { self.message = message }
    var errorDescription: String? { message }
}

/// One direction of a sealed session. A box is seq (8 bytes, big-endian) || AES-256-GCM
/// ciphertext || tag, with nonce 4 zero bytes || seq and the seq bytes as additional data.
/// open accepts only seq = last+1. Not thread-safe: the sender keeps seal order equal to wire order.
struct Channel {
    private let key: SymmetricKey
    private var seq: UInt64 = 0 // sealer: last used; opener: last accepted

    init(key: Data) { self.key = SymmetricKey(data: key) }

    mutating func seal(_ plain: Data) -> Data {
        seq += 1
        let sb = Channel.seqBytes(seq)
        // Sealing with a 32-byte key and 12-byte nonce cannot fail.
        let box = try! AES.GCM.seal(plain, using: key, nonce: Channel.nonce(sb), authenticating: sb)
        return sb + box.ciphertext + box.tag
    }

    mutating func open(_ box: Data) throws -> Data {
        let box = Data(box) // rebase indices to 0
        guard box.count >= 8 + 16 else { throw LinkError("sealed message too short") }
        let sb = box.prefix(8)
        let next = sb.reduce(UInt64(0)) { $0 << 8 | UInt64($1) }
        // Exactly the next number: the relay never drops or reorders legitimately, so any gap or
        // repeat is a hub replaying, dropping or splicing (e.g. a JPEG onto another reply).
        guard next == seq &+ 1 else { throw LinkError("sealed message out of order (replayed, dropped or spliced)") }
        let sealed = try AES.GCM.SealedBox(nonce: Channel.nonce(sb), ciphertext: box[8..<(box.count - 16)], tag: box.suffix(16))
        guard let plain = try? AES.GCM.open(sealed, using: key, authenticating: sb) else {
            throw LinkError("sealed message failed authentication")
        }
        seq = next
        return plain
    }

    private static func seqBytes(_ n: UInt64) -> Data { withUnsafeBytes(of: n.bigEndian) { Data($0) } }

    private static func nonce(_ seqBytes: Data) -> AES.GCM.Nonce {
        try! AES.GCM.Nonce(data: Data(count: 4) + seqBytes) // always 12 bytes
    }
}

/// Both directions of one agent session, as the phone sees them.
final class Seal {
    var tx: Channel // phone -> agent
    var rx: Channel // agent -> phone

    init(key: Data, device: String, nP: Data, nA: Data) {
        let ch = Link.channels(key: key, device: device, nP: nP, nA: nA)
        tx = Channel(key: ch.toAgent)
        rx = Channel(key: ch.toPhone)
    }
}
