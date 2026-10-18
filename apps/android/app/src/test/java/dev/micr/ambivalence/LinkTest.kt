package dev.micr.ambivalence

import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertThrows
import org.junit.Test
import java.math.BigInteger
import java.security.KeyFactory
import java.security.KeyPair
import java.security.spec.ECPrivateKeySpec

/** The pinned vectors from internal/link/link_test.go, plus a Go-generated P-256 ECDH vector. */
class LinkTest {
    private fun bytes(n: Int, b: Int) = ByteArray(n) { b.toByte() }
    private val a = bytes(65, 1)
    private val p = bytes(65, 2)
    private val nA = bytes(16, 3)
    private val nB = bytes(16, 4)
    private val shared = bytes(32, 5)

    @Test fun commit() = assertEquals(
        "c8f3be02f0acacde00678c4c449775ec01b7d712490e4c17e5c6c9b504e2544d", Link.hex(Link.h("commit", a, nA)))

    @Test fun sas() = assertEquals("V7US6R", Link.sas(a, p, nA, nB))

    @Test fun keyAndMac() {
        val key = Link.key(shared, a, p)
        assertEquals("cf76a39ef4c45ad928dbc050ebefe7ddd0b3b6f07c73d7b62de1676b7abf9b30", Link.hex(key))
        assertEquals(
            "8851d815f3c73fb55dd47226a9b3d95bae443a00f0c872cd3bb0832044e954a9",
            Link.hex(Link.mac(key, "agent", "dev".toByteArray(), nA)),
        )
    }

    @Test fun sealedBox() {
        val (toPhone, toAgent) = Link.channels(shared, "dev", bytes(16, 3), bytes(16, 4))
        val box = Channel(toPhone).seal("hi".toByteArray())
        assertEquals("0000000000000001ee841b7f6af6a77bafbac9ed833fcb93b2ba", Link.hex(box))
        // The receiver opens exactly the next seq, then rejects a replay; the other direction differs.
        val rx = Channel(toPhone)
        assertArrayEquals("hi".toByteArray(), rx.open(box))
        assertThrows(IllegalArgumentException::class.java) { rx.open(box) }
        assertThrows(IllegalArgumentException::class.java) { Channel(toAgent).open(box) }
        val tampered = box.copyOf().also { it[10] = (it[10].toInt() xor 1).toByte() }
        assertThrows(IllegalArgumentException::class.java) { Channel(toPhone).open(tampered) }
    }

    // Generated with Go crypto/ecdh; aPub's X starts with 0x00, exercising left padding.
    private val aPriv = "11000000000000000000000000000000000000000000000000000000000001df"
    private val aPub = "040098c92492e632715073ae266a4f39135bf4932c8cac3924e42aaa13f6c3816afca5180d32329bd0f0a053fae06302ba2a5a20546fc6017b14d88e5b33495572"
    private val bPriv = "2222222222222222222222222222222222222222222222222222222222222222"
    private val bPub = "04d65a93977caa3d1b081852ff57a79e465f1660577304baead505dd3a48589cf350185e895372df6221ea3a137557e473fddb6755f05bd507c3c533fce9c91285"
    private val ecdh = "7f5a4866aac5fcace69213c28b0742d232d6338f99267f8a1d0a161f317bb28c"

    private fun party(priv: String, pub: String): Party {
        val k = KeyFactory.getInstance("EC").generatePrivate(ECPrivateKeySpec(BigInteger(priv, 16), Link.p256))
        return Party(KeyPair(Link.decodePoint(Link.unhex(pub)), k))
    }

    @Test fun ecdhMatchesGo() {
        val pa = party(aPriv, aPub)
        val pb = party(bPriv, bPub)
        assertEquals(aPub, Link.hex(pa.pub))
        assertEquals(bPub, Link.hex(pb.pub))
        assertEquals(ecdh, Link.hex(pa.shared(Link.unhex(bPub))))
        assertEquals(ecdh, Link.hex(pb.shared(Link.unhex(aPub))))
    }

    @Test fun freshPartiesAgree() {
        val x = Party()
        val y = Party()
        assertEquals(65, x.pub.size)
        assertArrayEquals(x.shared(y.pub), y.shared(x.pub))
    }

    @Test fun rejectsOffCurvePoint() {
        assertThrows(IllegalArgumentException::class.java) { Party().shared(a) }
        val bad = Link.unhex(bPub).also { it[64] = (it[64].toInt() xor 1).toByte() }
        assertThrows(IllegalArgumentException::class.java) { Party().shared(bad) }
    }
}
