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
                deviceTypes: [.builtInWideAngleCamera], mediaType: .video, position: .unspecified
            ).devices
            var reply = self.state(type: "cameras")
            reply["cameras"] = found.map { d -> [String: Any] in
                ["id": self.name(d.position), "facing": self.name(d.position), "torch": d.hasTorch,
                 "minZoom": Double(d.minAvailableVideoZoomFactor), "maxZoom": Double(d.maxAvailableVideoZoomFactor)]
            }
            reply["host"] = ["name": "ios", "platform": "ios"]
            return reply
        }
    }

    /// Applies a command (camera, torchOn, zoomLevel) and returns the resulting status.
    func command(camera: String?, torchOn: Bool?, zoom: Double?) async throws -> [String: Any] {
        try await exclusive {
            if let camera {
                let pos: AVCaptureDevice.Position
                switch camera {
                case "front", "user": pos = .front
                case "back", "environment": pos = .back
                default: throw LinkError("unknown camera \(camera) (want front or back)")
                }
                if pos != self.position || self.input == nil, let err = self.bind(pos) { throw LinkError("switch failed: \(err)") }
            }
            guard let device = self.input?.device else { throw LinkError("camera not ready") }
            if let torchOn, let err = self.setTorch(device, on: torchOn) { throw LinkError("torch failed: \(err)") }
            if let zoom, let err = self.setZoom(device, zoom) { throw LinkError(err) }
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
            "type": type, "facing": name(position), "cameraId": name(position),
            "torch": d?.torchMode == .on, "zoom": Double(d?.videoZoomFactor ?? 1), "connected": true,
        ]
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
