package dev.micr.ambivalence

import java.math.BigInteger
import java.security.AlgorithmParameters
import java.security.KeyFactory
import java.security.KeyPair
import java.security.KeyPairGenerator
import java.security.MessageDigest
import java.security.SecureRandom
import java.security.interfaces.ECPublicKey
import java.security.spec.ECGenParameterSpec
import java.security.spec.ECParameterSpec
import java.security.spec.ECPoint
import java.security.spec.ECPublicKeySpec
import java.security.spec.ECFieldFp
import javax.crypto.Cipher
import javax.crypto.KeyAgreement
import javax.crypto.Mac
import javax.crypto.spec.GCMParameterSpec
import javax.crypto.spec.SecretKeySpec

/**
 * End-to-end trust between this phone and its agent; a port of internal/link (keep in sync,
 * vectors in LinkTest). Hubs only relay and never see the key.
 */
object Link {
    private const val SAS_ALPHABET = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
    private val random = SecureRandom()

    /** SHA-256 over "ambivalence-v1/"+label, then each field as a 4-byte big-endian length and its bytes. */
    fun h(label: String, vararg fields: ByteArray): ByteArray {
        val d = MessageDigest.getInstance("SHA-256")
        d.update("ambivalence-v1/$label".toByteArray(Charsets.UTF_8))
        for (f in fields) {
            val n = f.size
            d.update(byteArrayOf((n ushr 24).toByte(), (n ushr 16).toByte(), (n ushr 8).toByte(), n.toByte()))
            d.update(f)
        }
        return d.digest()
    }

    /** HMAC-SHA256(key, H(label, fields...)). */
    fun mac(key: ByteArray, label: String, vararg fields: ByteArray): ByteArray {
        val m = Mac.getInstance("HmacSHA256")
        m.init(SecretKeySpec(key, "HmacSHA256"))
        return m.doFinal(h(label, *fields))
    }

    /** The first 30 bits of H("sas", ...) as 6 characters of the pair-code alphabet. */
    fun sas(agentPub: ByteArray, phonePub: ByteArray, nA: ByteArray, nB: ByteArray): String {
        val d = h("sas", agentPub, phonePub, nA, nB)
        val v = ((d[0].toLong() and 0xff) shl 24) or ((d[1].toLong() and 0xff) shl 16) or
            ((d[2].toLong() and 0xff) shl 8) or (d[3].toLong() and 0xff)
        return String(CharArray(6) { i -> SAS_ALPHABET[((v ushr (27 - 5 * i)) and 31).toInt()] })
    }

    /** The long-term link key from the ECDH secret and both public keys. */
    fun key(shared: ByteArray, agentPub: ByteArray, phonePub: ByteArray) = h("key", shared, agentPub, phonePub)

    fun nonce(): ByteArray = ByteArray(16).also { random.nextBytes(it) }

    /** Constant-time comparison. */
    fun equal(a: ByteArray, b: ByteArray) = MessageDigest.isEqual(a, b)

    fun hex(b: ByteArray): String {
        val out = CharArray(b.size * 2)
        for (i in b.indices) {
            val v = b[i].toInt() and 0xff
            out[2 * i] = HEX[v ushr 4]
            out[2 * i + 1] = HEX[v and 15]
        }
        return String(out)
    }

    /** Decodes lowercase or uppercase hex; malformed input yields an empty array, which every length check rejects. */
    fun unhex(s: String): ByteArray {
        if (s.length % 2 != 0) return ByteArray(0)
        val out = ByteArray(s.length / 2)
        for (i in out.indices) {
            val hi = Character.digit(s[2 * i], 16)
            val lo = Character.digit(s[2 * i + 1], 16)
            if (hi < 0 || lo < 0) return ByteArray(0)
            out[i] = ((hi shl 4) or lo).toByte()
        }
        return out
    }

    private const val HEX = "0123456789abcdef"

    /** Agent->phone and phone->agent sealing keys for one session. */
    fun channels(key: ByteArray, device: String, nP: ByteArray, nA: ByteArray): Pair<ByteArray, ByteArray> {
        val sk = h("session", key, device.toByteArray(Charsets.UTF_8), nP, nA)
        return h("seal-agent", sk) to h("seal-phone", sk)
    }

    internal val p256: ECParameterSpec by lazy {
        AlgorithmParameters.getInstance("EC").run {
            init(ECGenParameterSpec("secp256r1"))
            getParameterSpec(ECParameterSpec::class.java)
        }
    }

    /** Uncompressed point encoding: 0x04 || X || Y, each coordinate left-padded to 32 bytes. */
    fun encodePoint(w: ECPoint): ByteArray {
        val out = ByteArray(65)
        out[0] = 4
        fixed32(w.affineX).copyInto(out, 1)
        fixed32(w.affineY).copyInto(out, 33)
        return out
    }

    /** Parses an uncompressed P-256 point and checks it lies on the curve. */
    fun decodePoint(b: ByteArray): ECPublicKey {
        require(b.size == 65 && b[0] == 4.toByte()) { "bad peer key" }
        val x = BigInteger(1, b.copyOfRange(1, 33))
        val y = BigInteger(1, b.copyOfRange(33, 65))
        val curve = p256.curve
        val p = (curve.field as ECFieldFp).p
        require(x < p && y < p) { "bad peer key" }
        require(y.multiply(y).mod(p) == x.pow(3).add(curve.a.multiply(x)).add(curve.b).mod(p)) { "bad peer key" }
        return KeyFactory.getInstance("EC").generatePublic(ECPublicKeySpec(ECPoint(x, y), p256)) as ECPublicKey
    }

    internal fun fixed32(v: BigInteger): ByteArray {
        val raw = v.toByteArray() // big-endian, may carry a leading sign byte or be short
        val out = ByteArray(32)
        val n = minOf(raw.size, 32)
        raw.copyInto(out, 32 - n, raw.size - n, raw.size)
        return out
    }
}

/** One side's ephemeral P-256 key for a pairing. */
class Party internal constructor(private val kp: KeyPair) {
    val pub: ByteArray = Link.encodePoint((kp.public as ECPublicKey).w)

    constructor() : this(KeyPairGenerator.getInstance("EC").run {
        initialize(ECGenParameterSpec("secp256r1"))
        generateKeyPair()
    })

    /** ECDH with the peer's uncompressed public key: the 32-byte X coordinate. */
    fun shared(peerPub: ByteArray): ByteArray {
        val ka = KeyAgreement.getInstance("ECDH")
        ka.init(kp.private)
        ka.doPhase(Link.decodePoint(peerPub), true)
        return Link.fixed32(BigInteger(1, ka.generateSecret()))
    }
}

/**
 * Seals or opens one direction. A box is seq (8 bytes BE) || AES-256-GCM ciphertext+tag, nonce
 * 4 zero bytes || seq, the seq bytes as additional data. [open] accepts only seq = last+1.
 * Not thread-safe; the sender must keep seal order equal to wire order.
 */
class Channel(key: ByteArray) {
    private val key = SecretKeySpec(key, "AES")
    private val cipher = Cipher.getInstance("AES/GCM/NoPadding")
    private var seq = 0L // sealer: last used; opener: last accepted

    fun seal(plain: ByteArray): ByteArray {
        seq++
        val head = seqBytes(seq)
        cipher.init(Cipher.ENCRYPT_MODE, key, GCMParameterSpec(128, nonce(head)))
        cipher.updateAAD(head)
        val ct = cipher.doFinal(plain)
        return head + ct
    }

    fun open(box: ByteArray): ByteArray {
        require(box.size >= 8 + 16) { "sealed message too short" }
        val head = box.copyOfRange(0, 8)
        var got = 0L
        for (b in head) got = (got shl 8) or (b.toLong() and 0xff)
        // Exactly the next number: any gap or repeat is the hub replaying, dropping or splicing.
        require(got == seq + 1) { "sealed message out of order (replayed, dropped or spliced)" }
        val plain = try {
            cipher.init(Cipher.DECRYPT_MODE, key, GCMParameterSpec(128, nonce(head)))
            cipher.updateAAD(head)
            cipher.doFinal(box, 8, box.size - 8)
        } catch (e: java.security.GeneralSecurityException) {
            throw IllegalArgumentException("sealed message failed authentication")
        }
        seq = got
        return plain
    }

    private fun seqBytes(n: Long) = ByteArray(8) { i -> (n ushr (56 - 8 * i)).toByte() }
    private fun nonce(head: ByteArray) = ByteArray(12).also { head.copyInto(it, 4) }
}

/** A session's two channels as the phone sees them: open what the agent sent, seal what goes back. */
class Seal(key: ByteArray, device: String, nP: ByteArray, nA: ByteArray) {
    val rx: Channel
    val tx: Channel

    init {
        val (toPhone, toAgent) = Link.channels(key, device, nP, nA)
        rx = Channel(toPhone)
        tx = Channel(toAgent)
    }
}
