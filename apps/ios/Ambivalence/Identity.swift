import Foundation
import Security

/// The phone's one identity, shared by every hub connection. `device` is a public id kept in
/// UserDefaults. The link key from pairing is a secret: it lives in the Keychain (this device
/// only, after first unlock), is never sent to a hub, and is nil until the first pairing.
final class Identity: @unchecked Sendable { // key is guarded by lock; device is immutable
    let device: String
    private let lock = NSLock()
    private var cached: Data?

    private static let service = "dev.micr.ambivalence"
    private static let account = "link-key"

    init() {
        if let d = UserDefaults.standard.string(forKey: "device") {
            device = d
        } else {
            device = Link.hex(Link.random(16))
            UserDefaults.standard.set(device, forKey: "device")
        }
        cached = Identity.load()
    }

    var key: Data? {
        lock.lock()
        defer { lock.unlock() }
        return cached
    }

    /// Stores a new link key, replacing any earlier pairing.
    func setKey(_ key: Data) throws {
        lock.lock()
        defer { lock.unlock() }
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: Identity.service,
            kSecAttrAccount as String: Identity.account,
        ]
        SecItemDelete(query as CFDictionary)
        var add = query
        add[kSecValueData as String] = key
        add[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
        let status = SecItemAdd(add as CFDictionary, nil)
        guard status == errSecSuccess else { throw LinkError("could not store the link key (keychain \(status))") }
        cached = key
    }

    private static func load() -> Data? {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
            kSecReturnData as String: true,
            kSecMatchLimit as String: kSecMatchLimitOne,
        ]
        var out: CFTypeRef?
        guard SecItemCopyMatching(query as CFDictionary, &out) == errSecSuccess, let d = out as? Data, d.count == 32 else { return nil }
        return d
    }
}
