package dev.micr.ambivalence

import android.content.SharedPreferences
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okio.ByteString.Companion.toByteString
import org.json.JSONObject
import java.util.Base64
import java.util.concurrent.CompletableFuture
import java.util.concurrent.Executors
import java.util.concurrent.ScheduledFuture
import java.util.concurrent.TimeUnit

/**
 * The phone's one identity, shared by every hub connection. `device` is a public id; `key` is
 * the link key from the one-time pairing. Both live in the app's private SharedPreferences
 * ("ambivalence": "device", "key"); the key is never sent to a hub.
 */
class Identity(private val prefs: SharedPreferences) {
    val device: String = prefs.getString("device", null)
        ?: Link.hex(Link.nonce()).also { prefs.edit().putString("device", it).apply() }

    @Volatile var key: ByteArray? = prefs.getString("key", null)?.let { Link.unhex(it) }?.takeIf { it.size == 32 }
        private set

    fun setKey(k: ByteArray) {
        prefs.edit().putString("key", Link.hex(k)).commit()
        key = k
    }
}

/**
 * The camera, as served to agents. Calls block the calling connection's inbox thread until the
 * hardware is done and throw with a user-facing message on failure. Returned objects hold the
 * reply fields; the caller adds type and id.
 */
interface CameraHost {
    fun cameras(): JSONObject
    fun command(camera: String?, torchOn: Boolean?, zoom: Double?, controls: JSONObject? = null): JSONObject
    fun capture(quality: Int, maxWidth: Int): Pair<JSONObject, ByteArray>

    /** One clip, capped by the agent at 60 s. Returns metadata and a WAV file. */
    fun listen(durationMs: Int, sampleRate: Int): Pair<JSONObject, ByteArray>

    /** Starts continuous recording; a zero max means until stop. */
    fun audioStart(maxDurationMs: Int, sampleRate: Int): JSONObject

    /** Drains audio recorded since the previous call, waiting up to maxMs for it. */
    fun audioRead(listenId: String, maxMs: Int): Pair<JSONObject, ByteArray>

    /** Stops the session with this id, or any session when null (agent left). */
    fun audioStop(listenId: String?): JSONObject

    fun level(durationMs: Int, sampleRate: Int): JSONObject
}

/**
 * One hub connection, the phone side of docs/protocol.md (mirrors internal/host/fake.go). It
 * keeps reconnecting until [disconnect]. Each connection has its own agent state (challenge,
 * pairing, seal); the identity and camera are shared. Incoming messages run one at a time on
 * this connection's inbox thread, so sealed sequence checks follow arrival order.
 */
class HubClient(
    /** Ordered URLs reaching one hub (a discovered hub advertises one per address). */
    private val candidates: List<String>,
    private val identity: Identity,
    private val camera: CameraHost,
    private val events: Events,
) {
    /** Stable name of this hub entry: its first candidate. */
    val url = candidates.first()

    interface Events {
        fun changed()
        fun welcomed(client: HubClient)
        fun dropped(client: HubClient)
        fun note(text: String)
        /** Shows the pairing code; [answer] completes true on Confirm, false on Cancel. */
        fun confirm(sas: String, answer: CompletableFuture<Boolean>)
    }

    enum class Agent { NONE, CONNECTING, AUTHED }

    // Read by the UI thread.
    @Volatile var code: String? = null
        private set
    @Volatile var hubId: String? = null
        private set
    @Volatile var live = false // welcomed on the current socket
        private set
    @Volatile var agent = Agent.NONE
        private set

    private val inbox = Executors.newSingleThreadScheduledExecutor()
    @Volatile private var socket: WebSocket? = null
    @Volatile private var stopped = false
    @Volatile private var pendingConfirm: CompletableFuture<Boolean>? = null
    @Volatile private var confirmGone = false // the prompt closed because the agent or socket went away
    private val writeLock = Any()

    // Per-agent state, inbox thread only: reset whenever an agent arrives or leaves. seal is set
    // once the agent has proven the link; only messages sealed under it may touch the camera.
    private var agentHere = false
    private var seal: Seal? = null
    private var challenge: ByteArray? = null
    private var pairing: Party? = null
    private var pairNB = ByteArray(0)
    private var pairCommit = ByteArray(0)
    private var squat: ScheduledFuture<*>? = null
    private var generation = 0

    // Candidate dialing, inbox thread only: try each in turn, then wait 3 s and start over from
    // the one that last worked. No racing.
    private var tryIdx = 0
    private var good = 0
    private var reached = false
    private val http = if (candidates.size > 1) baseHttp.newBuilder().connectTimeout(3, TimeUnit.SECONDS).build() else baseHttp

    fun start() = inbox.execute { open() }

    fun disconnect() {
        stopped = true
        pendingConfirm?.complete(false)
        socket?.close(1000, "bye")
        socket = null
        live = false
        inbox.shutdownNow()
    }

    private fun open() {
        if (stopped) return
        reached = false
        val ws = http.newWebSocket(Request.Builder().url(candidates[tryIdx]).build(), object : WebSocketListener() {
            override fun onOpen(webSocket: WebSocket, response: okhttp3.Response) {
                post { if (webSocket === socket) { reached = true; good = tryIdx } }
                webSocket.send(
                    JSONObject().put("v", 1).put("type", "hello").put("role", "host").put("device", identity.device)
                        .put("host", JSONObject().put("name", "android").put("platform", "android")).toString(),
                )
            }

            override fun onMessage(webSocket: WebSocket, text: String) {
                // The inbox may be blocked on the pairing screen; an agent leaving cancels it.
                pendingConfirm?.let { p ->
                    val m = runCatching { JSONObject(text) }.getOrNull()
                    if (m?.optString("type") == "status" && !m.optBoolean("connected")) dismissConfirm()
                }
                post { if (webSocket === socket) handle(text) }
            }

            override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
                webSocket.close(1000, null)
            }

            override fun onClosed(webSocket: WebSocket, code: Int, reason: String) = lost(webSocket)

            override fun onFailure(webSocket: WebSocket, t: Throwable, response: okhttp3.Response?) = lost(webSocket)
        })
        socket = ws
        if (stopped) { // disconnect raced this dial
            ws.cancel()
            socket = null
            return
        }
        events.changed()
    }

    // Callbacks can fire before open() has stored the socket; the inbox runs after it, in order.
    private fun lost(ws: WebSocket) {
        if (ws === socket) dismissConfirm() // the inbox may be blocked on it
        post {
            if (ws !== socket || stopped) return@post
            socket = null
            live = false
            code = null
            resetAgent(false)
            events.dropped(this)
            events.changed()
            if (!reached && (tryIdx + 1) % candidates.size != good) {
                tryIdx = (tryIdx + 1) % candidates.size // unreachable address: try the next one now
                open()
            } else {
                tryIdx = good
                inbox.schedule({ open() }, 3, TimeUnit.SECONDS)
            }
        }
    }

    private fun post(task: () -> Unit) {
        runCatching { inbox.execute(task) } // rejected after disconnect
    }

    private fun handle(text: String) {
        val m = runCatching { JSONObject(text) }.getOrNull() ?: return
        val id = m.optString("id")
        when (m.optString("type")) {
            "welcome" -> {
                code = m.optString("code").ifEmpty { null }
                hubId = m.optString("hubId").ifEmpty { null }
                live = true
                events.welcomed(this)
                presence(m.optBoolean("connected"))
                events.changed()
            }
            "status" -> presence(m.optBoolean("connected"))
            "ping" -> send(JSONObject().put("type", "pong").put("id", id))
            "auth" -> auth(m)
            "pair1" -> pair1(m)
            "pair3" -> pair3(m)
            "sealed" -> sealed(m)
            // Unsealed camera requests never count, even after auth: a hub could inject them.
            "cameras", "command", "capture", "listen", "audio_start", "audio_read", "audio_stop", "level" -> sendError(id, UNAUTHENTICATED)
            "error" -> events.note(m.optString("error"))
        }
    }

    // An agent arrived or left. The hub repeats status, so act only on a change; a second
    // challenge would void the first. A linked phone challenges every arriving agent; one that
    // neither proves the key nor starts pairing within 15 s is dropped so it cannot squat.
    private fun presence(present: Boolean) {
        if (present && agentHere) return
        resetAgent(present)
        if (!present) return
        val gen = generation
        squat = inbox.schedule({ if (gen == generation && seal == null && pairing == null) reject() }, 15, TimeUnit.SECONDS)
        if (identity.key != null) {
            val c = Link.nonce()
            challenge = c
            send(JSONObject().put("type", "auth").put("nonce", Link.hex(c)))
        }
    }

    private fun resetAgent(present: Boolean) {
        // Privacy: no microphone survives an agent leaving, reconnecting or being replaced.
        runCatching { camera.audioStop(null) }
        agentHere = present
        seal = null
        challenge = null
        pairing = null
        squat?.cancel(false)
        squat = null
        generation++
        setAgent(if (present) Agent.CONNECTING else Agent.NONE)
    }

    private fun setAgent(a: Agent) {
        if (agent == a) return
        agent = a
        events.changed()
    }

    private fun reject() {
        send(JSONObject().put("type", "reject"))
        // The hub frees the slot at once, so the next agent's status is a real arrival.
        resetAgent(false)
        events.note("refused an agent that could not prove the link")
    }

    private fun authed(s: Seal) {
        seal = s
        challenge = null
        pairing = null
        squat?.cancel(false)
        squat = null
        setAgent(Agent.AUTHED)
    }

    private fun auth(m: JSONObject) {
        val nP = Link.unhex(m.optString("nonce"))
        val nA = Link.unhex(m.optString("agentNonce"))
        val mac = Link.unhex(m.optString("mac"))
        val key = identity.key
        val c = challenge
        val dev = identity.device.toByteArray(Charsets.UTF_8)
        if (key == null || c == null || nA.size != 16 || !Link.equal(nP, c) ||
            !Link.equal(mac, Link.mac(key, "agent", dev, nP, nA))
        ) return reject()
        authed(Seal(key, identity.device, nP, nA))
        send(JSONObject().put("type", "authok").put("nonce", m.optString("nonce")).put("mac", Link.hex(Link.mac(key, "phone", dev, nP, nA))))
    }

    private fun pair1(m: JSONObject) {
        val id = m.optString("id")
        pairCommit = Link.unhex(m.optString("commit"))
        val p = try { Party() } catch (e: Exception) { return sendError(id, e.message ?: "pairing failed") }
        pairing = p
        pairNB = Link.nonce()
        send(
            JSONObject().put("type", "pair2").put("id", id).put("pub", Link.hex(p.pub))
                .put("nonce", Link.hex(pairNB)).put("device", identity.device),
        )
    }

    private fun pair3(m: JSONObject) {
        val id = m.optString("id")
        val agentPub = Link.unhex(m.optString("pub"))
        val nA = Link.unhex(m.optString("nonce"))
        val p = pairing
        if (p == null || nA.size != 16 || !Link.equal(pairCommit, Link.h("commit", agentPub, nA))) {
            sendError(id, "pairing commitment mismatch")
            return reject()
        }
        val shared = runCatching { p.shared(agentPub) }.getOrNull()
        if (shared == null || !confirm(Link.sas(agentPub, p.pub, nA, pairNB))) {
            // Agent gone: no reject, which could drop a newer agent whose arrival is queued behind us.
            if (confirmGone) return
            sendError(id, "pairing cancelled on the phone")
            return reject()
        }
        val key = Link.key(shared, agentPub, p.pub)
        identity.setKey(key)
        authed(Seal(key, identity.device, pairNB, nA))
        send(JSONObject().put("type", "pair4").put("id", id).put("mac", Link.hex(Link.mac(key, "confirm", identity.device.toByteArray(Charsets.UTF_8)))))
    }

    // Blocks the inbox until the user answers, the agent leaves, the socket drops or 2 minutes pass.
    private fun confirm(sas: String): Boolean {
        val answer = CompletableFuture<Boolean>()
        confirmGone = false
        pendingConfirm = answer
        try {
            if (socket == null || stopped) return false
            events.confirm(sas, answer)
            return answer.get(120, TimeUnit.SECONDS)
        } catch (e: Exception) {
            return false
        } finally {
            answer.complete(false) // dismisses the screen if still showing
            pendingConfirm = null
        }
    }

    // Called off the inbox thread: closes an open pairing prompt as Cancel; no key is stored.
    private fun dismissConfirm() {
        val p = pendingConfirm ?: return
        confirmGone = true
        p.complete(false)
    }

    private fun sealed(m: JSONObject) {
        val id = m.optString("id")
        // No seal yet (e.g. the phone just reconnected): make the agent re-authenticate.
        val s = seal ?: return sendError(id, UNAUTHENTICATED)
        val inner = try {
            JSONObject(String(s.rx.open(Base64.getDecoder().decode(m.optString("box"))), Charsets.UTF_8))
        } catch (e: Exception) {
            return // forged or replayed by the hub: ignore it, keep serving the real agent
        }
        if (inner.optString("id") != id) return
        serve(s, inner)
    }

    // Serves one opened request; every reply goes back sealed under the session it came in on.
    private fun serve(s: Seal, m: JSONObject) {
        val id = m.optString("id")
        val type = m.optString("type")
        try {
            when (type) {
                "cameras" -> sendSealed(s, camera.cameras().put("type", "cameras").put("id", id))
                "command" -> {
                    val torch = if (m.has("torchOn") && !m.isNull("torchOn")) m.optBoolean("torchOn") else null
                    val zoom = if (m.has("zoomLevel") && !m.isNull("zoomLevel")) m.optDouble("zoomLevel") else null
                    val cam = m.optString("camera").ifEmpty { null }
                    sendSealed(s, camera.command(cam, torch, zoom, m.optJSONObject("controls")).put("type", "status").put("id", id))
                }
                "capture" -> {
                    val q = m.optInt("quality").takeIf { it > 0 } ?: DEFAULT_QUALITY
                    val w = m.optInt("maxWidth").takeIf { it > 0 } ?: DEFAULT_MAX_WIDTH
                    val (meta, jpeg) = camera.capture(q, w)
                    meta.put("type", "frame").put("id", id).put("mime", "image/jpeg").put("bytes", jpeg.size)
                    sendSealed(s, meta, jpeg)
                }
                "listen" -> {
                    val ms = m.optInt("durationMs").takeIf { it > 0 } ?: DEFAULT_AUDIO_MS
                    val rate = m.optInt("sampleRate").takeIf { it > 0 } ?: DEFAULT_SAMPLE_RATE
                    val (meta, wav) = camera.listen(ms, rate)
                    meta.put("type", "frame").put("id", id).put("mime", "audio/wav").put("bytes", wav.size)
                    sendSealed(s, meta, wav)
                }
                "audio_start" -> {
                    val max = m.optInt("maxDurationMs")
                    val rate = m.optInt("sampleRate").takeIf { it > 0 } ?: DEFAULT_SAMPLE_RATE
                    sendSealed(s, camera.audioStart(max, rate).put("type", "audio_started").put("id", id))
                }
                "audio_read" -> {
                    val ms = (m.optInt("durationMs").takeIf { it > 0 } ?: DEFAULT_CHUNK_MS).coerceIn(100, MAX_CHUNK_MS)
                    val (meta, wav) = camera.audioRead(m.optString("listenId"), ms)
                    meta.put("type", "frame").put("id", id).put("mime", "audio/wav").put("bytes", wav.size)
                    sendSealed(s, meta, wav)
                }
                "audio_stop" -> {
                    val meta = camera.audioStop(m.optString("listenId").takeIf { it.isNotEmpty() })
                    sendSealed(s, meta.put("type", "status").put("id", id))
                }
                "level" -> {
                    val ms = (m.optInt("durationMs").takeIf { it > 0 } ?: 1000).coerceIn(100, MAX_CHUNK_MS)
                    val rate = m.optInt("sampleRate").takeIf { it > 0 } ?: DEFAULT_SAMPLE_RATE
                    sendSealed(s, camera.level(ms, rate).put("type", "levels").put("id", id))
                }
                else -> sendSealed(s, JSONObject().put("type", "error").put("id", id).put("error", "unknown request $type"))
            }
        } catch (e: Exception) {
            sendSealed(s, JSONObject().put("type", "error").put("id", id).put("error", e.message ?: e.toString()))
        }
    }

    private fun send(obj: JSONObject) {
        obj.put("v", 1)
        socket?.send(obj.toString())
    }

    private fun sendError(id: String, err: String) = send(JSONObject().put("type", "error").put("id", id).put("error", err))

    // Seals under the write lock so wire order equals sequence order; a frame's payload follows
    // as its own box. The outer id only routes; the agent checks the inner one.
    private fun sendSealed(s: Seal, inner: JSONObject, payload: ByteArray? = null) {
        inner.put("v", 1)
        synchronized(writeLock) {
            val ws = socket ?: return
            val box = s.tx.seal(inner.toString().toByteArray(Charsets.UTF_8))
            ws.send(
                JSONObject().put("v", 1).put("type", "sealed").put("id", inner.optString("id"))
                    .put("box", Base64.getEncoder().encodeToString(box)).toString(),
            )
            if (payload != null) ws.send(s.tx.seal(payload).toByteString())
        }
    }

    companion object {
        const val UNAUTHENTICATED = "agent not authenticated"
        private const val DEFAULT_QUALITY = 80
        private const val DEFAULT_MAX_WIDTH = 1280
        private const val DEFAULT_SAMPLE_RATE = 16000
        private const val DEFAULT_AUDIO_MS = 5000
        private const val DEFAULT_CHUNK_MS = 10000
        private const val MAX_CHUNK_MS = 30000
        private val baseHttp: OkHttpClient by lazy {
            OkHttpClient.Builder()
                .readTimeout(0, TimeUnit.MILLISECONDS)
                .pingInterval(20, TimeUnit.SECONDS)
                .build()
        }
    }
}

/**
 * Finds ambivalence hubs on the local Wi-Fi (mDNS service `_ambivalence._tcp`) and reports each
 * hub's candidate ws:// URLs on the main thread. Discovery confers no trust.
 */
class HubDiscovery(context: android.content.Context, private val found: (List<String>) -> Unit) {
    private val nsd = context.getSystemService(android.net.nsd.NsdManager::class.java)
    private val main = android.os.Handler(android.os.Looper.getMainLooper())
    private var listener: android.net.nsd.NsdManager.DiscoveryListener? = null

    fun start() {
        if (listener != null) return
        val l = object : android.net.nsd.NsdManager.DiscoveryListener {
            override fun onServiceFound(info: android.net.nsd.NsdServiceInfo) = resolve(info)
            override fun onServiceLost(info: android.net.nsd.NsdServiceInfo) {}
            override fun onDiscoveryStarted(type: String) {}
            override fun onDiscoveryStopped(type: String) {}
            override fun onStartDiscoveryFailed(type: String, code: Int) { listener = null }
            override fun onStopDiscoveryFailed(type: String, code: Int) {}
        }
        listener = l
        nsd.discoverServices("_ambivalence._tcp", android.net.nsd.NsdManager.PROTOCOL_DNS_SD, l)
    }

    fun stop() {
        listener?.let { runCatching { nsd.stopServiceDiscovery(it) } }
        listener = null
    }

    @Suppress("DEPRECATION") // resolveService is the only resolver below API 34
    private fun resolve(info: android.net.nsd.NsdServiceInfo) {
        nsd.resolveService(info, object : android.net.nsd.NsdManager.ResolveListener {
            override fun onResolveFailed(info: android.net.nsd.NsdServiceInfo, code: Int) {}
            override fun onServiceResolved(info: android.net.nsd.NsdServiceInfo) {
                // Prefer the hub's addrs= TXT list (see hub advertise); fall back to the resolved host.
                val listed = info.attributes["addrs"]?.toString(Charsets.UTF_8)?.split(',')?.map { it.trim() }?.filter { it.isNotEmpty() }.orEmpty()
                val hosts = listed.ifEmpty {
                    val addr = info.host ?: return
                    listOf(if (addr is java.net.Inet6Address) "[${addr.hostAddress?.substringBefore('%')}]" else addr.hostAddress ?: return)
                }
                val urls = hosts.map { "ws://$it:${info.port}/v1" }
                main.post { found(urls) }
            }
        })
    }
}
