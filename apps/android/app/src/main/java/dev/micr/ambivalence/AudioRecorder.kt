package dev.micr.ambivalence

import android.media.AudioFormat
import android.media.AudioRecord
import android.media.MediaRecorder
import org.json.JSONObject
import java.io.File
import java.io.FileOutputStream
import java.io.RandomAccessFile
import java.nio.ByteBuffer
import java.nio.ByteOrder
import kotlin.math.abs
import kotlin.math.log10
import kotlin.math.max
import kotlin.math.min
import kotlin.math.sqrt

/**
 * One microphone at a time, shared by every hub connection. Continuous sessions write raw
 * 16-bit mono PCM to a cache file; reads drain it from an offset, so an agent that processes a
 * chunk slowly still gets every second recorded in between. All calls block until the hardware
 * or file is ready, matching CameraHost's calling convention.
 */
class AudioRecorder(private val dir: File) {
    /** Called on a recording thread; the UI marshals it to the main thread. */
    var onRecording: ((Boolean) -> Unit)? = null

    private val lock = Any()
    private var session: Session? = null

    private class Session(val id: String, val file: File, val rate: Int, val maxBytes: Long) {
        @Volatile var written = 0L
        @Volatile var readOffset = 0L
        @Volatile var ended = false
        @Volatile var stopping = false
        @Volatile var error: String? = null
        @Volatile var record: AudioRecord? = null
        @Volatile var thread: Thread? = null
        var readers = 0
        var deletePending = false
    }

    fun start(maxDurationMs: Int, sampleRate: Int): JSONObject {
        stopLocked() // finishes the old session before taking the lock for the new one
        synchronized(lock) {
            val rate = sampleRate.coerceIn(8000, 48000)
            val id = Link.hex(Link.nonce()).take(16)
            val file = File.createTempFile("audio-", ".pcm", dir)
            val min = AudioRecord.getMinBufferSize(rate, AudioFormat.CHANNEL_IN_MONO, AudioFormat.ENCODING_PCM_16BIT)
            if (min <= 0) throw IllegalStateException("this device cannot record at $rate Hz")
            val rec = AudioRecord(
                MediaRecorder.AudioSource.MIC, rate, AudioFormat.CHANNEL_IN_MONO,
                AudioFormat.ENCODING_PCM_16BIT, max(min * 2, rate),
            )
            if (rec.state != AudioRecord.STATE_INITIALIZED) {
                rec.release()
                throw IllegalStateException("microphone unavailable")
            }
            val s = Session(id, file, rate, max(0, maxDurationMs).toLong() * rate * 2 / 1000)
            s.record = rec
            val out = FileOutputStream(file)
            rec.startRecording()
            s.thread = Thread {
                val buf = ShortArray(max(1024, min / 2))
                try {
                    out.use {
                        while (!s.stopping && !s.ended) {
                            val n = rec.read(buf, 0, buf.size)
                            if (n < 0) break
                            if (n == 0) continue
                            val bytes = ByteArray(n * 2)
                            ByteBuffer.wrap(bytes).order(ByteOrder.LITTLE_ENDIAN).asShortBuffer().put(buf, 0, n)
                            synchronized(lock) {
                                it.write(bytes)
                                s.written += bytes.size
                                // A zero limit means "until audio_stop".
                                if (s.maxBytes > 0 && s.written >= s.maxBytes) s.ended = true
                            }
                        }
                    }
                } catch (e: Exception) {
                    s.error = e.message ?: e.toString()
                } finally {
                    runCatching { rec.stop() }
                    rec.release()
                    s.ended = true
                    onRecording?.invoke(false)
                }
            }.also { it.isDaemon = true; it.start() }
            session = s
            onRecording?.invoke(true)
            return JSONObject().put("listenId", id).put("sampleRate", rate).put("maxDurationMs", maxDurationMs)
        }
    }

    fun read(id: String, maxMs: Int): Pair<JSONObject, ByteArray> =
        readChunk(id, maxMs.coerceIn(100, 30000))

    // chunkMs may exceed the protocol chunk cap for a one-shot clip: there the whole clip is
    // wanted in one reply, and the recorder stops on its own at that length.
    private fun readChunk(id: String, chunkMs: Int): Pair<JSONObject, ByteArray> {
        val s = synchronized(lock) {
            val cur = session ?: throw IllegalStateException("no such audio session")
            if (cur.id != id) throw IllegalStateException("no such audio session")
            cur.readers++
            cur
        }
        try {
            val want = chunkMs.toLong() * s.rate * 2 / 1000
            val deadline = System.currentTimeMillis() + chunkMs + 5000
            while (true) {
                val ready = synchronized(lock) { s.written - s.readOffset >= want || s.ended }
                if (ready || System.currentTimeMillis() >= deadline) break
                Thread.sleep(20)
            }
            var pcm = ByteArray(0)
            var ended: Boolean
            var totalMs: Int
            synchronized(lock) {
                s.error?.let { throw IllegalStateException(it) }
                val n = min(want, s.written - s.readOffset).toInt() and -2
                if (n > 0) {
                    RandomAccessFile(s.file, "r").use {
                        it.seek(s.readOffset)
                        pcm = ByteArray(n)
                        it.readFully(pcm)
                    }
                    s.readOffset += n
                }
                ended = s.ended && s.readOffset >= s.written
                totalMs = (s.written * 1000 / (s.rate * 2)).toInt()
            }
            val level = levels(pcm)
            val meta = JSONObject()
                .put("mime", "audio/wav").put("bytes", 44 + pcm.size).put("durationMs", pcm.size * 1000 / (s.rate * 2))
                .put("sampleRate", s.rate).put("totalMs", totalMs).put("ended", ended)
                .put("levels", level)
            return meta to wav(pcm, s.rate)
        } finally {
            synchronized(lock) {
                s.readers--
                if (s.readers == 0 && s.deletePending) s.file.delete()
            }
        }
    }

    /** Records one whole clip; the session limit is the clip length. */
    fun oneShot(durationMs: Int, sampleRate: Int): Pair<JSONObject, ByteArray> {
        val started = start(durationMs, sampleRate)
        val id = started.getString("listenId")
        return try {
            val (meta, pcm) = readChunk(id, durationMs.coerceIn(100, 60000))
            meta.put("durationMs", durationMs) to pcm
        } finally {
            // Also covers an agent that left mid-clip and pulled the session out from under us.
            runCatching { stop(id) }
        }
    }

    /** Measures the same signal a clip would contain, without returning the audio. */
    fun level(durationMs: Int, sampleRate: Int): JSONObject {
        val started = start(durationMs, sampleRate)
        val id = started.getString("listenId")
        return try {
            val (meta, wavBytes) = readChunk(id, durationMs.coerceIn(100, 60000))
            val pcm = wavBytes.copyOfRange(44, wavBytes.size)
            JSONObject().put("durationMs", durationMs).put("sampleRate", meta.optInt("sampleRate")).put("levels", levels(pcm))
        } finally {
            runCatching { stop(id) }
        }
    }

    /**
     * Stops the session with [id], or any session when null (the agent left). Safe to call from
     * a connection thread while another thread is blocked in read.
     */
    fun stop(id: String?): JSONObject {
        val s = synchronized(lock) {
            val cur = session ?: throw IllegalStateException("no such audio session")
            if (id != null && cur.id != id) throw IllegalStateException("no such audio session")
            session = null
            cur.stopping = true
            cur.ended = true
            runCatching { cur.record?.stop() }
            cur
        }
        finish(s)
        val totalMs = (s.written * 1000 / (s.rate * 2)).toInt()
        return JSONObject().put("durationMs", totalMs).put("totalMs", totalMs).put("ended", true)
    }

    // Detaches and finishes the current session. Called by start() before it takes the lock for
    // a new session, so the old AudioRecord is fully released first.
    private fun stopLocked() {
        val s = synchronized(lock) {
            val cur = session ?: return
            session = null
            cur.stopping = true
            cur.ended = true
            runCatching { cur.record?.stop() }
            cur
        }
        finish(s)
    }

    private fun finish(s: Session) {
        runCatching { s.thread?.join(2000) }
        runCatching { s.record?.release() }
        synchronized(lock) {
            if (s.readers == 0) s.file.delete() else s.deletePending = true
        }
    }

    private fun wav(pcm: ByteArray, rate: Int): ByteArray {
        val out = ByteArray(44 + pcm.size)
        val v = ByteBuffer.wrap(out).order(ByteOrder.LITTLE_ENDIAN)
        fun str(off: Int, s: String) = s.forEachIndexed { i, c -> out[off + i] = c.code.toByte() }
        str(0, "RIFF"); v.putInt(4, 36 + pcm.size); str(8, "WAVE"); str(12, "fmt ")
        v.putInt(16, 16); v.putShort(20, 1); v.putShort(22, 1)
        v.putInt(24, rate); v.putInt(28, rate * 2); v.putShort(32, 2); v.putShort(34, 16)
        str(36, "data"); v.putInt(40, pcm.size)
        pcm.copyInto(out, 44)
        return out
    }

    private fun levels(pcm: ByteArray): JSONObject {
        val n = pcm.size / 2
        if (n == 0) return JSONObject().put("rmsDb", -100).put("peakDb", -100).put("clipping", false)
        val buf = ByteBuffer.wrap(pcm).order(ByteOrder.LITTLE_ENDIAN).asShortBuffer()
        var sum = 0.0
        var peak = 0
        for (i in 0 until n) {
            val v = buf.get(i).toInt()
            sum += v.toDouble() * v
            peak = max(peak, abs(v))
        }
        fun db(x: Double): Double = if (x <= 0) -100.0 else max(-100.0, Math.round(20 * log10(x / 32768.0) * 10) / 10.0)
        return JSONObject().put("rmsDb", db(sqrt(sum / n))).put("peakDb", db(peak.toDouble())).put("clipping", peak >= 32767)
    }
}
