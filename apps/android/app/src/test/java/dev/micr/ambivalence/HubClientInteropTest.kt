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
import java.util.Base64
import java.util.concurrent.CompletableFuture
import java.util.concurrent.CountDownLatch
import java.util.concurrent.Semaphore
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger

/**
 * Pairs, authenticates and captures through the real Go hub and agent CLI. Opt in with
 * AMBIVALENCE_BIN=/path/to/ambivalence (built from cmd/ambivalence); skipped otherwise.
 */
class HubClientInteropTest {
    private val bin = System.getenv("AMBIVALENCE_BIN")
    private val jpeg = byteArrayOf(0xFF.toByte(), 0xD8.toByte(), 1, 2, 3, 4, 0xFF.toByte(), 0xD9.toByte())

    @Test fun staleSessionCleanupDoesNotHideNewerRecordingIndicator() {
        val dir = Files.createTempDirectory("ambivalence-audio-test").toFile()
        try {
            val recorder = AudioRecorder(dir)
            val events = mutableListOf<Boolean>()
            recorder.onRecording = { events.add(it) }

            val oldGen = recorder.beginSessionGenerationForTest()
            val newGen = recorder.beginSessionGenerationForTest()
            assertEquals(listOf(true, true), events)

            // Old worker finishing after newGen started must not emit false.
            recorder.finishSessionGenerationForTest(oldGen)
            assertEquals(listOf(true, true), events)

            // Current worker finishing emits false.
            recorder.finishSessionGenerationForTest(newGen)
            assertEquals(listOf(true, true, false), events)
        } finally {
            dir.deleteRecursively()
        }
    }

    @Test fun maxChunkDurationDoesNotOverflowAtHighSampleRates() {
        val dir = Files.createTempDirectory("ambivalence-audio-duration-test").toFile()
        try {
            val recorder = AudioRecorder(dir)

            // 30 seconds of 16-bit mono PCM at 48 kHz = 2,880,000 bytes (2,880,000 * 1000 overflows Int).
            recorder.seedEndedSessionForTest("owner", "id-48k", 48_000, 2_880_000)
            val (meta48k, wav48k) = recorder.read("owner", "id-48k", 30_000)
            assertEquals(30_000, meta48k.getInt("durationMs"))
            assertEquals(30_000, meta48k.getInt("totalMs"))
            assertEquals(44 + 2_880_000, wav48k.size)

            // 30 seconds of 16-bit mono PCM at 44.1 kHz = 2,646,000 bytes (2,646,000 * 1000 overflows Int).
            recorder.seedEndedSessionForTest("owner", "id-44k1", 44_100, 2_646_000)
            val (meta44k1, wav44k1) = recorder.read("owner", "id-44k1", 30_000)
            assertEquals(30_000, meta44k1.getInt("durationMs"))
            assertEquals(30_000, meta44k1.getInt("totalMs"))
            assertEquals(44 + 2_646_000, wav44k1.size)

            // Stop ack preserves connected=true so agent status cache does not flip to disconnected.
            recorder.seedEndedSessionForTest("owner", "id-stop", 16_000, 32_000)
            val stopped = recorder.stop("owner", "id-stop")
            assertTrue(stopped.getBoolean("connected"))
        } finally {
            dir.deleteRecursively()
        }
    }

    @Test fun queuedListenAfterDepartureDoesNotOpenMicrophone() {
        val identity = Identity(MemoryPrefs())
        val key = ByteArray(32) { (it + 1).toByte() }
        identity.setKey(key)
        val events = object : HubClient.Events {
            override fun changed() {}
            override fun welcomed(client: HubClient) {}
            override fun dropped(client: HubClient) {}
            override fun note(text: String) {}
            override fun confirm(sas: String, answer: CompletableFuture<Boolean>) { answer.complete(true) }
        }
        val enterCapture = CountDownLatch(1)
        val releaseCapture = CountDownLatch(1)
        val camera = object : FakeCamera() {
            override fun capture(quality: Int, maxWidth: Int): Pair<JSONObject, ByteArray> {
                enterCapture.countDown()
                releaseCapture.await(5, TimeUnit.SECONDS)
                return super.capture(quality, maxWidth)
            }
        }
        val client = HubClient(listOf("ws://127.0.0.1:1/v1"), identity, camera, events)
        val nP = ByteArray(16) { 0x11 }
        val nA = ByteArray(16) { 0x22 }
        // Install an authenticated seal and a dummy active socket via reflection to exercise
        // WebSocketListener.onMessage queueing directly without needing a live network server.
        val agentSeal = Seal(key, identity.device, nP, nA)
        val phoneSeal = Seal(key, identity.device, nP, nA)
        val dummySocket = java.lang.reflect.Proxy.newProxyInstance(
            okhttp3.WebSocket::class.java.classLoader,
            arrayOf(okhttp3.WebSocket::class.java),
        ) { _, method, _ ->
            when (method.name) {
                "send", "close" -> true
                else -> null
            }
        } as okhttp3.WebSocket
        HubClient::class.java.getDeclaredField("socket").apply { isAccessible = true; set(client, dummySocket) }
        HubClient::class.java.getDeclaredField("seal").apply { isAccessible = true; set(client, phoneSeal) }
        HubClient::class.java.getDeclaredField("agentHere").apply { isAccessible = true; set(client, true) }

        fun sealedMsg(id: String, inner: JSONObject): String {
            val box = agentSeal.rx.seal(inner.put("id", id).toString().toByteArray(Charsets.UTF_8))
            return JSONObject().put("type", "sealed").put("id", id)
                .put("box", Base64.getEncoder().encodeToString(box)).toString()
        }

        val handleMethod = HubClient::class.java.getDeclaredMethod("handle", String::class.java).apply { isAccessible = true }
        val postMethod = HubClient::class.java.getDeclaredMethod("post", Function0::class.java).apply { isAccessible = true }
        val departuresSeenField = HubClient::class.java.getDeclaredField("departuresSeen").apply { isAccessible = true }
        val departuresSeen = departuresSeenField.get(client) as AtomicInteger

        // 1. Enqueue a capture that blocks the inbox thread.
        val capMsg = sealedMsg("cap-1", JSONObject().put("type", "capture"))
        postMethod.invoke(client, { handleMethod.invoke(client, capMsg) })
        assertTrue(enterCapture.await(5, TimeUnit.SECONDS))

        // 2. Enqueue a sealed listen request behind the blocked capture.
        val listenMsg = sealedMsg("listen-1", JSONObject().put("type", "listen").put("durationMs", 60000))
        postMethod.invoke(client, { handleMethod.invoke(client, listenMsg) })

        // 3. Agent departs while capture is still running on the inbox thread.
        departuresSeen.incrementAndGet()
        val depMsg = JSONObject().put("type", "status").put("connected", false).toString()
        postMethod.invoke(client, { handleMethod.invoke(client, depMsg) })

        // 4. Release the capture and wait for the inbox queue to drain.
        releaseCapture.countDown()
        val done = CountDownLatch(1)
        postMethod.invoke(client, { done.countDown() })
        assertTrue(done.await(5, TimeUnit.SECONDS))

        assertEquals("queued listen must not run after departure", 0, camera.listenCalls)
        client.disconnect()
    }

    @Test fun listenWaitingOnAudioSemaphoreDoesNotStartAfterDeparture() {
        val identity = Identity(MemoryPrefs())
        val key = ByteArray(32) { (it + 1).toByte() }
        identity.setKey(key)
        val events = object : HubClient.Events {
            override fun changed() {}
            override fun welcomed(client: HubClient) {}
            override fun dropped(client: HubClient) {}
            override fun note(text: String) {}
            override fun confirm(sas: String, answer: CompletableFuture<Boolean>) { answer.complete(true) }
        }
        val audioLock = Semaphore(1, true)
        assertTrue(audioLock.tryAcquire()) // another hub connection currently holds the microphone
        val enteredListen = CountDownLatch(1)
        val openedAfterSemaphore = AtomicInteger(0)
        val camera = object : FakeCamera() {
            override fun listen(owner: String, durationMs: Int, sampleRate: Int, cancelled: () -> Boolean): Pair<JSONObject, ByteArray> {
                enteredListen.countDown()
                return AudioRecorder.withPermit(audioLock, cancelled) {
                    openedAfterSemaphore.incrementAndGet()
                    super.listen(owner, durationMs, sampleRate, cancelled)
                }
            }
        }
        val client = HubClient(listOf("ws://127.0.0.1:1/v1"), identity, camera, events)
        val nP = ByteArray(16) { 0x11 }
        val nA = ByteArray(16) { 0x22 }
        val agentSeal = Seal(key, identity.device, nP, nA)
        val phoneSeal = Seal(key, identity.device, nP, nA)
        val dummySocket = java.lang.reflect.Proxy.newProxyInstance(
            okhttp3.WebSocket::class.java.classLoader,
            arrayOf(okhttp3.WebSocket::class.java),
        ) { _, method, _ ->
            when (method.name) {
                "send", "close" -> true
                else -> null
            }
        } as okhttp3.WebSocket
        HubClient::class.java.getDeclaredField("socket").apply { isAccessible = true; set(client, dummySocket) }
        HubClient::class.java.getDeclaredField("seal").apply { isAccessible = true; set(client, phoneSeal) }
        HubClient::class.java.getDeclaredField("agentHere").apply { isAccessible = true; set(client, true) }

        fun sealedMsg(id: String, inner: JSONObject): String {
            val box = agentSeal.rx.seal(inner.put("id", id).toString().toByteArray(Charsets.UTF_8))
            return JSONObject().put("type", "sealed").put("id", id)
                .put("box", Base64.getEncoder().encodeToString(box)).toString()
        }

        val handleMethod = HubClient::class.java.getDeclaredMethod("handle", String::class.java).apply { isAccessible = true }
        val postMethod = HubClient::class.java.getDeclaredMethod("post", Function0::class.java).apply { isAccessible = true }
        val departuresSeenField = HubClient::class.java.getDeclaredField("departuresSeen").apply { isAccessible = true }
        val departuresSeen = departuresSeenField.get(client) as AtomicInteger

        // 1. Start a listen request that passes HubClient's initial check and blocks on audioLock.
        val listenMsg = sealedMsg("listen-sem-1", JSONObject().put("type", "listen").put("durationMs", 60000))
        postMethod.invoke(client, { handleMethod.invoke(client, listenMsg) })
        assertTrue(enteredListen.await(5, TimeUnit.SECONDS))

        // 2. While waiting on the semaphore, the agent departs.
        departuresSeen.incrementAndGet()
        val depMsg = JSONObject().put("type", "status").put("connected", false).toString()
        postMethod.invoke(client, { handleMethod.invoke(client, depMsg) })

        // 3. Release the semaphore; the departed request must abort without opening the microphone.
        audioLock.release()
        val done = CountDownLatch(1)
        postMethod.invoke(client, { done.countDown() })
        assertTrue(done.await(5, TimeUnit.SECONDS))

        assertEquals("listen waiting on semaphore must not open microphone after departure", 0, openedAfterSemaphore.get())
        client.disconnect()
    }

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
            val camera = FakeCamera()
            val client = HubClient(listOf("ws://127.0.0.1:$dead/v1", url), identity, camera, events)
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
            val configured = run(config, "configure", """{"focusMode":"manual","focusPosition":0,"exposureMode":"manual","exposureTime":0.008,"iso":100}""", "--hub", url)
            assertEquals(configured.first, 0, configured.second)
            assertTrue(configured.first, configured.first.contains("focusPosition"))
            assertTrue(configured.first, configured.first.contains("0.008"))
            val stopsBefore = camera.audioStopCalls
            client.disconnect()
            assertTrue("disconnect stops this connection's microphone", camera.audioStopCalls > stopsBefore)
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

    private open inner class FakeCamera : CameraHost {
        @Volatile var listenCalls = 0
        @Volatile var audioStopCalls = 0
        @Volatile var lastStopOwner: String? = null
        override fun cameras(): JSONObject = JSONObject().put("capabilities", JSONObject().put("controlsVersion", 1)).put("facing", "back").put("torch", false).put("zoom", 1.0)
            .put("cameras", JSONArray().put(JSONObject().put("id", "fake-back").put("facing", "back").put("torch", false).put("minZoom", 1).put("maxZoom", 1)))
            .put("host", JSONObject().put("name", "android").put("platform", "android"))
        override fun command(camera: String?, torchOn: Boolean?, zoom: Double?, controls: JSONObject?): JSONObject =
            JSONObject().put("settings", controls ?: JSONObject()).put("facing", "back").put("cameraId", "back").put("torch", false).put("zoom", 1.0).put("connected", true)
        override fun capture(quality: Int, maxWidth: Int): Pair<JSONObject, ByteArray> =
            JSONObject().put("facing", "back").put("cameraId", "back").put("torch", false).put("zoom", 1.0).put("width", 4).put("height", 2) to jpeg

        override fun listen(owner: String, durationMs: Int, sampleRate: Int, cancelled: () -> Boolean): Pair<JSONObject, ByteArray> {
            listenCalls++
            return JSONObject().put("mime", "audio/wav").put("durationMs", durationMs).put("sampleRate", sampleRate).put("levels", JSONObject().put("rmsDb", -100).put("peakDb", -100)) to ByteArray(44)
        }

        override fun audioStart(owner: String, maxDurationMs: Int, sampleRate: Int, cancelled: () -> Boolean): JSONObject =
            JSONObject().put("listenId", "fake").put("sampleRate", sampleRate).put("maxDurationMs", maxDurationMs)

        override fun audioRead(owner: String, listenId: String, maxMs: Int, cancelled: () -> Boolean): Pair<JSONObject, ByteArray> =
            JSONObject().put("mime", "audio/wav").put("durationMs", 0).put("sampleRate", 16000).put("totalMs", 0).put("ended", true) to ByteArray(44)

        override fun audioStop(owner: String, listenId: String?): JSONObject {
            audioStopCalls++
            lastStopOwner = owner
            return JSONObject().put("durationMs", 0).put("totalMs", 0).put("ended", true).put("connected", true)
        }

        override fun level(owner: String, durationMs: Int, sampleRate: Int, cancelled: () -> Boolean): JSONObject =
            JSONObject().put("durationMs", durationMs).put("sampleRate", sampleRate).put("levels", JSONObject().put("rmsDb", -100).put("peakDb", -100))
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
