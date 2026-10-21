package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Microck/ambivalence/internal/config"
	"github.com/Microck/ambivalence/internal/hub"
	"github.com/Microck/ambivalence/internal/peer"
	"github.com/Microck/ambivalence/internal/protocol"
)

type Options struct {
	Listen    string
	HubURL    string
	PublicHub string
	StartHub  bool
}

type Server struct {
	opt     Options
	hub     *hub.Hub
	mu      sync.Mutex
	agent   *peer.Session
	hubURL  string
	pairing chan struct{} // non-nil while a code pairing waits for the user's Confirm tap
	// audioListenID is the phone-side continuous listening session, if one is open.
	audioListenID string
}

func Run(ctx context.Context, opt Options) error {
	s := &Server{opt: opt, hubURL: opt.HubURL}
	if opt.StartHub {
		if opt.Listen == "" {
			opt.Listen = protocol.DefaultListen
		}
		h := hub.New(opt.Listen)
		s.hub = h
		errc := make(chan error, 1)
		go func() { errc <- h.Start(ctx) }()
		if err := waitListen(h, 3*time.Second); err != nil {
			return err
		}
		if s.hubURL == "" {
			s.hubURL = "ws://" + rewriteLocal(h.ListenAddr()) + protocol.Path
		}
		log.Printf("hub %s", h.ListenAddr())
		select {
		case err := <-errc:
			if err != nil {
				return err
			}
		default:
		}
	}
	if s.hubURL == "" {
		s.hubURL = config.HubURL("")
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "ambivalence",
		Version: "0.2.2",
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "camera_status",
		Description: "Hub listen address, pair state, and last camera status. Use before capture if you are unsure whether a phone is paired.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, Title: "Camera status"},
	}, s.cameraStatus)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "camera_connect",
		Description: "Connect to the user's phone camera. After the first pairing no arguments are needed: it joins the linked phone and waits for the user to open the ambivalence app. Only the very first time, pass the 6-character code shown on the phone: the tool returns a confirmation code at once; ask the user to check the phone shows the same code and tap Confirm. Other camera tools connect automatically, so calling this is optional.",
		Annotations: &mcp.ToolAnnotations{Title: "Connect camera"},
	}, s.cameraConnect)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "camera_disconnect",
		Description: "Drop the current agent pairing. The hub keeps running.",
		Annotations: &mcp.ToolAnnotations{Title: "Disconnect camera"},
	}, s.cameraDisconnect)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "camera_capture",
		Description: "Take a still JPEG from the paired camera. Returns the image plus size/facing/torch metadata. This is the main way to see a physical object.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, Title: "Capture frame"},
	}, s.cameraCapture)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "camera_list",
		Description: "List selectable camera/lens IDs, labels, zoom ranges and manual control capabilities.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, Title: "List cameras"},
	}, s.cameraList)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "camera_switch",
		Description: "Switch the active camera. facing is front or back, or a camera id from camera_list.",
		Annotations: &mcp.ToolAnnotations{Title: "Switch camera"},
	}, s.cameraSwitch)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "camera_torch",
		Description: "Turn the flashlight/torch on or off on the paired camera.",
		Annotations: &mcp.ToolAnnotations{Title: "Torch"},
	}, s.cameraTorch)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "camera_zoom",
		Description: "Set zoom ratio on the paired camera (1.0 is default).",
		Annotations: &mcp.ToolAnnotations{Title: "Zoom"},
	}, s.cameraZoom)

	mcp.AddTool(server, &mcp.Tool{Name: "camera_configure", Description: "Configure focus, shutter duration/ISO, EV bias and white balance. Call camera_list first for supported controls/ranges. Switch lenses separately; omitted control groups stay unchanged. Unsupported settings return errors."}, s.cameraConfigure)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "audio_record",
		Description: "Record a short clip (up to 60 seconds) from the paired phone's microphone and return it as WAV audio, with duration/loudness metadata. Use for one-off sounds such as a beep, a fan or a machine. For longer listening use audio_listen_start and audio_listen_next, which return chunks until you stop.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, Title: "Record audio"},
	}, s.audioRecord)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "audio_listen_start",
		Description: "Start continuous listening on the paired phone. The phone buffers audio, so call audio_listen_next whenever you are ready and nothing between calls is lost. Use audio_listen_stop when done. The phone shows a recording indicator while the session is open.",
		Annotations: &mcp.ToolAnnotations{Title: "Start listening"},
	}, s.audioListenStart)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "audio_listen_next",
		Description: "Return the next chunk (default 10s, max 30s) of the active listening session as WAV audio. Call it again to keep listening; audio recorded while you were busy is buffered. ended=true means the session hit its maximum length or was stopped.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, Title: "Next audio chunk"},
	}, s.audioListenNext)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "audio_listen_stop",
		Description: "Stop the active continuous listening session and close the phone's microphone.",
		Annotations: &mcp.ToolAnnotations{Title: "Stop listening"},
	}, s.audioListenStop)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "audio_level",
		Description: "Measure loudness for a short window (default 1s, max 30s) without returning audio. Returns RMS and peak dBFS plus a clipping flag. Cheap way to check whether a room, fan or machine is audible.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, Title: "Audio level"},
	}, s.audioLevel)
	log.Printf("mcp stdio hub=%s", s.hubURL)
	return server.Run(ctx, &mcp.StdioTransport{})
}

type emptyArgs struct{}

type connectArgs struct {
	Code      string `json:"code,omitempty" jsonschema:"6-character code from the phone; only for the first pairing"`
	Hub       string `json:"hub,omitempty" jsonschema:"WebSocket hub URL if not using the in-process hub"`
	TimeoutMs int    `json:"timeoutMs,omitempty" jsonschema:"milliseconds to wait for the phone, default 60000"`
}

type captureArgs struct {
	Quality  *int `json:"quality,omitempty" jsonschema:"JPEG quality 1-100, default 80"`
	MaxWidth *int `json:"maxWidth,omitempty" jsonschema:"longest edge in pixels, default 1280"`
}

type switchArgs struct {
	Camera string `json:"camera" jsonschema:"front, back, or a camera id"`
}

type torchArgs struct {
	On bool `json:"on" jsonschema:"true turns the torch on"`
}

type zoomArgs struct {
	Factor float64 `json:"factor" jsonschema:"zoom ratio, 1.0 is no zoom"`
}

type audioArgs struct {
	DurationMs *int `json:"durationMs,omitempty" jsonschema:"clip length in milliseconds, default 5000, max 60000"`
}

type listenStartArgs struct {
	MaxDurationMs *int `json:"maxDurationMs,omitempty" jsonschema:"session limit in milliseconds, default 600000, max 7200000; recording stops when it is reached"`
}

type listenNextArgs struct {
	ChunkMs *int `json:"chunkMs,omitempty" jsonschema:"maximum chunk length in milliseconds, default 10000, max 30000"`
}

type levelArgs struct {
	DurationMs *int `json:"durationMs,omitempty" jsonschema:"measurement window in milliseconds, default 1000, max 30000"`
}

func (s *Server) cameraStatus(ctx context.Context, _ *mcp.CallToolRequest, _ emptyArgs) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, _ := config.Load()
	out := map[string]any{
		"hub":       s.hubURL,
		"publicHub": s.opt.PublicHub,
		"listen":    "",
		"linked":    cfg.Device != "",
		"connected": false,
		"listening": s.audioListenID != "",
	}
	if s.hub != nil {
		out["listen"] = s.hub.ListenAddr()
	}
	if s.agent != nil {
		st := s.agent.Status()
		out["connected"] = st.Connected
		out["facing"] = st.Facing
		out["torch"] = st.Torch
		out["zoom"] = st.Zoom
		out["cameraId"] = st.CameraID
		out["settings"] = st.Settings
		out["capabilities"] = st.Capabilities
		if st.Host != nil {
			out["host"] = st.Host
		}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return textResult(string(b)), out, nil
}

func (s *Server) cameraConnect(ctx context.Context, _ *mcp.CallToolRequest, args connectArgs) (*mcp.CallToolResult, any, error) {
	hubURL := s.hubURL
	if strings.TrimSpace(args.Hub) != "" {
		hubURL = args.Hub
	}
	if args.Code != "" {
		return s.startPairing(hubURL, args.Code)
	}
	timeout := 60 * time.Second
	if args.TimeoutMs > 0 {
		timeout = time.Duration(args.TimeoutMs) * time.Millisecond
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err := s.connect(cctx, hubURL); err != nil {
		return errResult(err), nil, nil
	}
	return textResult("connected to the phone camera"), map[string]any{"hub": hubURL}, nil
}

// startPairing returns as soon as the confirmation code is known, so the agent can tell the
// user what to check; pairing then completes in the background when the user taps Confirm.
func (s *Server) startPairing(hubURL, code string) (*mcp.CallToolResult, any, error) {
	s.dropAgent()
	sasCh, errCh, done := make(chan string, 1), make(chan error, 1), make(chan struct{})
	s.mu.Lock()
	s.pairing = done
	s.mu.Unlock()
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		sess, err := peer.ConnectAgent(ctx, hubURL, code, func(sas string) { sasCh <- sas })
		s.mu.Lock()
		s.pairing = nil
		if err == nil {
			s.agent, s.hubURL = sess, hubURL
		}
		s.mu.Unlock()
		errCh <- err
	}()
	select {
	case sas := <-sasCh:
		msg := fmt.Sprintf("Pairing started. Ask the user to check that the phone shows %s and tap Confirm there. Camera tools will work once they do.", sas)
		return textResult(msg), map[string]any{"confirmCode": sas}, nil
	case err := <-errCh:
		return errResult(err), nil, nil
	}
}

func (s *Server) dropAgent() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audioListenID = ""
	if s.agent != nil {
		_ = s.agent.Close()
		s.agent = nil
	}
}

// connect replaces the current session with one to the saved linked phone.
func (s *Server) connect(ctx context.Context, hubURL string) (*peer.Session, error) {
	s.dropAgent()
	sess, err := peer.ConnectAgent(ctx, hubURL, "", nil)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.agent, s.hubURL = sess, hubURL
	s.mu.Unlock()
	return sess, nil
}

func (s *Server) cameraDisconnect(ctx context.Context, _ *mcp.CallToolRequest, _ emptyArgs) (*mcp.CallToolResult, any, error) {
	s.dropAgent()
	return textResult("disconnected"), nil, nil
}

func (s *Server) cameraCapture(ctx context.Context, _ *mcp.CallToolRequest, args captureArgs) (*mcp.CallToolResult, any, error) {
	sess, err := s.requireAgent(ctx)
	if err != nil {
		return errResult(err), nil, nil
	}
	q, w := protocol.DefaultQuality, protocol.DefaultMaxWidth
	if args.Quality != nil {
		q = *args.Quality
	}
	if args.MaxWidth != nil {
		w = *args.MaxWidth
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	fr, err := sess.Capture(cctx, q, w)
	if err != nil {
		return errResult(err), nil, nil
	}
	meta := fmt.Sprintf("%dx%d facing=%s torch=%v zoom=%.2f bytes=%d", fr.Meta.Width, fr.Meta.Height, fr.Meta.Facing, fr.Meta.Torch, fr.Meta.Zoom, len(fr.Data))
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.ImageContent{Data: fr.Data, MIMEType: "image/jpeg"},
			&mcp.TextContent{Text: meta},
		},
	}, map[string]any{"width": fr.Meta.Width, "height": fr.Meta.Height, "facing": fr.Meta.Facing, "torch": fr.Meta.Torch, "bytes": len(fr.Data), "cameraId": fr.Meta.CameraID, "settings": fr.Meta.Settings}, nil
}

func (s *Server) audioRecord(ctx context.Context, _ *mcp.CallToolRequest, args audioArgs) (*mcp.CallToolResult, any, error) {
	duration := protocol.DefaultAudioMs
	if args.DurationMs != nil {
		duration = *args.DurationMs
	}
	if duration < 100 || duration > protocol.MaxOneShotAudioMs {
		return errResult(fmt.Errorf("duration must be between 100 and %d ms", protocol.MaxOneShotAudioMs)), nil, nil
	}
	sess, err := s.requireAgent(ctx)
	if err != nil {
		return errResult(err), nil, nil
	}
	// A one-shot clip replaces an open listening session: two sessions on one microphone
	// would only confuse the agent and keep the indicator lit.
	s.stopListening(sess)
	if !sess.Alive() {
		sess, err = s.requireAgent(ctx)
		if err != nil {
			return errResult(err), nil, nil
		}
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(duration+15_000)*time.Millisecond)
	defer cancel()
	fr, err := sess.Listen(cctx, duration, 0)
	if err != nil {
		if cctx.Err() != nil || !sess.Alive() {
			s.dropAgent()
		}
		return errResult(err), nil, nil
	}
	return audioResult(fr), audioMeta(fr), nil
}

func (s *Server) audioListenStart(ctx context.Context, _ *mcp.CallToolRequest, args listenStartArgs) (*mcp.CallToolResult, any, error) {
	maxMs := protocol.DefaultSessionMs
	if args.MaxDurationMs != nil {
		maxMs = *args.MaxDurationMs
	}
	if maxMs < 0 || maxMs > protocol.MaxSessionMs {
		return errResult(fmt.Errorf("max duration must be between 0 and %d ms", protocol.MaxSessionMs)), nil, nil
	}
	sess, err := s.requireAgent(ctx)
	if err != nil {
		return errResult(err), nil, nil
	}
	s.stopListening(sess)
	if !sess.Alive() {
		sess, err = s.requireAgent(ctx)
		if err != nil {
			return errResult(err), nil, nil
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	started, err := sess.AudioStart(cctx, maxMs, 0)
	if err != nil {
		if cctx.Err() != nil || !sess.Alive() {
			s.dropAgent()
		}
		return errResult(err), nil, nil
	}
	s.mu.Lock()
	s.audioListenID = started.ListenID
	s.mu.Unlock()
	limit := "until stopped"
	if started.MaxDurationMs > 0 {
		limit = fmt.Sprintf("max %.1fs", float64(started.MaxDurationMs)/1000)
	}
	msg := fmt.Sprintf("listening (session %s, %s). Call audio_listen_next for the next chunk; the phone buffers audio between calls. Use audio_listen_stop when done.", started.ListenID, limit)
	return textResult(msg), map[string]any{"listenId": started.ListenID, "sampleRate": started.SampleRate, "maxDurationMs": started.MaxDurationMs}, nil
}

func (s *Server) audioListenNext(ctx context.Context, _ *mcp.CallToolRequest, args listenNextArgs) (*mcp.CallToolResult, any, error) {
	chunk := protocol.DefaultChunkMs
	if args.ChunkMs != nil {
		chunk = *args.ChunkMs
	}
	if chunk < 100 || chunk > protocol.MaxChunkMs {
		return errResult(fmt.Errorf("chunk must be between 100 and %d ms", protocol.MaxChunkMs)), nil, nil
	}
	s.mu.Lock()
	id := s.audioListenID
	s.mu.Unlock()
	if id == "" {
		return errResult(fmt.Errorf("no active listening session: call audio_listen_start first")), nil, nil
	}
	sess, err := s.requireAgent(ctx)
	if err != nil {
		return errResult(err), nil, nil
	}
	// The phone fills the requested chunk in real time, so the call lasts about chunkMs.
	cctx, cancel := context.WithTimeout(ctx, time.Duration(chunk+20_000)*time.Millisecond)
	defer cancel()
	fr, err := sess.AudioRead(cctx, id, chunk)
	if err != nil {
		if cctx.Err() != nil || !sess.Alive() {
			s.dropAgent()
		}
		return errResult(err), nil, nil
	}
	if fr.Meta.Ended {
		s.mu.Lock()
		if s.audioListenID == id {
			s.audioListenID = ""
		}
		s.mu.Unlock()
	}
	if len(fr.Data) <= 44 { // a WAV header with no samples
		b, _ := json.Marshal(fr.Meta)
		return textResult("no new audio; session ended=" + fmt.Sprint(fr.Meta.Ended) + "\n" + string(b)), audioMeta(fr), nil
	}
	return audioResult(fr), audioMeta(fr), nil
}

func (s *Server) audioListenStop(ctx context.Context, _ *mcp.CallToolRequest, _ emptyArgs) (*mcp.CallToolResult, any, error) {
	id := s.takeListenID()
	if id == "" {
		return errResult(fmt.Errorf("no active listening session")), nil, nil
	}
	sess, err := s.requireAgent(ctx)
	if err != nil {
		return errResult(err), nil, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	meta, err := sess.AudioStop(cctx, id)
	if err != nil {
		if cctx.Err() != nil || !sess.Alive() {
			s.dropAgent()
		}
		return errResult(err), nil, nil
	}
	b, _ := json.MarshalIndent(meta, "", "  ")
	return textResult(string(b)), meta, nil
}

func (s *Server) audioLevel(ctx context.Context, _ *mcp.CallToolRequest, args levelArgs) (*mcp.CallToolResult, any, error) {
	duration := 1000
	if args.DurationMs != nil {
		duration = *args.DurationMs
	}
	if duration < 100 || duration > protocol.MaxChunkMs {
		return errResult(fmt.Errorf("duration must be between 100 and %d ms", protocol.MaxChunkMs)), nil, nil
	}
	sess, err := s.requireAgent(ctx)
	if err != nil {
		return errResult(err), nil, nil
	}
	// Level uses the one-shot path, which replaces any open session on the phone. Close the
	// session here first so a later audio_listen_next cannot address a session that is gone.
	s.stopListening(sess)
	if !sess.Alive() {
		sess, err = s.requireAgent(ctx)
		if err != nil {
			return errResult(err), nil, nil
		}
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(duration+15_000)*time.Millisecond)
	defer cancel()
	fr, err := sess.Level(cctx, duration)
	if err != nil {
		if cctx.Err() != nil || !sess.Alive() {
			s.dropAgent()
		}
		return errResult(err), nil, nil
	}
	b, _ := json.MarshalIndent(fr.Meta, "", "  ")
	return textResult(string(b)), fr.Meta, nil
}

// stopListening closes an open session, ignoring failures: the session may already be gone
// with a phone that reconnected, and the next request would fail for the same reason anyway.
func (s *Server) stopListening(sess *peer.Session) {
	id := s.takeListenID()
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := sess.AudioStop(ctx, id); err != nil && (ctx.Err() != nil || !sess.Alive()) {
		s.dropAgent()
	}
}

func (s *Server) takeListenID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.audioListenID
	s.audioListenID = ""
	return id
}

func audioMeta(fr peer.Frame) map[string]any {
	m := map[string]any{
		"bytes":      len(fr.Data),
		"mime":       fr.Meta.Mime,
		"durationMs": fr.Meta.DurationMs,
		"sampleRate": fr.Meta.SampleRate,
		"totalMs":    fr.Meta.TotalMs,
		"ended":      fr.Meta.Ended,
	}
	if fr.Meta.Levels != nil {
		m["rmsDb"] = fr.Meta.Levels.RMSDB
		m["peakDb"] = fr.Meta.Levels.PeakDB
		m["clipping"] = fr.Meta.Levels.Clipping
	}
	return m
}

func audioResult(fr peer.Frame) *mcp.CallToolResult {
	meta := fmt.Sprintf("duration=%.1fs sampleRate=%d bytes=%d", float64(fr.Meta.DurationMs)/1000, fr.Meta.SampleRate, len(fr.Data))
	if fr.Meta.TotalMs > 0 {
		meta += fmt.Sprintf(" total=%.1fs", float64(fr.Meta.TotalMs)/1000)
	}
	if fr.Meta.Levels != nil {
		meta += fmt.Sprintf(" rms=%.1fdB peak=%.1fdB clipping=%v", fr.Meta.Levels.RMSDB, fr.Meta.Levels.PeakDB, fr.Meta.Levels.Clipping)
	}
	if fr.Meta.Ended {
		meta += " ended=true"
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.AudioContent{Data: fr.Data, MIMEType: fr.Meta.Mime},
			&mcp.TextContent{Text: meta},
		},
	}
}

func (s *Server) cameraList(ctx context.Context, _ *mcp.CallToolRequest, _ emptyArgs) (*mcp.CallToolResult, any, error) {
	sess, err := s.requireAgent(ctx)
	if err != nil {
		return errResult(err), nil, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	fr, err := sess.Cameras(cctx)
	if err != nil {
		return errResult(err), nil, nil
	}
	b, _ := json.MarshalIndent(fr.Meta, "", "  ")
	return textResult(string(b)), fr.Meta, nil
}

func (s *Server) cameraSwitch(ctx context.Context, _ *mcp.CallToolRequest, args switchArgs) (*mcp.CallToolResult, any, error) {
	return s.command(ctx, protocol.Message{Camera: args.Camera})
}

func (s *Server) cameraTorch(ctx context.Context, _ *mcp.CallToolRequest, args torchArgs) (*mcp.CallToolResult, any, error) {
	on := args.On
	return s.command(ctx, protocol.Message{TorchOn: &on})
}

func (s *Server) cameraZoom(ctx context.Context, _ *mcp.CallToolRequest, args zoomArgs) (*mcp.CallToolResult, any, error) {
	z := args.Factor
	return s.command(ctx, protocol.Message{ZoomLevel: &z})
}

func (s *Server) command(ctx context.Context, msg protocol.Message) (*mcp.CallToolResult, any, error) {
	sess, err := s.requireAgent(ctx)
	if err != nil {
		return errResult(err), nil, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	fr, err := sess.Command(cctx, msg)
	if err != nil {
		return errResult(err), nil, nil
	}
	b, _ := json.MarshalIndent(fr.Meta, "", "  ")
	return textResult(string(b)), fr.Meta, nil
}

// requireAgent returns a live session, joining the linked phone if there is none (or the
// last one dropped). Waits up to 30s so "open the app" after asking still works.
func (s *Server) requireAgent(ctx context.Context) (*peer.Session, error) {
	s.mu.Lock()
	sess, hubURL, pairing := s.agent, s.hubURL, s.pairing
	s.mu.Unlock()
	if pairing != nil { // the user is about to tap Confirm; use that session
		select {
		case <-pairing:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		s.mu.Lock()
		sess = s.agent
		s.mu.Unlock()
	}
	if sess != nil && sess.Alive() {
		return sess, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return s.connect(cctx, hubURL)
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func errResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}
}

func waitListen(h *hub.Hub, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		c, err := net.DialTimeout("tcp", rewriteLocal(h.ListenAddr()), 50*time.Millisecond)
		if err == nil {
			c.Close()
			return nil
		}
	}
	return fmt.Errorf("hub did not listen on %s", h.ListenAddr())
}

func rewriteLocal(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	// A wildcard listen address is not dialable; reach the local hub over loopback.
	if host == "0.0.0.0" || host == "::" || host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func init() {
	log.SetOutput(os.Stderr)
}

func (s *Server) cameraConfigure(ctx context.Context, _ *mcp.CallToolRequest, args protocol.Controls) (*mcp.CallToolResult, any, error) {
	if err := args.Validate(); err != nil {
		return errResult(err), nil, nil
	}
	return s.command(ctx, protocol.Message{Controls: &args})
}
