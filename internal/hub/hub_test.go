package hub_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image/jpeg"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Microck/ambivalence/internal/audio"
	"github.com/Microck/ambivalence/internal/host"
	"github.com/Microck/ambivalence/internal/hub"
	"github.com/Microck/ambivalence/internal/link"
	"github.com/Microck/ambivalence/internal/peer"
	"github.com/Microck/ambivalence/internal/protocol"
)

func startHub(t *testing.T, ctx context.Context) string {
	t.Helper()
	h := hub.New("127.0.0.1:0")
	go func() { _ = h.Start(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if addr := h.ListenAddr(); addr != "127.0.0.1:0" {
			if c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond); err == nil {
				c.Close()
				return "ws://" + addr + protocol.Path
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("hub not listening")
	return ""
}

// serve runs the phone on wsURL and returns the pair code the hub assigned.
func serve(t *testing.T, ctx context.Context, p *host.Phone, wsURL string) string {
	t.Helper()
	codes := make(chan string, 1)
	go func() { _ = p.Serve(ctx, wsURL, func(c string) { codes <- c }) }()
	select {
	case c := <-codes:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("phone did not connect")
		return ""
	}
}

// pair runs first-time pairing and checks both sides showed the same confirmation code.
func pair(t *testing.T, ctx context.Context, p *host.Phone, wsURL, code string) (*peer.Session, []byte) {
	t.Helper()
	var phoneSAS string
	p.Confirm = func(s string) bool { phoneSAS = s; return true }
	s, err := peer.Dial(ctx, peer.DialOptions{Role: protocol.RoleAgent, Code: code, HubURL: wsURL})
	if err != nil {
		t.Fatal(err)
	}
	var agentSAS string
	device, key, err := s.Pair(ctx, func(sas string) { agentSAS = sas })
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	if device != p.Device || agentSAS == "" || agentSAS != phoneSAS || !bytes.Equal(key, p.Key()) {
		t.Fatalf("pairing mismatch: device %q/%q sas %q/%q", device, p.Device, agentSAS, phoneSAS)
	}
	return s, key
}

// linked joins the linked phone like ConnectAgent: a just-closed agent may still hold the slot
// for a moment, so "busy" is retried briefly.
func linked(ctx context.Context, wsURL string, device string, key []byte) (*peer.Session, error) {
	s, err := peer.Dial(ctx, peer.DialOptions{Role: protocol.RoleAgent, Device: device, Key: key, HubURL: wsURL})
	for i := 0; errors.Is(err, peer.ErrPhoneBusy) && i < 20; i++ {
		time.Sleep(50 * time.Millisecond)
		s, err = peer.Dial(ctx, peer.DialOptions{Role: protocol.RoleAgent, Device: device, Key: key, HubURL: wsURL})
	}
	if err != nil {
		return nil, err
	}
	if err := s.WaitAuthed(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func TestPairAndCapture(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wsURL := startHub(t, ctx)
	phone := host.NewPhone("test")
	sess, _ := pair(t, ctx, phone, wsURL, serve(t, ctx, phone, wsURL))
	defer sess.Close()

	fr, err := sess.Capture(ctx, 70, 640)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if img, err := jpeg.Decode(bytes.NewReader(fr.Data)); err != nil || img.Bounds().Dx() != 640 {
		t.Fatalf("decode: %v", err)
	}
	on := true
	if got, err := sess.Command(ctx, protocol.Message{TorchOn: &on}); err != nil || !got.Meta.Torch {
		t.Fatalf("torch: %v", err)
	}
}

// Audio travels like camera frames: sealed, binary, and only after the phone advertises support.
// A short clip and a continuous session both return playable WAV chunks.
func TestAudioClipAndSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wsURL := startHub(t, ctx)
	phone := host.NewPhone("test")
	sess, _ := pair(t, ctx, phone, wsURL, serve(t, ctx, phone, wsURL))
	defer sess.Close()

	clip, err := sess.Listen(ctx, 300, 16000)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	pcm, err := audio.PCMFromWAV(clip.Data)
	if err != nil || len(pcm) == 0 || clip.Meta.Mime != protocol.AudioMime {
		t.Fatalf("clip: %v (%d bytes, mime %q)", err, len(pcm), clip.Meta.Mime)
	}
	if clip.Meta.Levels == nil || clip.Meta.Levels.PeakDB == -100 {
		t.Fatalf("clip level: %+v", clip.Meta.Levels)
	}

	started, err := sess.AudioStart(ctx, 3000, 16000)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if started.ListenID == "" || started.Type != protocol.TypeAudioStarted {
		t.Fatalf("started: %+v", started)
	}
	chunk, err := sess.AudioRead(ctx, started.ListenID, 250)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if pcm, err := audio.PCMFromWAV(chunk.Data); err != nil || len(pcm) == 0 {
		t.Fatalf("chunk: %v (%d bytes)", err, len(pcm))
	}
	if chunk.Meta.Ended {
		t.Fatal("session ended before its limit")
	}
	stopped, err := sess.AudioStop(ctx, started.ListenID)
	if err != nil || stopped.Type != protocol.TypeStatus || stopped.TotalMs <= 0 {
		t.Fatalf("stop: %v %+v", err, stopped)
	}

	level, err := sess.Level(ctx, 200)
	if err != nil || level.Meta.Levels == nil || level.Meta.Type != protocol.TypeLevels {
		t.Fatalf("level: %v %+v", err, level.Meta)
	}
}

func TestCancelledPairingFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wsURL := startHub(t, ctx)
	phone := host.NewPhone("test")
	phone.Confirm = func(string) bool { return false } // user taps Cancel
	code := serve(t, ctx, phone, wsURL)
	s, err := peer.Dial(ctx, peer.DialOptions{Role: protocol.RoleAgent, Code: code, HubURL: wsURL})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := s.Pair(ctx, func(string) {}); err == nil {
		t.Fatal("pairing succeeded although the user cancelled")
	}
	if phone.Key() != nil {
		t.Fatal("phone stored a key after cancel")
	}
}

// After one pairing the agent reaches the phone by device id + key alone, across a phone
// restart (new code), and keeps the same session through it. A wrong key is refused by the phone.
func TestLinkedAgentAcrossPhoneRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	wsURL := startHub(t, ctx)
	phone := host.NewPhone("test")
	phoneCtx, phoneStop := context.WithCancel(ctx)
	paired, key := pair(t, ctx, phone, wsURL, serve(t, phoneCtx, phone, wsURL))
	paired.Close()

	if _, err := linked(ctx, wsURL, phone.Device, bytes.Repeat([]byte{7}, 32)); err == nil {
		t.Fatal("wrong key accepted")
	}
	agent, err := linked(ctx, wsURL, phone.Device, key)
	if err != nil {
		t.Fatalf("linked: %v", err)
	}
	defer agent.Close()

	phoneStop()
	time.Sleep(100 * time.Millisecond)
	serve(t, ctx, phone, wsURL)
	if _, err := agent.Capture(ctx, 60, 320); err != nil {
		t.Fatalf("same session after phone restart: %v", err)
	}
}

// The phone serves two hubs at once; its linked agent works on whichever one it uses.
func TestTwoHubs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	hubA, hubB := startHub(t, ctx), startHub(t, ctx)
	phone := host.NewPhone("test")
	codeA := serve(t, ctx, phone, hubA)
	serve(t, ctx, phone, hubB)
	s, key := pair(t, ctx, phone, hubA, codeA)
	s.Close()
	for _, u := range []string{hubB, hubA} {
		a, err := linked(ctx, u, phone.Device, key)
		if err != nil {
			t.Fatalf("linked via %s: %v", u, err)
		}
		if _, err := a.Capture(ctx, 60, 320); err != nil {
			t.Fatalf("capture via %s: %v", u, err)
		}
		a.Close()
	}
}

// rogueHub is a hostile hub on the Wi-Fi: it records everything a phone or agent sends it.
type rogueHub struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []string
	act  func(ws *websocket.Conn, hello protocol.Message)
}

func newRogue(act func(ws *websocket.Conn, hello protocol.Message)) *rogueHub {
	r := &rogueHub{act: act}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ws, err := up.Upgrade(w, req, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		r.record(data)
		hello, _ := protocol.Decode(data)
		go func() {
			for {
				_, d, err := ws.ReadMessage()
				if err != nil {
					return
				}
				r.record(d)
			}
		}()
		r.act(ws, hello)
		time.Sleep(2 * time.Second)
	}))
	return r
}

func (r *rogueHub) record(b []byte) { r.mu.Lock(); r.seen = append(r.seen, string(b)); r.mu.Unlock() }
func (r *rogueHub) url() string     { return "ws" + strings.TrimPrefix(r.srv.URL, "http") + protocol.Path }
func (r *rogueHub) all() string     { r.mu.Lock(); defer r.mu.Unlock(); return strings.Join(r.seen, "\n") }
func send(ws *websocket.Conn, m protocol.Message) {
	b, _ := protocol.Encode(m)
	_ = ws.WriteMessage(websocket.TextMessage, b)
}

// A linked phone that connects to a rogue hub (e.g. via a forged mDNS advert) reveals no
// secret, rejects the rogue's fake agent, and never sends a frame.
func TestRogueHubGetsNothingFromPhone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wsURL := startHub(t, ctx)
	phone := host.NewPhone("test")
	s, key := pair(t, ctx, phone, wsURL, serve(t, ctx, phone, wsURL))
	s.Close()

	rogue := newRogue(func(ws *websocket.Conn, _ protocol.Message) {
		send(ws, protocol.Message{Type: protocol.TypeWelcome, Code: "AAAAAA", Connected: true})
		time.Sleep(200 * time.Millisecond)
		send(ws, protocol.Message{Type: protocol.TypeAuth, Nonce: strings.Repeat("00", 16), MAC: strings.Repeat("ab", 32)})
		send(ws, protocol.Message{ID: "x", Type: protocol.TypeCapture})
	})
	defer rogue.srv.Close()
	go func() { _ = phone.Serve(ctx, rogue.url(), nil) }()
	time.Sleep(1500 * time.Millisecond)

	got := rogue.all()
	if strings.Contains(got, link.Hex(key)) {
		t.Fatal("phone leaked the link key to a rogue hub")
	}
	if !strings.Contains(got, `"type":"reject"`) {
		t.Fatalf("phone did not reject the rogue agent:\n%s", got)
	}
	if strings.Contains(got, `"type":"frame"`) {
		t.Fatal("phone sent a frame to an unauthenticated agent")
	}
}

// An agent pointed at a rogue hub that impersonates the phone refuses it: it answers one fresh
// challenge (useless for replay) but never accepts the fake phone's proof or sends a request.
func TestAgentRefusesRoguePhone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	key := bytes.Repeat([]byte{9}, 32)
	rogue := newRogue(func(ws *websocket.Conn, hello protocol.Message) {
		send(ws, protocol.Message{Type: protocol.TypeWelcome, Code: "AAAAAA", Connected: true})
		send(ws, protocol.Message{Type: protocol.TypeAuth, Nonce: strings.Repeat("11", 16)})
		time.Sleep(200 * time.Millisecond)
		send(ws, protocol.Message{Type: protocol.TypeAuthOK, Nonce: strings.Repeat("11", 16), MAC: strings.Repeat("cd", 32)})
	})
	defer rogue.srv.Close()
	if _, err := linked(ctx, rogue.url(), "victimdevice0000", key); err == nil {
		t.Fatal("agent accepted a phone that cannot prove the link")
	}
	got := rogue.all()
	if strings.Contains(got, link.Hex(key)) || strings.Contains(got, `"type":"capture"`) {
		t.Fatalf("agent leaked the key or sent a request to a rogue phone:\n%s", got)
	}
}

// mitm is a hub on one client's path (phone or agent) that relays everything to the real hub (so
// a genuine handshake succeeds through it) and can then inject or tamper with traffic.
type mitm struct {
	srv        *httptest.Server
	mu         sync.Mutex
	client     *websocket.Conn
	fromClient []string                      // text the phone sent
	toClient   [][]byte                      // text the agent sent the phone (via the real hub)
	tamperUp   func(mt int, b []byte) []byte // returns nil to drop
}

func newMITM(realHub string) *mitm {
	m := &mitm{}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ph, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ph.Close()
		hb, _, err := websocket.DefaultDialer.Dial(realHub, nil)
		if err != nil {
			return
		}
		defer hb.Close()
		m.mu.Lock()
		m.client = ph
		m.mu.Unlock()
		go func() { // real hub -> phone
			for {
				mt, b, err := hb.ReadMessage()
				if err != nil {
					ph.Close()
					return
				}
				m.mu.Lock()
				if mt == websocket.TextMessage {
					m.toClient = append(m.toClient, b)
				}
				_ = ph.WriteMessage(mt, b)
				m.mu.Unlock()
			}
		}()
		for { // phone -> real hub
			mt, b, err := ph.ReadMessage()
			if err != nil {
				return
			}
			m.mu.Lock()
			if mt == websocket.TextMessage {
				m.fromClient = append(m.fromClient, string(b))
			}
			tamper := m.tamperUp
			m.mu.Unlock()
			if tamper != nil {
				if b = tamper(mt, b); b == nil {
					continue // dropped
				}
			}
			_ = hb.WriteMessage(mt, b)
		}
	}))
	return m
}

// A hub that splices replies, keeping A's metadata and B's JPEG (dropping A's JPEG and B's
// metadata), must not get B's image accepted as A's: the capture fails instead.
func TestHubSplicedFrameRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wsURL := startHub(t, ctx)
	phone := host.NewPhone("test")
	proxy := newMITM(wsURL)
	defer proxy.srv.Close()
	agent, _ := pair(t, ctx, phone, wsURL, serve(t, ctx, phone, proxy.url()))
	defer agent.Close()
	n := 0
	proxy.mu.Lock()
	proxy.tamperUp = func(mt int, b []byte) []byte {
		n++ // phone output in order: A meta, A jpeg, B meta, B jpeg
		if n == 2 || n == 3 {
			return nil
		}
		return b
	}
	proxy.mu.Unlock()
	errs := make(chan error, 2)
	for _, q := range []int{40, 90} {
		go func(q int) { _, err := agent.Capture(ctx, q, 320); errs <- err }(q)
		time.Sleep(100 * time.Millisecond) // keep A before B
	}
	for range 2 {
		if err := <-errs; err == nil {
			t.Fatal("a capture succeeded although the hub spliced the replies")
		}
	}
}

func (m *mitm) url() string { return "ws" + strings.TrimPrefix(m.srv.URL, "http") + protocol.Path }

// inject writes to the client as if its peer sent it.
func (m *mitm) inject(b []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.client.WriteMessage(websocket.TextMessage, b)
}

func (m *mitm) clientSent() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.fromClient...)
}

// lastSealedToClient returns the latest sealed request the agent sent through the hub.
func (m *mitm) lastSealedToClient() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.toClient) - 1; i >= 0; i-- {
		if bytes.Contains(m.toClient[i], []byte(`"type":"sealed"`)) {
			return m.toClient[i]
		}
	}
	return nil
}

// A hub that relayed one genuine handshake cannot then drive the camera: injected unsealed
// requests, replayed sealed ones and tampered ones get no camera action, and the real agent's
// session keeps working with the state it set.
func TestRelayThenInject(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wsURL := startHub(t, ctx)
	phone := host.NewPhone("test")
	proxy := newMITM(wsURL)
	defer proxy.srv.Close()
	agent, _ := pair(t, ctx, phone, wsURL, serve(t, ctx, phone, proxy.url()))
	defer agent.Close()
	if _, err := agent.Capture(ctx, 60, 320); err != nil {
		t.Fatalf("genuine capture through the relay: %v", err)
	}
	replay := proxy.lastSealedToClient()
	if replay == nil {
		t.Fatal("no sealed request seen")
	}
	before := len(proxy.clientSent())

	on := true
	evil := []protocol.Message{
		{ID: "inj-capture", Type: protocol.TypeCapture},
		{ID: "inj-torch", Type: protocol.TypeCommand, TorchOn: &on},
	}
	for _, e := range evil {
		b, _ := protocol.Encode(e)
		proxy.inject(b)
	}
	proxy.inject(replay)
	tampered := bytes.Replace(replay, []byte(`"box":"`), []byte(`"box":"A`), 1)
	proxy.inject(tampered)
	time.Sleep(500 * time.Millisecond)

	for _, s := range proxy.clientSent()[before:] {
		if strings.Contains(s, `"type":"sealed"`) || strings.Contains(s, `"type":"frame"`) {
			t.Fatalf("phone acted on injected traffic: %s", s)
		}
	}
	st, err := agent.Command(ctx, protocol.Message{})
	if err != nil {
		t.Fatalf("real agent after injection: %v", err)
	}
	if st.Meta.Torch {
		t.Fatal("injected torch command took effect")
	}
}

// A hub that alters a frame on its way to the agent is caught: the capture fails rather than
// returning the hub's image.
func TestHubForgedFrameRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wsURL := startHub(t, ctx)
	phone := host.NewPhone("test")
	proxy := newMITM(wsURL)
	defer proxy.srv.Close()
	agent, _ := pair(t, ctx, phone, wsURL, serve(t, ctx, phone, proxy.url()))
	defer agent.Close()
	proxy.mu.Lock()
	proxy.tamperUp = func(mt int, b []byte) []byte {
		if mt == websocket.BinaryMessage {
			b = append([]byte(nil), b...)
			b[len(b)/2] ^= 0xff
		}
		return b
	}
	proxy.mu.Unlock()
	if _, err := agent.Capture(ctx, 60, 320); err == nil {
		t.Fatal("agent accepted a frame the hub altered")
	}
}

// A hub on the agent's path records authok, forges "unauthenticated" for a pending request, then
// replays authok. The agent must not rebuild the old channels (that would reuse AES-GCM nonces):
// no two sealed boxes it sends may share a sequence number under one session's keys.
func TestReplayedAuthOKCannotResetChannels(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wsURL := startHub(t, ctx)
	phone := host.NewPhone("test")
	s, key := pair(t, ctx, phone, wsURL, serve(t, ctx, phone, wsURL))
	s.Close()

	proxy := newMITM(wsURL)
	defer proxy.srv.Close()
	agent, err := linked(ctx, proxy.url(), phone.Device, key)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	if _, err := agent.Capture(ctx, 60, 320); err != nil {
		t.Fatal(err)
	}
	var authok []byte
	proxy.mu.Lock()
	for _, b := range proxy.toClient {
		if bytes.Contains(b, []byte(`"type":"authok"`)) {
			authok = b
		}
	}
	attacked := false
	proxy.tamperUp = func(mt int, b []byte) []byte {
		if attacked || mt != websocket.TextMessage || !bytes.Contains(b, []byte(`"type":"sealed"`)) {
			return b
		}
		attacked = true
		m, _ := protocol.Decode(b)
		go func() {
			fake, _ := protocol.Encode(protocol.Message{ID: m.ID, Type: protocol.TypeError, Err: protocol.ErrUnauthenticated})
			proxy.inject(fake)
			time.Sleep(50 * time.Millisecond)
			proxy.inject(authok)
		}()
		return nil // the real phone never sees this request
	}
	proxy.mu.Unlock()
	if authok == nil {
		t.Fatal("no authok recorded")
	}
	sctx, scancel := context.WithTimeout(ctx, 2*time.Second)
	_, _ = agent.Capture(sctx, 60, 320)
	scancel()

	seen := map[string]bool{}
	for _, s := range proxy.clientSent() {
		m, err := protocol.Decode([]byte(s))
		if err != nil || m.Type != protocol.TypeSealed {
			continue
		}
		box, _ := base64.StdEncoding.DecodeString(m.Box)
		seq := string(box[:8])
		if seen[seq] {
			t.Fatalf("agent reused sealed sequence %x: replayed authok rebuilt its channels", box[:8])
		}
		seen[seq] = true
	}
}

func TestLensAndManualControlsSealedRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	url := startHub(t, ctx)
	phone := host.NewPhone("controls-test")
	sess, _ := pair(t, ctx, phone, url, serve(t, ctx, phone, url))
	defer sess.Close()
	if _, err := sess.Command(ctx, protocol.Message{Camera: "fake-front"}); err != nil {
		t.Fatal(err)
	}
	zero, shutter, iso := 0.0, 1.0/125, 100.0
	control := &protocol.Controls{FocusMode: "manual", FocusPosition: &zero, ExposureMode: "manual", ExposureTime: &shutter, ISO: &iso, WhiteBalanceMode: "manual", WhiteBalanceGains: &protocol.Gains{Red: 2, Green: 1, Blue: 1.5}}
	got, err := sess.Command(ctx, protocol.Message{Controls: control})
	if err != nil {
		t.Fatal(err)
	}
	if got.Meta.CameraID != "fake-front" || got.Meta.Facing != "front" || got.Meta.Settings["focusPosition"] != float64(0) || got.Meta.Settings["iso"] != iso {
		t.Fatalf("wrong configuration: %+v", got.Meta)
	}
	for _, id := range []string{"fake-front", "front"} {
		got, err = sess.Command(ctx, protocol.Message{Camera: id})
		if err != nil || got.Meta.Settings["iso"] != iso || got.Meta.Settings["focusPosition"] != zero {
			t.Fatalf("reselecting %s reset controls: %+v %v", id, got.Meta.Settings, err)
		}
	}
	listed, err := sess.Cameras(ctx)
	if err != nil || listed.Meta.Capabilities["focusPoint"] != true || len(listed.Meta.Cameras[0].Capabilities) == 0 {
		t.Fatalf("fake controls not advertised: %+v %v", listed.Meta, err)
	}
	fr, err := sess.Capture(ctx, 70, 320)
	if err != nil || fr.Meta.CameraID != "fake-front" || fr.Meta.Settings["exposureTime"] != shutter {
		t.Fatalf("capture metadata: %+v %v", fr.Meta, err)
	}
	got, err = sess.Command(ctx, protocol.Message{Controls: &protocol.Controls{FocusMode: "auto"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, stale := got.Meta.Settings["focusPosition"]; stale || got.Meta.Settings["iso"] != iso {
		t.Fatalf("reset changed wrong groups: %+v", got.Meta.Settings)
	}
	if _, err = sess.Command(ctx, protocol.Message{Camera: "nonexistent"}); err == nil {
		t.Fatal("unknown lens accepted")
	}
	if _, err = sess.Command(ctx, protocol.Message{Controls: &protocol.Controls{ExposureMode: "manual", ISO: &iso}}); err == nil {
		t.Fatal("incomplete exposure accepted")
	}
	if _, err = sess.Command(ctx, protocol.Message{Controls: &protocol.Controls{ExposureCompensation: &zero}}); err == nil {
		t.Fatal("EV-only patch accepted while manual exposure was active")
	}
	got, err = sess.Cameras(ctx)
	if err != nil || got.Meta.Settings["iso"] != iso || got.Meta.Settings["exposureCompensation"] != nil {
		t.Fatalf("rejected EV patch changed settings: %+v %v", got.Meta.Settings, err)
	}
	got, err = sess.Command(ctx, protocol.Message{Controls: &protocol.Controls{ExposureMode: "auto", ExposureCompensation: &zero}})
	if err != nil || got.Meta.Settings["exposureMode"] != "auto" || got.Meta.Settings["exposureCompensation"] != zero || got.Meta.Settings["iso"] != nil {
		t.Fatalf("explicit auto exposure did not reset manual settings: %+v %v", got.Meta.Settings, err)
	}
}
