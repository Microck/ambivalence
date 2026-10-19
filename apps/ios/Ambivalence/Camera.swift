import AVFoundation
import UIKit

/// The one camera every hub connection shares. Each operation (including a whole capture, from
/// shutter to JPEG) holds `gate`, so agents on different hubs and the on-screen buttons take turns.
/// AVCaptureSession and device mutations run on `queue`; replies report the device's own state
/// after the change, never the request. Mutable state is confined to `queue` (hence @unchecked Sendable).
final class Camera: NSObject, @unchecked Sendable {
    let session = AVCaptureSession()
    /// Called on the main queue whenever the torch may have changed.
    var onTorch: ((Bool) -> Void)?

    private let gate = Gate()
    private let queue = DispatchQueue(label: "ambivalence.camera")
    private let photo = AVCapturePhotoOutput()
    private var input: AVCaptureDeviceInput?
    private var shots: [Int64: CheckedContinuation<Data, Error>] = [:]

    /// Binds the back camera and starts the session; returns a problem to show, if any.
    func start() async -> String? {
        try? await exclusive {
            let err = self.bind(.back)
            if !self.session.isRunning { self.session.startRunning() }
            return err.map { "camera: \($0)" }
        }
    }

    func cameras() async throws -> [String: Any] {
        try await exclusive {
            let found = AVCaptureDevice.DiscoverySession(
                deviceTypes: [.builtInWideAngleCamera, .builtInUltraWideCamera, .builtInTelephotoCamera], mediaType: .video, position: .unspecified
            ).devices
            var reply = self.state(type: "cameras")
            reply["cameras"] = found.map { d -> [String: Any] in
                ["id": d.uniqueID, "label": d.localizedName, "facing": self.name(d.position), "torch": d.hasTorch,
                 "capabilities": self.capabilities(d),
                 "minZoom": Double(d.minAvailableVideoZoomFactor), "maxZoom": Double(d.maxAvailableVideoZoomFactor)]
            }
            reply["host"] = ["name": "ios", "platform": "ios"]
            return reply
        }
    }

    /// Applies a command (camera, torchOn, zoomLevel) and returns the resulting status.
    func command(camera: String?, torchOn: Bool?, zoom: Double?, controls: [String: Any]? = nil) async throws -> [String: Any] {
        try await exclusive {
            if let camera {
                if let err = self.select(camera) { throw LinkError(err) }
            }
            guard let device = self.input?.device else { throw LinkError("camera not ready") }
            if let torchOn, let err = self.setTorch(device, on: torchOn) { throw LinkError("torch failed: \(err)") }
            if let zoom, let err = self.setZoom(device, zoom) { throw LinkError(err) }
            if let controls { try self.configure(device, controls) }
            self.publishTorch()
            return self.state(type: "status")
        }
    }

    /// Takes one photo; returns the frame metadata (without id) and the JPEG.
    func capture(quality: Int, maxWidth: Int) async throws -> (meta: [String: Any], jpeg: Data) {
        await gate.lock()
        do {
            let data: Data = try await withCheckedThrowingContinuation { cont in
                queue.async {
                    guard self.input != nil, self.session.isRunning else { return cont.resume(throwing: LinkError("camera not ready")) }
                    let settings = AVCapturePhotoSettings(format: [AVVideoCodecKey: AVVideoCodecType.jpeg])
                    // Computational quality processing may override custom exposure values.
                    if self.input?.device.exposureMode == .custom { settings.photoQualityPrioritization = .speed }
                    if let conn = self.photo.connection(with: .video), conn.isVideoRotationAngleSupported(90) {
                        conn.videoRotationAngle = 90 // portrait, matching how the phone is held
                    }
                    self.shots[settings.uniqueID] = cont
                    self.photo.capturePhoto(with: settings, delegate: self)
                }
            }
            let out = try await onQueue { try self.encode(data, quality: quality, maxWidth: maxWidth) }
            await gate.unlock()
            return out
        } catch {
            await gate.unlock()
            throw error
        }
    }

    func flip() async throws {
        try await exclusive {
            if let err = self.bind(self.position == .back ? .front : .back) { throw LinkError("flip failed: \(err)") }
            self.publishTorch()
        }
    }

    func toggleTorch() async throws {
        try await exclusive {
            guard let device = self.input?.device else { throw LinkError("camera not ready") }
            if let err = self.setTorch(device, on: device.torchMode != .on) { throw LinkError(err) }
            self.publishTorch()
        }
    }

    // MARK: queue only

    private func exclusive<T>(_ body: @escaping () throws -> T) async throws -> T {
        await gate.lock()
        do {
            let r = try await onQueue(body)
            await gate.unlock()
            return r
        } catch {
            await gate.unlock()
            throw error
        }
    }

    private func onQueue<T>(_ body: @escaping () throws -> T) async throws -> T {
        try await withCheckedThrowingContinuation { cont in
            queue.async { cont.resume(with: Result { try body() }) }
        }
    }

    private var position: AVCaptureDevice.Position { input?.device.position ?? .back }

    private func bind(_ pos: AVCaptureDevice.Position) -> String? {
        guard let device = AVCaptureDevice.default(.builtInWideAngleCamera, for: .video, position: pos) else {
            return "no \(name(pos)) camera"
        }
        return bindDevice(device)
    }

    private func bindDevice(_ device: AVCaptureDevice) -> String? {
        if input?.device.uniqueID == device.uniqueID { return nil }
        let pos = device.position
        let newInput: AVCaptureDeviceInput
        do { newInput = try AVCaptureDeviceInput(device: device) } catch { return error.localizedDescription }
        session.beginConfiguration()
        defer { session.commitConfiguration() }
        session.sessionPreset = .photo
        if let old = input { session.removeInput(old) }
        guard session.canAddInput(newInput) else {
            if let old = input { session.addInput(old) }
            return "cannot use \(name(pos)) camera"
        }
        session.addInput(newInput)
        if !session.outputs.contains(photo), session.canAddOutput(photo) { session.addOutput(photo) }
        input = newInput
        do {
            try device.lockForConfiguration()
            defer { device.unlockForConfiguration() }
            if device.isFocusModeSupported(.continuousAutoFocus) { device.focusMode = .continuousAutoFocus }
            if device.isExposureModeSupported(.continuousAutoExposure) { device.exposureMode = .continuousAutoExposure }
            if device.isWhiteBalanceModeSupported(.continuousAutoWhiteBalance) { device.whiteBalanceMode = .continuousAutoWhiteBalance }
            device.setExposureTargetBias(0, completionHandler: nil)
            device.videoZoomFactor = max(device.minAvailableVideoZoomFactor, min(1, device.maxAvailableVideoZoomFactor))
            if device.hasTorch { device.torchMode = .off }
        } catch { return error.localizedDescription }
        return nil
    }

    private func setTorch(_ device: AVCaptureDevice, on: Bool) -> String? {
        guard device.hasTorch else { return on ? "\(name(device.position)) camera has no torch" : nil }
        do {
            try device.lockForConfiguration()
            defer { device.unlockForConfiguration() }
            if on { try device.setTorchModeOn(level: AVCaptureDevice.maxAvailableTorchLevel) } else { device.torchMode = .off }
        } catch { return error.localizedDescription }
        return nil
    }

    private func setZoom(_ device: AVCaptureDevice, _ z: Double) -> String? {
        let lo = Double(device.minAvailableVideoZoomFactor), hi = Double(device.maxAvailableVideoZoomFactor)
        guard z >= lo, z <= hi else { return "zoom \(z) out of range \(lo)-\(hi)" }
        do {
            try device.lockForConfiguration()
            device.videoZoomFactor = CGFloat(z)
            device.unlockForConfiguration()
        } catch { return error.localizedDescription }
        return nil
    }

    /// Draws the photo upright, scaled so the longest edge is <= maxWidth.
    private func encode(_ data: Data, quality: Int, maxWidth: Int) throws -> (meta: [String: Any], jpeg: Data) {
        guard let image = UIImage(data: data) else { throw LinkError("no image data") }
        let longest = max(image.size.width, image.size.height)
        let scale = min(1, CGFloat(maxWidth) / longest)
        let size = CGSize(width: (image.size.width * scale).rounded(), height: (image.size.height * scale).rounded())
        let format = UIGraphicsImageRendererFormat()
        format.scale = 1
        let jpeg = UIGraphicsImageRenderer(size: size, format: format).jpegData(
            withCompressionQuality: CGFloat(min(max(quality, 1), 100)) / 100
        ) { _ in image.draw(in: CGRect(origin: .zero, size: size)) }
        var meta = state(type: "frame")
        meta["mime"] = "image/jpeg"
        meta["bytes"] = jpeg.count
        meta["width"] = Int(size.width)
        meta["height"] = Int(size.height)
        meta.removeValue(forKey: "connected")
        return (meta, jpeg)
    }

    private func state(type: String) -> [String: Any] {
        let d = input?.device
        return [
            "type": type, "facing": name(position), "cameraId": d?.uniqueID ?? "",
            "torch": d?.torchMode == .on, "zoom": Double(d?.videoZoomFactor ?? 1), "connected": true,
            "capabilities": d.map { capabilities($0) } ?? [:],
            "settings": d.map { settings($0) } ?? [:],
        ]
    }

    private func select(_ id: String) -> String? {
        if ["front", "user", "back", "environment"].contains(id) {
            let p: AVCaptureDevice.Position = (id == "front" || id == "user") ? .front : .back
            return bind(p)
        }
        let devices = AVCaptureDevice.DiscoverySession(deviceTypes: [.builtInWideAngleCamera, .builtInUltraWideCamera, .builtInTelephotoCamera], mediaType: .video, position: .unspecified).devices
        guard let d = devices.first(where: { $0.uniqueID == id }) else { return "unknown camera \(id)" }
        return bindDevice(d)
    }

    private func capabilities(_ d: AVCaptureDevice) -> [String: Any] {
        let f = d.activeFormat
        return ["controlsVersion": 1, "focusMode": (d.isFocusModeSupported(.continuousAutoFocus) ? ["auto"] : []) + (d.isLockingFocusWithCustomLensPositionSupported ? ["manual"] : []),
                "focusPosition": ["min": 0, "max": 1], "focusPoint": d.isFocusPointOfInterestSupported,
                "exposureMode": (d.isExposureModeSupported(.continuousAutoExposure) ? ["auto"] : []) + (d.isExposureModeSupported(.custom) ? ["manual"] : []),
                "exposureTime": ["min": CMTimeGetSeconds(f.minExposureDuration), "max": CMTimeGetSeconds(f.maxExposureDuration)],
                "iso": ["min": f.minISO, "max": f.maxISO],
                "exposureCompensation": ["min": d.minExposureTargetBias, "max": d.maxExposureTargetBias],
                "whiteBalanceMode": (d.isWhiteBalanceModeSupported(.continuousAutoWhiteBalance) ? ["auto"] : []) + (d.isWhiteBalanceModeSupported(.locked) ? ["manual"] : []),
                "whiteBalanceGains": ["min": 1, "max": d.maxWhiteBalanceGain],
                "whiteBalanceTemperature": ["min": 1000, "max": 15000]]
    }

    private func settings(_ d: AVCaptureDevice) -> [String: Any] {
        let g = d.deviceWhiteBalanceGains
        return ["focusMode": d.focusMode == .locked ? "manual" : "auto", "focusPosition": d.lensPosition,
                "exposureMode": d.exposureMode == .custom ? "manual" : "auto", "exposureTime": CMTimeGetSeconds(d.exposureDuration),
                "iso": d.iso, "exposureCompensation": d.exposureTargetBias,
                "whiteBalanceMode": d.whiteBalanceMode == .locked ? "manual" : "auto",
                "whiteBalanceGains": ["red": g.redGain, "green": g.greenGain, "blue": g.blueGain]]
    }

    // Validate the whole request before applying any control group. Values are never clamped.
    private func configure(_ d: AVCaptureDevice, _ c: [String: Any]) throws {
        let known: Set<String> = ["focusMode", "focusPosition", "focusPoint", "exposureMode", "exposureTime", "iso", "exposureCompensation", "whiteBalanceMode", "whiteBalanceGains", "whiteBalanceTemperature"]
        guard Set(c.keys).isSubset(of: known) else { throw LinkError("unknown control") }
        func number(_ key: String, _ lo: Double, _ hi: Double) throws -> Double? {
            guard let raw = c[key] else { return nil }
            guard let n = raw as? NSNumber, n.doubleValue.isFinite, n.doubleValue >= lo, n.doubleValue <= hi else { throw LinkError("\(key) out of range \(lo)-\(hi)") }
            return n.doubleValue
        }
        for key in ["focusMode", "exposureMode", "whiteBalanceMode"] {
            if let v = c[key], !(v is String && ["auto", "manual"].contains(v as! String)) { throw LinkError("invalid \(key)") }
        }
        let fm = c["focusMode"] as? String, em = c["exposureMode"] as? String, wm = c["whiteBalanceMode"] as? String
        let fp = try number("focusPosition", 0, 1)
        guard (fm == "manual") == (fp != nil) else { throw LinkError("manual focus requires focusPosition") }
        if fm == "manual" && !d.isLockingFocusWithCustomLensPositionSupported { throw LinkError("manual focus unsupported") }
        if fm == "auto" && !d.isFocusModeSupported(.continuousAutoFocus) { throw LinkError("autofocus unsupported") }
        var point: CGPoint?
        if let raw = c["focusPoint"] {
            guard fm == "auto", d.isFocusPointOfInterestSupported, let p = raw as? [String: NSNumber],
                  let x = p["x"]?.doubleValue, let y = p["y"]?.doubleValue, x.isFinite, y.isFinite, (0...1).contains(x), (0...1).contains(y) else { throw LinkError("focusPoint unsupported or invalid") }
            point = CGPoint(x: x, y: y)
        }
        let t = try number("exposureTime", CMTimeGetSeconds(d.activeFormat.minExposureDuration), CMTimeGetSeconds(d.activeFormat.maxExposureDuration))
        let iso = try number("iso", Double(d.activeFormat.minISO), Double(d.activeFormat.maxISO))
        let ev = try number("exposureCompensation", Double(d.minExposureTargetBias), Double(d.maxExposureTargetBias))
        if em == "manual" {
            guard t != nil, iso != nil, ev == nil, d.isExposureModeSupported(.custom) else { throw LinkError("manual exposure requires supported shutter and ISO without EV bias") }
        } else if t != nil || iso != nil { throw LinkError("shutter and ISO require manual exposure") }
        if ev != nil && em != "auto" && d.exposureMode == .custom { throw LinkError("EV bias requires auto exposure") }
        if em == "auto" && !d.isExposureModeSupported(.continuousAutoExposure) { throw LinkError("auto exposure unsupported") }
        let temp = try number("whiteBalanceTemperature", 1000, 15000)
        var gains: AVCaptureDevice.WhiteBalanceGains?
        if let raw = c["whiteBalanceGains"] {
            guard let g = raw as? [String: NSNumber], let r = g["red"], let green = g["green"], let b = g["blue"] else { throw LinkError("invalid RGB gains") }
            gains = AVCaptureDevice.WhiteBalanceGains(redGain: r.floatValue, greenGain: green.floatValue, blueGain: b.floatValue)
        }
        if let temp { gains = d.deviceWhiteBalanceGains(for: .init(temperature: Float(temp), tint: 0)) }
        if wm == "manual" {
            guard (c["whiteBalanceGains"] != nil) != (temp != nil), d.isWhiteBalanceModeSupported(.locked), let g = gains,
                  [g.redGain, g.greenGain, g.blueGain].allSatisfy({ $0.isFinite && $0 >= 1 && $0 <= d.maxWhiteBalanceGain }) else { throw LinkError("unsupported white balance or gains out of range") }
        } else if gains != nil { throw LinkError("gains/temperature require manual white balance") }
        if wm == "auto" && !d.isWhiteBalanceModeSupported(.continuousAutoWhiteBalance) { throw LinkError("auto white balance unsupported") }
        try d.lockForConfiguration()
        let applied = DispatchGroup()
        if let fp { applied.enter(); d.setFocusModeLocked(lensPosition: Float(fp)) { _ in applied.leave() } }
        if fm == "auto" { if let point { d.focusPointOfInterest = point }; d.focusMode = .continuousAutoFocus }
        if let t, let iso { applied.enter(); d.setExposureModeCustom(duration: CMTime(seconds: t, preferredTimescale: 1_000_000_000), iso: Float(iso)) { _ in applied.leave() } }
        if em == "auto" { d.exposureMode = .continuousAutoExposure }
        if let ev { applied.enter(); d.setExposureTargetBias(Float(ev)) { _ in applied.leave() } }
        if let gains { applied.enter(); d.setWhiteBalanceModeLocked(with: gains) { _ in applied.leave() } }
        if wm == "auto" { d.whiteBalanceMode = .continuousAutoWhiteBalance }
        d.unlockForConfiguration()
        guard applied.wait(timeout: .now() + 8) == .success else { throw LinkError("camera control timed out; query status before retrying") }
    }

    private func publishTorch() {
        let on = input?.device.torchMode == .on
        DispatchQueue.main.async { self.onTorch?(on) }
    }

    private func name(_ p: AVCaptureDevice.Position) -> String { p == .front ? "front" : "back" }
}

extension Camera: AVCapturePhotoCaptureDelegate {
    func photoOutput(_ output: AVCapturePhotoOutput, didFinishProcessingPhoto photo: AVCapturePhoto, error: Error?) {
        queue.async {
            guard let shot = self.shots.removeValue(forKey: photo.resolvedSettings.uniqueID) else { return }
            if let error { return shot.resume(throwing: error) }
            guard let data = photo.fileDataRepresentation() else { return shot.resume(throwing: LinkError("no image data")) }
            shot.resume(returning: data)
        }
    }
}

/// A FIFO async mutex: actors alone are reentrant across awaits, so they cannot hold a camera turn.
actor Gate {
    private var busy = false
    private var waiters: [CheckedContinuation<Void, Never>] = []

    func lock() async {
        guard busy else { busy = true; return }
        await withCheckedContinuation { waiters.append($0) }
    }

    func unlock() {
        if waiters.isEmpty { busy = false } else { waiters.removeFirst().resume() }
    }
}
