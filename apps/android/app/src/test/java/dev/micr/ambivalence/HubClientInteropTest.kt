package dev.micr.ambivalence

import android.content.SharedPreferences
import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Assume.assumeTrue
import org.junit.Test
import java.io.File
import java.net.ServerSocket
import java.net.Socket
import java.nio.file.Files
import java.util.concurrent.CompletableFuture
import java.util.concurrent.TimeUnit

/**
 * Pairs, authenticates and captures through the real Go hub and agent CLI. Opt in with
 * AMBIVALENCE_BIN=/path/to/ambivalence (built from cmd/ambivalence); skipped otherwise.
 */
class HubClientInteropTest {
    private val bin = System.getenv("AMBIVALENCE_BIN")
    private val jpeg = byteArrayOf(0xFF.toByte(), 0xD8.toByte(), 1, 2, 3, 4, 0xFF.toByte(), 0xD9.toByte())

    @Test fun pairAuthCapture() {
        assumeTrue("set AMBIVALENCE_BIN to run", bin != null)
        val port = ServerSocket(0).use { it.localPort }
        val url = "ws://127.0.0.1:$port/v1"
        val config = Files.createTempDirectory("ambivalence-agent").toFile()
        val hub = ProcessBuilder(bin, "hub", "--listen", "127.0.0.1:$port").redirectErrorStream(true)
            .redirectOutput(File(config, "hub.log")).start()
        try {
            for (i in 0 until 100) {
                if (runCatching { Socket("127.0.0.1", port).close() }.isSuccess) break
                Thread.sleep(100)
            }
            val identity = Identity(MemoryPrefs())
            var shownSas: String? = null
            val events = object : HubClient.Events {
                override fun changed() {}
                override fun welcomed(client: HubClient) {}
                override fun dropped(client: HubClient) {}
                override fun note(text: String) { println("note: $text") }
                override fun confirm(sas: String, answer: CompletableFuture<Boolean>) { shownSas = sas; answer.complete(true) }
            }
            // The first candidate refuses connections, like a hub address on another LAN; the client must move on.
            val dead = ServerSocket(0).use { it.localPort }
            val client = HubClient(listOf("ws://127.0.0.1:$dead/v1", url), identity, FakeCamera(), events)
            client.start()
            val deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(10)
            while (client.code == null && System.nanoTime() < deadline) Thread.sleep(50)
            val code = client.code
            assertNotNull("no welcome from the hub", code)
            assertNotNull("welcome carries hubId", client.hubId)

            val pair = run(config, "pair", "--hub", url, code!!)
            assertEquals(pair.first, 0, pair.second)
            assertNotNull(identity.key)
            assertTrue(pair.first, pair.first.contains("phone shows ${shownSas!!}"))

            // A new agent connection: the phone challenges, the agent proves the key, capture is sealed.
            val out = File(config, "shot.jpg")
            val cap = run(config, "capture", "--hub", url, "-o", out.path)
            assertEquals(cap.first, 0, cap.second)
            assertArrayEquals(jpeg, out.readBytes())
            val cams = run(config, "cameras", "--hub", url)
            assertEquals(cams.first, 0, cams.second)
            assertTrue(cams.first, cams.first.contains("fake-back"))
            client.disconnect()
        } finally {
            hub.destroy()
        }
    }

    private fun run(config: File, vararg args: String): Pair<String, Int> {
        val p = ProcessBuilder(bin, *args).redirectErrorStream(true)
        p.environment()["XDG_CONFIG_HOME"] = config.path
        p.environment()["HOME"] = config.path
        val proc = p.start()
        val text = proc.inputStream.bufferedReader().readText()
        proc.waitFor(60, TimeUnit.SECONDS)
        println("$ ambivalence ${args.joinToString(" ")}\n$text")
        return text to proc.exitValue()
    }

    private inner class FakeCamera : CameraHost {
        override fun cameras(): JSONObject = JSONObject().put("facing", "back").put("torch", false).put("zoom", 1.0)
            .put("cameras", JSONArray().put(JSONObject().put("id", "fake-back").put("facing", "back").put("torch", false).put("minZoom", 1).put("maxZoom", 1)))
            .put("host", JSONObject().put("name", "android").put("platform", "android"))
        override fun command(camera: String?, torchOn: Boolean?, zoom: Double?, controls: JSONObject?): JSONObject =
            JSONObject().put("facing", "back").put("cameraId", "back").put("torch", false).put("zoom", 1.0).put("connected", true)
        override fun capture(quality: Int, maxWidth: Int): Pair<JSONObject, ByteArray> =
            JSONObject().put("facing", "back").put("cameraId", "back").put("torch", false).put("zoom", 1.0).put("width", 4).put("height", 2) to jpeg
    }

    private class MemoryPrefs : SharedPreferences {
        private val map = HashMap<String, Any?>()
        override fun getAll(): MutableMap<String, *> = map
        override fun getString(key: String, defValue: String?) = map[key] as String? ?: defValue
        override fun getStringSet(key: String, defValues: MutableSet<String>?) = defValues
        override fun getInt(key: String, defValue: Int) = defValue
        override fun getLong(key: String, defValue: Long) = defValue
        override fun getFloat(key: String, defValue: Float) = defValue
        override fun getBoolean(key: String, defValue: Boolean) = defValue
        override fun contains(key: String) = key in map
        override fun registerOnSharedPreferenceChangeListener(l: SharedPreferences.OnSharedPreferenceChangeListener?) {}
        override fun unregisterOnSharedPreferenceChangeListener(l: SharedPreferences.OnSharedPreferenceChangeListener?) {}
        override fun edit(): SharedPreferences.Editor = object : SharedPreferences.Editor {
            override fun putString(key: String, value: String?) = apply { map[key] = value }
            override fun putStringSet(key: String, values: MutableSet<String>?) = this
            override fun putInt(key: String, value: Int) = this
            override fun putLong(key: String, value: Long) = this
            override fun putFloat(key: String, value: Float) = this
            override fun putBoolean(key: String, value: Boolean) = this
            override fun remove(key: String) = apply { map.remove(key) }
            override fun clear() = apply { map.clear() }
            override fun commit() = true
            override fun apply() {}
        }
    }
}
