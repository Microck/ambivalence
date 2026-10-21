package dev.micr.ambivalence

import android.os.Handler
import android.os.Looper
import java.util.concurrent.CompletableFuture

/**
 * Every hub this phone is reachable on: the saved URL plus hubs discovered on the Wi-Fi, at most
 * [MAX] live connections. All share one [Identity] and one camera; each [HubClient] has its own
 * agent state. Discovery only adds connections and never replaces the saved URL.
 *
 * Same-hub dedupe: hubs send a per-process `hubId` in welcome. When two live connections carry
 * the same hubId, the saved URL wins, otherwise the older connection; the loser is closed and
 * parked (not redialed) until the winner drops. hubId is not a trust signal, so the saved URL is
 * never parked. All methods run on the main thread.
 */
class Hubs(
    private val identity: Identity,
    private val camera: CameraHost,
    private val ui: Ui,
) {
    interface Ui {
        fun onChanged()
        fun onNote(text: String)
        fun confirm(sas: String, answer: CompletableFuture<Boolean>)
    }

    private val main = Handler(Looper.getMainLooper())
    private val clients = ArrayList<HubClient>() // oldest first
    private val discovered = LinkedHashMap<String, List<String>>() // entry (first candidate) -> candidates
    private val parked = HashMap<String, HubClient>() // parked entry -> live connection to the same hub
    var saved: String? = null
        private set

    /** Makes [url] the saved hub. The previous saved hub is dropped unless it was also discovered. */
    fun setSaved(url: String) {
        val old = saved
        saved = url
        parked.remove(url)
        if (old != null && old != url && old !in discovered.keys) find(old)?.let { remove(it) }
        if (find(url) == null && clients.size >= MAX) clients.last().let { remove(it) }
        fill()
        ui.onChanged()
    }

    /** One discovered hub, as its ordered candidate URLs (one per advertised address): one entry, one connection. */
    fun discovered(urls: List<String>) {
        if (urls.isEmpty()) return
        // Identical URLs share one connection: skip a hub already known by any of its URLs.
        if (urls.any { it == saved || discovered.values.any { known -> it in known } }) return
        discovered[urls.first()] = urls
        fill()
        ui.onChanged()
    }

    fun stop() {
        clients.forEach { it.disconnect() }
        clients.clear()
        parked.clear()
    }

    /** The pair code from the saved hub, else from the first connected one. */
    fun code(): String? {
        val live = clients.filter { it.live && it.code != null }
        return (live.firstOrNull { it.url == saved } ?: live.firstOrNull())?.code
    }

    fun summary(): String {
        val live = clients.filter { it.live }
        return when {
            clients.isEmpty() -> "looking for your agent on this Wi-Fi"
            clients.any { it.agent == HubClient.Agent.AUTHED } -> "paired, agent can see this camera and hear this microphone"
            clients.any { it.agent == HubClient.Agent.CONNECTING } -> "agent connecting"
            live.size > 1 -> "connected to ${live.size} hubs, waiting for the agent"
            live.size == 1 -> "connected, waiting for the agent"
            else -> "connecting to " + clients.joinToString(", ") { android.net.Uri.parse(it.url).host ?: it.url }
        }
    }

    private fun find(url: String) = clients.firstOrNull { it.url == url }

    // Dials every wanted entry (saved first, then discovered) that is not connected or parked, up to MAX.
    private fun fill() {
        val wanted = listOfNotNull(saved?.let { listOf(it) }) + discovered.values
        for (urls in wanted) {
            if (clients.size >= MAX) return
            if (urls.first() in parked || find(urls.first()) != null) continue
            val c = HubClient(urls, identity, camera, events)
            clients += c
            c.start()
        }
    }

    private fun remove(c: HubClient) {
        clients.remove(c)
        c.disconnect()
        unpark(c)
    }

    // Connections parked behind c may dial again now that c is gone or down.
    private fun unpark(c: HubClient) {
        parked.values.removeAll { it === c }
        fill()
    }

    private fun onWelcomed(c: HubClient) {
        val id = c.hubId ?: return
        if (c !in clients || !c.live) return
        val other = clients.firstOrNull { it !== c && it.live && it.hubId == id } ?: return
        val loser = when {
            c.url == saved -> other
            other.url == saved -> c
            clients.indexOf(other) < clients.indexOf(c) -> c
            else -> other
        }
        val winner = if (loser === c) other else c
        parked[loser.url] = winner
        remove(loser)
        ui.onChanged()
    }

    private val events = object : HubClient.Events {
        override fun changed() { main.post { ui.onChanged() } }
        override fun welcomed(client: HubClient) { main.post { onWelcomed(client) } }
        override fun dropped(client: HubClient) { main.post { if (client in clients) unpark(client) } }
        override fun note(text: String) { main.post { ui.onNote(text) } }
        override fun confirm(sas: String, answer: CompletableFuture<Boolean>) { main.post { ui.confirm(sas, answer) } }
    }

    companion object {
        const val MAX = 4
    }
}
