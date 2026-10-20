import AVFoundation
import SwiftUI

struct ContentView: View {
    @StateObject private var host = CameraHost()
    @FocusState private var editingHub: Bool
    @State private var confirmingForget = false

    var body: some View {
        ZStack {
            CameraPreview(session: host.session)
                .ignoresSafeArea()
                .onTapGesture { editingHub = false }
            VStack(spacing: 0) {
                VStack(alignment: .leading, spacing: 4) {
                    HStack(spacing: 8) {
                        Mark().frame(width: 22, height: 22)
                        Text("ambivalence").font(.system(size: 15, weight: .bold))
                    }
                    .padding(.bottom, 4)
                    Text(host.status)
                        .font(.system(size: 15))
                        .accessibilityIdentifier("status_text")
                    Text(host.code)
                        .font(.system(size: 44, weight: .bold, design: .monospaced))
                        .kerning(6)
                        .textSelection(.enabled)
                        .accessibilityIdentifier("pair_code")
                    Text(host.linked ? "linked: just ask your agent to use the camera" : host.code == "------" ? "" : "first time only, on the agent: ambivalence pair \(host.code)")
                        .font(.system(size: 13, design: .monospaced))
                    if host.linked {
                        Button("forget agent", role: .destructive) { confirmingForget = true }
                            .font(.system(size: 13, design: .monospaced))
                            .foregroundStyle(.white)
                            .frame(minHeight: 44)
                            .accessibilityIdentifier("forget_agent")
                            .disabled(host.forgettingAgent)
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.horizontal, 20)
                .padding(.vertical, 12)
                .background(.black.opacity(0.6))

                Spacer()

                VStack(spacing: 12) {
                    TextField("hub URL, e.g. wss://….trycloudflare.com/v1", text: $host.hubURL)
                        .font(.system(size: 14))
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .keyboardType(.URL)
                        .submitLabel(.go)
                        .focused($editingHub)
                        .onSubmit { host.connect() }
                        .padding(.horizontal, 12)
                        .frame(height: 48)
                        .overlay(RoundedRectangle(cornerRadius: 6).stroke(.white, lineWidth: 1))
                        .accessibilityIdentifier("hub_url")
                    HStack(spacing: 12) {
                        HostButton(title: "connect", id: "connect_button") { editingHub = false; host.connect() }
                        HostButton(title: "flip", id: "flip_button") { host.flip() }
                        HostButton(title: host.torchOn ? "torch on" : "torch off", id: "torch_button") { host.toggleTorch() }
                    }
                }
                .padding(.horizontal, 20)
                .padding(.top, 12)
                .padding(.bottom, 8)
                .background(.black.opacity(0.6))
            }
            .foregroundStyle(.white)

            if let sas = host.sas {
                PairConfirm(sas: sas, answer: host.answer)
            }
        }
        .background(Color.black)
        .confirmationDialog("forget this agent?", isPresented: $confirmingForget, titleVisibility: .visible) {
            Button("forget agent", role: .destructive) {
                Task { await host.forgetAgent() }
            }
            Button("cancel", role: .cancel) {}
        } message: {
            Text("This disconnects all agent sessions. Pair again to use the camera.")
        }
        .onAppear { host.start() }
        .onChange(of: host.sas) { _, sas in if sas != nil { editingHub = false } }
        // ambivalence://host?hub=wss://…/v1, printed as a QR code by `ambivalence hub`.
        .onOpenURL { url in
            if let hub = URLComponents(url: url, resolvingAgainstBaseURL: false)?
                .queryItems?.first(where: { $0.name == "hub" })?.value, !hub.isEmpty {
                editingHub = false
                host.hubURL = hub
                host.connect()
            }
        }
    }
}

private struct HostButton: View {
    let title: String
    let id: String
    let action: () -> Void
    var body: some View {
        Button(action: action) {
            Text(title).font(.system(size: 15)).frame(maxWidth: .infinity, minHeight: 48)
                .contentShape(Rectangle())
        }
        .overlay(RoundedRectangle(cornerRadius: 6).stroke(.white, lineWidth: 1))
        .accessibilityIdentifier(id)
    }
}

/// Numeric comparison for pairing: the agent shows the same code unless someone is in the middle.
private struct PairConfirm: View {
    let sas: String
    let answer: (Bool) -> Void
    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("pair with this agent?").font(.system(size: 15))
            Text("check it shows the same code").font(.system(size: 15))
            Text(sas)
                .font(.system(size: 44, weight: .bold, design: .monospaced))
                .kerning(6)
                .accessibilityIdentifier("sas_code")
            HStack(spacing: 12) {
                HostButton(title: "cancel", id: "sas_cancel") { answer(false) }
                HostButton(title: "confirm", id: "sas_confirm") { answer(true) }
            }
        }
        .padding(.horizontal, 20)
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .leading)
        .background(Color.black.ignoresSafeArea())
        .foregroundStyle(.white)
    }
}

/// The ambivalence mark; same geometry as assets/logo/mark.svg on its 24-unit grid.
/// Drawn as two overlapping fills (ring with pupil, then the stem) so no winding rules are involved.
struct Mark: View {
    var color: Color = .white
    var body: some View {
        GeometryReader { g in
            let s = min(g.size.width, g.size.height) / 24
            ZStack(alignment: .topLeading) {
                Path { p in
                    p.addEllipse(in: CGRect(x: 3 * s, y: 4 * s, width: 16 * s, height: 16 * s))
                    p.addEllipse(in: CGRect(x: 6.2 * s, y: 7.2 * s, width: 9.6 * s, height: 9.6 * s))
                    p.addEllipse(in: CGRect(x: 10.6 * s, y: 8.9 * s, width: 3.4 * s, height: 3.4 * s))
                }
                .fill(color, style: FillStyle(eoFill: true))
                Path(CGRect(x: 17.8 * s, y: 4 * s, width: 3.2 * s, height: 16 * s)).fill(color)
            }
        }
        .aspectRatio(1, contentMode: .fit)
    }
}

struct CameraPreview: UIViewRepresentable {
    let session: AVCaptureSession
    func makeUIView(context: Context) -> PreviewView {
        let v = PreviewView()
        v.previewLayer.session = session
        v.previewLayer.videoGravity = .resizeAspectFill
        return v
    }
    func updateUIView(_ uiView: PreviewView, context: Context) {}
}

final class PreviewView: UIView {
    override class var layerClass: AnyClass { AVCaptureVideoPreviewLayer.self }
    var previewLayer: AVCaptureVideoPreviewLayer { layer as! AVCaptureVideoPreviewLayer }
}
