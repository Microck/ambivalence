package dev.micr.ambivalence

import android.hardware.camera2.CameraCharacteristics as C
import android.hardware.camera2.CaptureRequest as R
import android.hardware.camera2.params.MeteringRectangle
import android.hardware.camera2.params.RggbChannelVector
import android.graphics.Rect
import androidx.camera.camera2.interop.CaptureRequestOptions
import androidx.camera.camera2.interop.ExperimentalCamera2Interop
import org.json.JSONArray
import org.json.JSONObject
import kotlin.math.roundToInt

/** Camera2 ranges and validation. Normalized focus is 0=infinity, 1=nearest. */
@androidx.annotation.OptIn(ExperimentalCamera2Interop::class)
internal class ManualControls(private val chars: C) {
    private val manualSensor = chars.get(C.REQUEST_AVAILABLE_CAPABILITIES)?.contains(C.REQUEST_AVAILABLE_CAPABILITIES_MANUAL_SENSOR) == true
    private val manualColor = chars.get(C.REQUEST_AVAILABLE_CAPABILITIES)?.contains(C.REQUEST_AVAILABLE_CAPABILITIES_MANUAL_POST_PROCESSING) == true
    private val distance = chars.get(C.LENS_INFO_MINIMUM_FOCUS_DISTANCE) ?: 0f
    private val afModes = chars.get(C.CONTROL_AF_AVAILABLE_MODES) ?: intArrayOf()
    private val canAutoFocus = afModes.contains(R.CONTROL_AF_MODE_CONTINUOUS_PICTURE)
    private val canManualFocus = distance > 0 && afModes.contains(R.CONTROL_AF_MODE_OFF)
    private val evRange = chars.get(C.CONTROL_AE_COMPENSATION_RANGE)
    private val evStep = chars.get(C.CONTROL_AE_COMPENSATION_STEP)?.toDouble() ?: 0.0
    fun capabilities(): JSONObject {
        val c = JSONObject().put("controlsVersion", 1)
            .put("focusMode", JSONArray().apply { if (canAutoFocus) put("auto"); if (canManualFocus) put("manual") })
            .put("focusPoint", canAutoFocus && (chars.get(C.CONTROL_MAX_REGIONS_AF) ?: 0) > 0)
            .put("exposureMode", JSONArray(listOf("auto") + if (manualSensor) listOf("manual") else emptyList<String>()))
            .put("whiteBalanceMode", JSONArray(listOf("auto") + if (manualColor) listOf("manual") else emptyList<String>()))
        if (canManualFocus) c.put("focusPosition", range(0.0, 1.0))
        if (manualSensor) {
            chars.get(C.SENSOR_INFO_EXPOSURE_TIME_RANGE)?.let { c.put("exposureTime", range(it.lower / 1e9, it.upper / 1e9)) }
            chars.get(C.SENSOR_INFO_SENSITIVITY_RANGE)?.let { c.put("iso", range(it.lower.toDouble(), it.upper.toDouble())) }
        }
        if (evRange != null && evStep > 0) c.put("exposureCompensation", range(evRange.lower * evStep, evRange.upper * evStep).put("step", evStep))
        // Camera2 gains have no portable maximum. Drivers may reject unsupported values.
        if (manualColor) c.put("whiteBalanceGains", JSONObject().put("min", 1))
        return c
    }
    private fun range(lo: Double, hi: Double) = JSONObject().put("min", lo).put("max", hi)

    fun options(c: JSONObject): CaptureRequestOptions {
        val known = setOf("focusMode", "focusPosition", "focusPoint", "exposureMode", "exposureTime", "iso", "exposureCompensation", "whiteBalanceMode", "whiteBalanceGains", "whiteBalanceTemperature")
        require(c.keys().asSequence().all { it in known }) { "unknown control" }
        val b = CaptureRequestOptions.Builder()
        val caps = capabilities()
        fun mode(key: String): String? {
            if (!c.has(key)) return null
            val value = c.getString(key)
            val supported = caps.getJSONArray(key)
            require((0 until supported.length()).any { supported.getString(it) == value }) { "$key $value unsupported" }
            return value
        }
        fun num(key: String, lo: Double, hi: Double): Double {
            val value = c.getDouble(key)
            require(value.isFinite() && value >= lo && value <= hi) { "$key out of range $lo-$hi" }
            return value
        }
        val fm = mode("focusMode")
        require((fm == "manual") == c.has("focusPosition")) { "manual focus requires focusPosition" }
        if (fm == "manual") {
            b.setCaptureRequestOption(R.CONTROL_AF_MODE, R.CONTROL_AF_MODE_OFF)
            b.setCaptureRequestOption(R.LENS_FOCUS_DISTANCE, num("focusPosition", 0.0, 1.0).toFloat() * distance)
        }
        if (fm == "auto") b.setCaptureRequestOption(R.CONTROL_AF_MODE, R.CONTROL_AF_MODE_CONTINUOUS_PICTURE)
        if (c.has("focusPoint")) {
            require(fm == "auto" && caps.getBoolean("focusPoint")) { "focus point unsupported or not in auto mode" }
            val p = c.getJSONObject("focusPoint")
            val x = p.getDouble("x"); val y = p.getDouble("y")
            require(x.isFinite() && y.isFinite() && x in 0.0..1.0 && y in 0.0..1.0) { "invalid focus point" }
            val r = chars.get(C.SENSOR_INFO_ACTIVE_ARRAY_SIZE) ?: error("no sensor bounds")
            val w = maxOf(1, r.width() / 10); val h = maxOf(1, r.height() / 10)
            val left = (r.left + x * r.width() - w / 2).roundToInt().coerceIn(r.left, r.right - w)
            val top = (r.top + y * r.height() - h / 2).roundToInt().coerceIn(r.top, r.bottom - h)
            b.setCaptureRequestOption(R.CONTROL_AF_REGIONS, arrayOf(MeteringRectangle(Rect(left, top, left + w, top + h), 1000)))
        }
        val em = mode("exposureMode")
        if (em == "manual") {
            require(c.has("exposureTime") && c.has("iso") && !c.has("exposureCompensation")) { "manual exposure requires shutter and ISO without EV bias" }
            val tr = chars.get(C.SENSOR_INFO_EXPOSURE_TIME_RANGE) ?: error("shutter unsupported")
            val ir = chars.get(C.SENSOR_INFO_SENSITIVITY_RANGE) ?: error("ISO unsupported")
            val t = num("exposureTime", tr.lower / 1e9, tr.upper / 1e9)
            val iso = num("iso", ir.lower.toDouble(), ir.upper.toDouble())
            require(iso == iso.roundToInt().toDouble()) { "ISO must be an integer" }
            b.setCaptureRequestOption(R.CONTROL_AE_MODE, R.CONTROL_AE_MODE_OFF)
            b.setCaptureRequestOption(R.SENSOR_EXPOSURE_TIME, (t * 1e9).toLong())
            b.setCaptureRequestOption(R.SENSOR_SENSITIVITY, iso.toInt())
        } else {
            require(!c.has("exposureTime") && !c.has("iso")) { "shutter and ISO require manual mode" }
            if (em == "auto") b.setCaptureRequestOption(R.CONTROL_AE_MODE, R.CONTROL_AE_MODE_ON)
        }
        if (c.has("exposureCompensation")) {
            require(evRange != null && evStep > 0 && em != "manual") { "EV compensation unsupported" }
            val ev = num("exposureCompensation", evRange.lower * evStep, evRange.upper * evStep)
            b.setCaptureRequestOption(R.CONTROL_AE_EXPOSURE_COMPENSATION, (ev / evStep).roundToInt())
        }
        val wm = mode("whiteBalanceMode")
        require(!c.has("whiteBalanceTemperature")) { "use RGB gains on Android; kelvin unsupported" }
        if (wm == "manual") {
            val g = c.getJSONObject("whiteBalanceGains")
            fun gain(k: String): Float { val v = g.getDouble(k); require(v.isFinite() && v >= 1 && v <= Float.MAX_VALUE) { "invalid gain" }; return v.toFloat() }
            b.setCaptureRequestOption(R.CONTROL_AWB_MODE, R.CONTROL_AWB_MODE_OFF)
            b.setCaptureRequestOption(R.COLOR_CORRECTION_MODE, R.COLOR_CORRECTION_MODE_TRANSFORM_MATRIX)
            b.setCaptureRequestOption(R.COLOR_CORRECTION_GAINS, RggbChannelVector(gain("red"), gain("green"), gain("green"), gain("blue")))
        } else {
            require(!c.has("whiteBalanceGains")) { "gains require manual white balance" }
            if (wm == "auto") b.setCaptureRequestOption(R.CONTROL_AWB_MODE, R.CONTROL_AWB_MODE_AUTO)
        }
        return b.build()
    }
}
