package dev.micr.ambivalence

import android.Manifest
import android.content.Intent
import android.content.pm.PackageManager
import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.graphics.Matrix
import android.media.ExifInterface
import android.os.Bundle
import android.view.Surface
import android.view.View
import android.view.inputmethod.EditorInfo
import android.view.inputmethod.InputMethodManager
import android.widget.Button
import android.widget.EditText
import android.widget.TextView
import androidx.appcompat.app.AppCompatActivity
import androidx.camera.core.Camera
import androidx.camera.core.CameraSelector
import androidx.camera.core.ImageCapture
import androidx.camera.core.ImageCaptureException
import androidx.camera.core.Preview
import androidx.camera.lifecycle.ProcessCameraProvider
import androidx.camera.view.PreviewView
import androidx.core.app.ActivityCompat
import androidx.core.content.ContextCompat
import org.json.JSONArray
import org.json.JSONObject
import java.io.ByteArrayOutputStream
import java.io.File
import java.util.concurrent.CompletableFuture
import java.util.concurrent.ExecutionException
import java.util.concurrent.ExecutorService
import java.util.concurrent.Executors
import java.util.concurrent.Semaphore
import java.util.concurrent.TimeUnit
import java.util.concurrent.TimeoutException

class MainActivity : AppCompatActivity(), Hubs.Ui, CameraHost {
    private lateinit var preview: PreviewView
    private lateinit var pairCode: TextView
    private lateinit var status: TextView
    private lateinit var hint: TextView
    private lateinit var hubUrl: EditText
    private lateinit var torchButton: Button
    private lateinit var confirmView: View
    private lateinit var sasView: TextView
    private lateinit var hubs: Hubs
    private lateinit var discovery: HubDiscovery
    private lateinit var cameraExecutor: ExecutorService
    private val prefs by lazy { getSharedPreferences("ambivalence", MODE_PRIVATE) }
    private val identity by lazy { Identity(prefs) }

    private var imageCapture: ImageCapture? = null
    private var camera: Camera? = null
    private var lensFacing = CameraSelector.LENS_FACING_BACK
    private var torchOn = false
    private var zoom = 1.0

    // Agents on different hubs share one camera: one request at a time, in arrival order.
    private val cameraLock = Semaphore(1, true)

    // Pairing confirmations from any connection, shown one at a time.
    private val confirms = ArrayDeque<Pair<String, CompletableFuture<Boolean>>>()

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)
        preview = findViewById(R.id.preview)
        pairCode = findViewById(R.id.pair_code)
        status = findViewById(R.id.status)
        hint = findViewById(R.id.hint)
        hubUrl = findViewById(R.id.hub_url)
        torchButton = findViewById(R.id.torch)
        confirmView = findViewById(R.id.confirm)
        sasView = findViewById(R.id.sas)
        hubs = Hubs(identity, this, this)
        discovery = HubDiscovery(this) { urls -> hubs.discovered(urls) }
        cameraExecutor = Executors.newSingleThreadExecutor()
        onChanged()

        hubUrl.setText(hubFrom(intent) ?: prefs.getString("hub", ""))
        hubUrl.setOnEditorActionListener { _, action, _ ->
            if (action == EditorInfo.IME_ACTION_GO) { connect(); true } else false
        }
        findViewById<Button>(R.id.connect).setOnClickListener { connect() }
        findViewById<Button>(R.id.flip).setOnClickListener {
            val next = if (lensFacing == CameraSelector.LENS_FACING_BACK) CameraSelector.LENS_FACING_FRONT else CameraSelector.LENS_FACING_BACK
            bindCamera(next) { err -> if (err != null) onNote("flip failed: ${err.message}") }
        }
        torchButton.setOnClickListener {
            val cam = camera ?: return@setOnClickListener
            if (!cam.cameraInfo.hasFlashUnit()) return@setOnClickListener onNote("this camera has no torch")
            val want = !torchOn
            val fut = cam.cameraControl.enableTorch(want)
            fut.addListener({
                try { fut.get(); syncTorch(cam, want) } catch (e: Exception) { onNote("torch failed: ${e.cause?.message ?: e.message}") }
            }, mainExecutor)
        }

        if (ContextCompat.checkSelfPermission(this, Manifest.permission.CAMERA) == PackageManager.PERMISSION_GRANTED) {
            startHosting()
        } else {
            ActivityCompat.requestPermissions(this, arrayOf(Manifest.permission.CAMERA), 7)
        }
    }

    // ambivalence://host?hub=wss://…/v1, printed as a QR code by `ambivalence hub`.
    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        hubFrom(intent)?.let { hubUrl.setText(it); connect() }
    }

    private fun hubFrom(intent: Intent?): String? = intent?.data?.getQueryParameter("hub")?.takeIf { it.isNotBlank() }

    private fun startHosting() {
        bindCamera(lensFacing) { err -> if (err != null) onNote("camera: ${err.message}") }
        discovery.start()
        if (hubUrl.text.isNotBlank()) connect() else onChanged()
    }

    // The typed URL becomes the saved hub; discovered hubs stay connected alongside it.
    private fun connect() {
        val url = hubUrl.text.toString().trim()
        getSystemService(InputMethodManager::class.java).hideSoftInputFromWindow(hubUrl.windowToken, 0)
        hubUrl.clearFocus()
        if (!url.startsWith("ws://") && !url.startsWith("wss://")) return onNote("hub URL must start with ws:// or wss://")
        prefs.edit().putString("hub", url).apply()
        hubs.setSaved(url)
    }

    // The code stays visible so a new agent can still pair; once linked, the hint says so.
    override fun onChanged() {
        val c = if (::hubs.isInitialized) hubs.code() else null
        pairCode.text = c ?: "------"
        hint.text = when {
            identity.key != null -> "linked: just ask your agent to use the camera"
            c == null -> ""
            else -> "first time only, on the agent: ambivalence pair $c"
        }
        if (::hubs.isInitialized) status.text = hubs.summary()
    }

    override fun onNote(text: String) {
        status.text = text
    }

    override fun confirm(sas: String, answer: CompletableFuture<Boolean>) {
        confirms.addLast(sas to answer)
        answer.whenComplete { _, _ -> runOnUiThread { showNextConfirm() } }
        showNextConfirm()
    }

    private fun showNextConfirm() {
        while (confirms.firstOrNull()?.second?.isDone == true) confirms.removeFirst()
        val (sas, answer) = confirms.firstOrNull() ?: run {
            confirmView.visibility = View.GONE
            onChanged()
            return
        }
        getSystemService(InputMethodManager::class.java).hideSoftInputFromWindow(hubUrl.windowToken, 0)
        sasView.text = sas
        findViewById<Button>(R.id.sas_cancel).setOnClickListener { answer.complete(false) }
        findViewById<Button>(R.id.sas_ok).setOnClickListener { answer.complete(true) }
        confirmView.visibility = View.VISIBLE
    }

    // `known` is the value an enableTorch future just confirmed; torchState LiveData can lag it.
    private fun syncTorch(cam: Camera, known: Boolean? = null) {
        torchOn = known ?: (cam.cameraInfo.torchState.value == 1)
        torchButton.text = if (torchOn) "torch on" else "torch off"
    }

    override fun onRequestPermissionsResult(requestCode: Int, permissions: Array<out String>, grantResults: IntArray) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults)
        if (grantResults.isNotEmpty() && grantResults[0] == PackageManager.PERMISSION_GRANTED) {
            startHosting()
        } else {
            onNote("camera permission denied")
        }
    }

    // Binds the requested lens and calls done on the main thread once the camera is live,
    // or with the failure. Callers must not report success before done(null).
    private fun bindCamera(facing: Int, done: (Throwable?) -> Unit) {
        val providerFuture = ProcessCameraProvider.getInstance(this)
        providerFuture.addListener({
            try {
                val provider = providerFuture.get()
                val selector = CameraSelector.Builder().requireLensFacing(facing).build()
                if (!provider.hasCamera(selector)) {
                    done(IllegalStateException("no ${facingName(facing)} camera"))
                    return@addListener
                }
                val previewUse = Preview.Builder().build().also { it.setSurfaceProvider(preview.surfaceProvider) }
                val capture = ImageCapture.Builder()
                    .setCaptureMode(ImageCapture.CAPTURE_MODE_MINIMIZE_LATENCY)
                    .build()
                provider.unbindAll()
                camera = provider.bindToLifecycle(this, selector, previewUse, capture)
                imageCapture = capture
                lensFacing = facing
                // A new camera starts with the torch off and zoom 1.0.
                syncTorch(camera!!)
                zoom = 1.0
                done(null)
            } catch (e: Exception) {
                done(e)
            }
        }, mainExecutor)
    }

    // Runs work on the main thread while holding the camera, and blocks the calling connection
    // until it completes the future. Failures surface as exceptions with a user-facing message.
    private fun <T> withCamera(work: (CompletableFuture<T>) -> Unit): T {
        if (!cameraLock.tryAcquire(30, TimeUnit.SECONDS)) throw IllegalStateException("camera busy")
        try {
            val f = CompletableFuture<T>()
            runOnUiThread {
                try { work(f) } catch (e: Exception) { f.completeExceptionally(e) }
            }
            return try {
                f.get(20, TimeUnit.SECONDS)
            } catch (e: ExecutionException) {
                throw (e.cause as? Exception) ?: e
            } catch (e: TimeoutException) {
                throw IllegalStateException("camera timed out")
            }
        } finally {
            cameraLock.release()
        }
    }

    private fun fail(msg: String): Nothing = throw IllegalStateException(msg)

    override fun capture(quality: Int, maxWidth: Int): Pair<JSONObject, ByteArray> = withCamera { f ->
        val cap = imageCapture ?: fail("camera not ready")
        cap.targetRotation = preview.display?.rotation ?: Surface.ROTATION_0
        val facing = facingName(lensFacing)
        val meta = JSONObject().put("facing", facing).put("cameraId", facing).put("torch", torchOn).put("zoom", zoom)
        val file = File(cacheDir, "shot-${System.nanoTime()}.jpg")
        val opts = ImageCapture.OutputFileOptions.Builder(file).build()
        cap.takePicture(opts, cameraExecutor, object : ImageCapture.OnImageSavedCallback {
            override fun onImageSaved(output: ImageCapture.OutputFileResults) {
                try {
                    val shot = encodeForAgent(file, quality, maxWidth)
                    f.complete(meta.put("width", shot.width).put("height", shot.height) to shot.jpeg)
                } catch (e: Exception) {
                    f.completeExceptionally(IllegalStateException(e.message ?: "encode failed"))
                } finally {
                    file.delete()
                }
            }
            override fun onError(exception: ImageCaptureException) {
                file.delete()
                f.completeExceptionally(IllegalStateException(exception.message ?: "capture failed"))
            }
        })
    }

    private class Shot(val jpeg: ByteArray, val width: Int, val height: Int)

    // Sensor JPEGs are ~12 MP and carry rotation only in EXIF. Agents want an
    // upright image with the longest edge <= maxWidth, so decode, rotate, scale, re-encode.
    private fun encodeForAgent(file: File, quality: Int, maxWidth: Int): Shot {
        val bounds = BitmapFactory.Options().apply { inJustDecodeBounds = true }
        BitmapFactory.decodeFile(file.path, bounds)
        val longest = maxOf(bounds.outWidth, bounds.outHeight)
        var sample = 1
        while (longest / (sample * 2) >= maxWidth) sample *= 2
        val decoded = BitmapFactory.decodeFile(file.path, BitmapFactory.Options().apply { inSampleSize = sample })
            ?: throw IllegalStateException("decode failed")
        val degrees = when (ExifInterface(file.path).getAttributeInt(ExifInterface.TAG_ORIENTATION, ExifInterface.ORIENTATION_NORMAL)) {
            ExifInterface.ORIENTATION_ROTATE_90 -> 90f
            ExifInterface.ORIENTATION_ROTATE_180 -> 180f
            ExifInterface.ORIENTATION_ROTATE_270 -> 270f
            else -> 0f
        }
        val scale = minOf(1f, maxWidth.toFloat() / maxOf(decoded.width, decoded.height))
        val matrix = Matrix().apply {
            postRotate(degrees)
            postScale(scale, scale)
        }
        val out = Bitmap.createBitmap(decoded, 0, 0, decoded.width, decoded.height, matrix, true)
        val buf = ByteArrayOutputStream()
        out.compress(Bitmap.CompressFormat.JPEG, quality.coerceIn(1, 100), buf)
        return Shot(buf.toByteArray(), out.width, out.height)
    }

    override fun command(camera: String?, torchOn: Boolean?, zoom: Double?): JSONObject = withCamera { f ->
        val target = when (camera) {
            null -> lensFacing
            "front", "user" -> CameraSelector.LENS_FACING_FRONT
            "back", "environment" -> CameraSelector.LENS_FACING_BACK
            else -> fail("unknown camera $camera (want front or back)")
        }
        if (target == lensFacing && this.camera != null) {
            applyControls(f, torchOn, zoom)
        } else {
            bindCamera(target) { err ->
                if (err != null) f.completeExceptionally(IllegalStateException("switch failed: ${err.message}")) else applyControls(f, torchOn, zoom)
            }
        }
    }

    // Torch then zoom, each awaited. The ack reports the camera's own state, not the request.
    private fun applyControls(f: CompletableFuture<JSONObject>, torch: Boolean?, zoomReq: Double?) {
        val err = { msg: String -> f.completeExceptionally(IllegalStateException(msg)); Unit }
        val cam = camera ?: return err("camera not ready")
        if (torch != null && torch && !cam.cameraInfo.hasFlashUnit()) {
            return err("${facingName(lensFacing)} camera has no torch")
        }
        if (zoomReq != null) {
            val zs = cam.cameraInfo.zoomState.value
            if (zs != null && (zoomReq < zs.minZoomRatio || zoomReq > zs.maxZoomRatio)) {
                return err("zoom $zoomReq out of range ${zs.minZoomRatio}-${zs.maxZoomRatio}")
            }
        }
        val ack = {
            syncTorch(cam, torch)
            zoom = (cam.cameraInfo.zoomState.value?.zoomRatio ?: 1f).toDouble()
            val side = facingName(lensFacing)
            f.complete(JSONObject().put("facing", side).put("cameraId", side).put("torch", torchOn).put("zoom", zoom).put("connected", true))
            Unit
        }
        val doZoom = {
            if (zoomReq == null) {
                ack()
            } else {
                val fut = cam.cameraControl.setZoomRatio(zoomReq.toFloat())
                fut.addListener({
                    try { fut.get(); ack() } catch (e: Exception) { err("zoom failed: ${e.cause?.message ?: e.message}") }
                }, mainExecutor)
            }
        }
        if (torch == null) {
            doZoom()
        } else {
            val fut = cam.cameraControl.enableTorch(torch)
            fut.addListener({
                try { fut.get(); doZoom() } catch (e: Exception) { err("torch failed: ${e.cause?.message ?: e.message}") }
            }, mainExecutor)
        }
    }

    override fun cameras(): JSONObject = withCamera { f ->
        val providerFuture = ProcessCameraProvider.getInstance(this)
        providerFuture.addListener({
            try {
                val cams = JSONArray()
                for (info in providerFuture.get().availableCameraInfos) {
                    val facing = when (info.lensFacing) {
                        CameraSelector.LENS_FACING_BACK -> "back"
                        CameraSelector.LENS_FACING_FRONT -> "front"
                        else -> "external"
                    }
                    // Zoom limits are only published for the bound camera.
                    val zs = if (info == camera?.cameraInfo) info.zoomState.value else null
                    cams.put(
                        JSONObject().put("id", facing).put("facing", facing).put("torch", info.hasFlashUnit())
                            .put("minZoom", (zs?.minZoomRatio ?: 1f).toDouble()).put("maxZoom", (zs?.maxZoomRatio ?: 1f).toDouble()),
                    )
                }
                f.complete(
                    JSONObject().put("facing", facingName(lensFacing)).put("torch", torchOn).put("zoom", zoom).put("cameras", cams)
                        .put("host", JSONObject().put("name", "android").put("platform", "android")),
                )
            } catch (e: Exception) {
                f.completeExceptionally(e)
            }
        }, mainExecutor)
    }

    private fun facingName(f: Int) = if (f == CameraSelector.LENS_FACING_FRONT) "front" else "back"

    override fun onDestroy() {
        super.onDestroy()
        discovery.stop()
        hubs.stop()
        confirms.forEach { it.second.complete(false) }
        cameraExecutor.shutdown()
    }
}
