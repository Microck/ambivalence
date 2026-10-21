package host

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"math"
	"os"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"

	"github.com/Microck/ambivalence/internal/audio"
	"github.com/Microck/ambivalence/internal/link"
	"github.com/Microck/ambivalence/internal/peer"
	"github.com/Microck/ambivalence/internal/protocol"
)

// Phone is the identity a fake host keeps across hub connections, like an app's storage.
// Key is nil until the first pairing; Confirm stands in for the user's tap on the pairing screen.
type Phone struct {
	Device  string
	Name    string
	Confirm func(sas string) bool

	mu    sync.Mutex
	key   []byte
	state string // optional file persisting Device and key
}

func NewPhone(name string) *Phone {
	return &Phone{Device: randomHex(16), Name: name, Confirm: func(string) bool { return true }}
}

func (p *Phone) Key() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.key
}

func (p *Phone) setKey(k []byte) {
	p.mu.Lock()
	p.key = k
	path := p.state
	p.mu.Unlock()
	if path != "" {
		b, _ := json.Marshal(phoneState{Device: p.Device, Key: link.Hex(k)})
		_ = os.WriteFile(path, b, 0o600)
	}
}

type phoneState struct {
	Device string `json:"device"`
	Key    string `json:"key,omitempty"`
}

// fakeAudio is a live recording session: samples exist from the moment it started, and reads
// consume them in order so an agent that polls slowly still receives every second.
type fakeAudio struct {
	id      string
	rate    int
	started time.Time
	maxMs   int
	read    int64 // samples handed to the agent
	ended   bool
}

func (a *fakeAudio) available() (int64, bool) {
	elapsed := time.Since(a.started).Milliseconds()
	ended := false
	if a.maxMs > 0 && elapsed >= int64(a.maxMs) {
		elapsed = int64(a.maxMs)
		ended = true
	}
	return elapsed * int64(a.rate) / 1000, ended
}

// take returns at most maxMs of contiguous audio after the previous read. totalMs is the
// session's recorded length so far.
func (a *fakeAudio) take(maxMs int) (pcm []byte, totalMs int, ended bool) {
	avail, ended := a.available()
	want := int64(maxMs) * int64(a.rate) / 1000
	n := avail - a.read
	if n < 0 {
		n = 0
	}
	if n > want {
		n = want
	}
	pcm = fakeTonePCM(a.read, n, a.rate)
	a.read += n
	if ended && a.read >= avail {
		a.ended = true
	}
	return pcm, int(avail * 1000 / int64(a.rate)), a.ended
}

// fakeTonePCM is a deterministic two-tone test signal so agents and tests hear something
// non-silent. It is computed from the absolute sample index, which keeps chunk joins seamless.
func fakeTonePCM(first, count int64, rate int) []byte {
	pcm := make([]byte, count*2)
	for i := int64(0); i < count; i++ {
		n := first + i
		t := float64(n) / float64(rate)
		freq := 440.0
		if int(t)%2 == 1 {
			freq = 660
		}
		amp := 0.0
		if math.Mod(t, 1) < 0.6 {
			amp = 0.25
		}
		v := int16(amp * 32767 * math.Sin(2*math.Pi*freq*t))
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(v))
	}
	return pcm
}

// LoadPhone restores a fake phone from path (or makes a new one there). An empty path keeps
// everything in memory.
func LoadPhone(path, name string) (*Phone, error) {
	p := NewPhone(name)
	p.state = path
	if path == "" {
		return p, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		b, _ = json.Marshal(phoneState{Device: p.Device})
		return p, os.WriteFile(path, b, 0o600)
	}
	if err != nil {
		return nil, err
	}
	var st phoneState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	p.Device = st.Device
	if st.Key != "" {
		if p.key, err = link.Unhex(st.Key); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// Serve joins one hub and answers the agent there with a labeled test chart. A phone may serve
// several hubs at once; the hub authorises nothing, every agent is checked here (package link).
func (p *Phone) Serve(ctx context.Context, hubURL string, ready func(code string)) error {
	sess, err := peer.Dial(ctx, peer.DialOptions{
		Role:   protocol.RoleHost,
		Device: p.Device,
		HubURL: hubURL,
		Host:   &protocol.HostInfo{Name: p.Name, Platform: "fake"},
	})
	if err != nil {
		return err
	}
	defer sess.Close()
	if ready != nil {
		ready(sess.Code())
	}

	torch := false
	zoom := 1.0
	facing := "back"
	cameraID := "fake-back"
	settings := map[string]any{}
	// The test host records control state without sensor limits. These ranges
	// describe the finite values accepted by Controls.Validate.
	positive := map[string]any{"min": math.SmallestNonzeroFloat64, "max": math.MaxFloat64}
	capabilities := map[string]any{
		"controlsVersion":         1,
		"focusMode":               []string{"auto", "manual"},
		"focusPosition":           map[string]any{"min": 0, "max": 1},
		"focusPoint":              true,
		"exposureMode":            []string{"auto", "manual"},
		"exposureTime":            positive,
		"iso":                     positive,
		"exposureCompensation":    map[string]any{"min": -math.MaxFloat64, "max": math.MaxFloat64},
		"whiteBalanceMode":        []string{"auto", "manual"},
		"whiteBalanceTemperature": positive,
		"whiteBalanceGains":       map[string]any{"min": 1, "max": math.MaxFloat64},
		"audioVersion":            1,
		"audio":                   protocol.AudioCapabilities(),
	}
	cams := []protocol.Camera{{
		ID: "fake-back", Facing: "back", Torch: true, MinZoom: 1, MaxZoom: 8, Capabilities: capabilities,
	}, {
		ID: "fake-front", Facing: "front", Torch: false, MinZoom: 1, MaxZoom: 2, Capabilities: capabilities,
	}}

	// Per-agent state: reset whenever an agent arrives or leaves. seal is set once the agent has
	// proven the link; only messages sealed under it may touch the camera.
	var (
		seal      *peer.Seal
		challenge []byte
		deadline  <-chan time.Time // agent must authenticate or start pairing before this
		pairing   *link.Party
		pairNB    []byte
		pairCmt   []byte
		listen    *fakeAudio
	)
	agentHere := false
	// newAgent runs when an agent arrives or leaves. The hub repeats status (welcome, then a
	// broadcast), so act only on a change; a second challenge would void the first.
	newAgent := func(present bool) {
		if present == agentHere && present {
			return
		}
		agentHere = present
		seal, challenge, pairing, deadline, listen = nil, nil, nil, nil, nil
		if !present {
			return
		}
		deadline = time.After(15 * time.Second)
		if key := p.Key(); key != nil {
			challenge = link.Nonce()
			_ = sess.Send(protocol.Message{Type: protocol.TypeAuth, Nonce: link.Hex(challenge)})
		}
	}
	reject := func() {
		_ = sess.Send(protocol.Message{Type: protocol.TypeReject})
		// The hub frees the slot at once, so the next agent's status is a real arrival.
		agentHere, seal, challenge, pairing, deadline, listen = false, nil, nil, nil, nil, nil
	}
	newAgent(sess.Status().Connected)

	// camera serves one opened request; every reply goes back sealed.
	camera := func(m protocol.Message) {
		reply := func(r protocol.Message, jpeg []byte) { r.ID = m.ID; _ = sess.SendSealed(seal, r, jpeg) }
		switch m.Type {
		case protocol.TypeCameras:
			reply(protocol.Message{Type: protocol.TypeCameras, Cameras: cams, Facing: facing, CameraID: cameraID, Settings: settings, Capabilities: capabilities, Torch: torch, Zoom: zoom,
				Host: &protocol.HostInfo{Name: p.Name, Platform: "fake"}}, nil)
		case protocol.TypeCommand:
			if m.TorchOn != nil {
				torch = *m.TorchOn
			}
			if m.ZoomLevel != nil {
				zoom = *m.ZoomLevel
			}
			if m.Camera != "" {
				found := false
				for _, cam := range cams {
					if m.Camera == cam.ID || m.Camera == cam.Facing {
						if cameraID != cam.ID {
							settings = map[string]any{}
						}
						facing = cam.Facing
						cameraID = cam.ID
						found = true
						break
					}
				}
				if !found {
					reply(protocol.Message{Type: protocol.TypeError, Err: "unknown camera"}, nil)
					return
				}
			}
			if m.Controls != nil {
				if err := m.Controls.Validate(); err != nil {
					reply(protocol.Message{Type: protocol.TypeError, Err: err.Error()}, nil)
					return
				}
				// An EV-only patch retains the current mode, which may be manual.
				if m.Controls.ExposureCompensation != nil && m.Controls.ExposureMode == "" && settings["exposureMode"] == "manual" {
					reply(protocol.Message{Type: protocol.TypeError, Err: "EV bias requires auto exposure"}, nil)
					return
				}
				b, _ := json.Marshal(m.Controls)
				var patch map[string]any
				_ = json.Unmarshal(b, &patch)
				for mode, keys := range map[string][]string{
					"focusMode":        {"focusMode", "focusPosition", "focusPoint"},
					"exposureMode":     {"exposureMode", "exposureTime", "iso", "exposureCompensation"},
					"whiteBalanceMode": {"whiteBalanceMode", "whiteBalanceGains", "whiteBalanceTemperature"},
				} {
					if _, ok := patch[mode]; ok {
						for _, key := range keys {
							delete(settings, key)
						}
					}
				}
				for key, value := range patch {
					settings[key] = value
				}
			}
			reply(protocol.Message{Type: protocol.TypeStatus, Facing: facing, CameraID: cameraID, Settings: settings, Capabilities: capabilities, Torch: torch, Zoom: zoom, Connected: true}, nil)
		case protocol.TypeCapture:
			jpegBytes, width, height, err := RenderChart(m.Quality, m.MaxWidth, facing, torch, zoom, sess.Code())
			if err != nil {
				reply(protocol.Message{Type: protocol.TypeError, Err: err.Error()}, nil)
				return
			}
			reply(protocol.Message{Facing: facing, CameraID: cameraID, Settings: settings, Capabilities: capabilities, Torch: torch, Zoom: zoom, Width: width, Height: height}, jpegBytes)
		case protocol.TypeListen:
			duration, rate, err := protocol.NormalizeAudio(m.DurationMs, m.SampleRate, protocol.DefaultAudioMs, protocol.MaxOneShotAudioMs)
			if err != nil {
				reply(protocol.Message{Type: protocol.TypeError, Err: err.Error()}, nil)
				return
			}
			pcm := fakeTonePCM(0, int64(duration*rate/1000), rate)
			level := audio.Level(pcm)
			reply(protocol.Message{Mime: protocol.AudioMime, DurationMs: duration, SampleRate: rate, Levels: &level}, audio.EncodeWAV(pcm, rate))
		case protocol.TypeAudioStart:
			rate := m.SampleRate
			if rate == 0 {
				rate = protocol.DefaultAudioSampleRate
			}
			if rate < protocol.MinAudioSampleRate || rate > protocol.MaxAudioSampleRate {
				reply(protocol.Message{Type: protocol.TypeError, Err: "unsupported sample rate"}, nil)
				return
			}
			if m.MaxDurationMs < 0 || m.MaxDurationMs > protocol.MaxSessionMs {
				reply(protocol.Message{Type: protocol.TypeError, Err: "invalid max duration"}, nil)
				return
			}
			listen = &fakeAudio{id: randomHex(8), rate: rate, started: time.Now(), maxMs: m.MaxDurationMs}
			reply(protocol.Message{Type: protocol.TypeAudioStarted, ListenID: listen.id, SampleRate: rate, MaxDurationMs: m.MaxDurationMs}, nil)
		case protocol.TypeAudioRead:
			if listen == nil || m.ListenID != listen.id {
				reply(protocol.Message{Type: protocol.TypeError, Err: "no such audio session"}, nil)
				return
			}
			want := int64(m.DurationMs) * int64(listen.rate) / 1000
			deadline := time.Now().Add(time.Duration(m.DurationMs+5000) * time.Millisecond)
			for {
				avail, ended := listen.available()
				if avail-listen.read >= want || ended || time.Now().After(deadline) {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			pcm, totalMs, ended := listen.take(m.DurationMs)
			level := audio.Level(pcm)
			reply(protocol.Message{Mime: protocol.AudioMime, DurationMs: len(pcm) * 1000 / (listen.rate * 2), SampleRate: listen.rate, TotalMs: totalMs, Ended: ended, Levels: &level}, audio.EncodeWAV(pcm, listen.rate))
		case protocol.TypeAudioStop:
			if listen == nil || m.ListenID != listen.id {
				reply(protocol.Message{Type: protocol.TypeError, Err: "no such audio session"}, nil)
				return
			}
			avail, _ := listen.available()
			totalMs := int(avail * 1000 / int64(listen.rate))
			listen = nil
			reply(protocol.Message{Type: protocol.TypeStatus, DurationMs: totalMs, TotalMs: totalMs, Ended: true}, nil)
		case protocol.TypeLevel:
			duration, rate, err := protocol.NormalizeAudio(m.DurationMs, m.SampleRate, 1000, protocol.MaxChunkMs)
			if err != nil {
				reply(protocol.Message{Type: protocol.TypeError, Err: err.Error()}, nil)
				return
			}
			pcm := fakeTonePCM(0, int64(duration*rate/1000), rate)
			level := audio.Level(pcm)
			reply(protocol.Message{Type: protocol.TypeLevels, DurationMs: duration, SampleRate: rate, Levels: &level}, nil)
		default:
			reply(protocol.Message{Type: protocol.TypeError, Err: "unknown request " + string(m.Type)}, nil)
		}
	}

	reqs := sess.HostRequests()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			if seal == nil && pairing == nil {
				reject() // silent squatter
			}
			deadline = nil
		case fr, ok := <-reqs:
			if !ok {
				return fmt.Errorf("host session closed")
			}
			m := fr.Meta
			switch m.Type {
			case protocol.TypePing:
				_ = sess.Send(protocol.Message{ID: m.ID, Type: protocol.TypePong})
			case protocol.TypeStatus:
				newAgent(m.Connected)
			case protocol.TypeAuth:
				nP, _ := link.Unhex(m.Nonce)
				nA, _ := link.Unhex(m.AgentNonce)
				mac, _ := link.Unhex(m.MAC)
				key := p.Key()
				if key == nil || challenge == nil || len(nA) != 16 || !link.Equal(nP, challenge) ||
					!link.Equal(mac, link.MAC(key, "agent", []byte(p.Device), nP, nA)) {
					reject()
					continue
				}
				seal, challenge = peer.NewSeal(key, p.Device, nP, nA, false), nil
				_ = sess.Send(protocol.Message{Type: protocol.TypeAuthOK, Nonce: m.Nonce, MAC: link.Hex(link.MAC(key, "phone", []byte(p.Device), nP, nA))})
			case protocol.TypePair1:
				pairCmt, _ = link.Unhex(m.Commit)
				if pairing, err = link.NewParty(); err != nil {
					_ = sess.ReplyError(m.ID, err.Error())
					continue
				}
				pairNB = link.Nonce()
				_ = sess.Send(protocol.Message{ID: m.ID, Type: protocol.TypePair2, Pub: link.Hex(pairing.Pub), Nonce: link.Hex(pairNB), Device: p.Device})
			case protocol.TypePair3:
				agentPub, _ := link.Unhex(m.Pub)
				nA, _ := link.Unhex(m.Nonce)
				if pairing == nil || len(nA) != 16 || !link.Equal(pairCmt, link.H("commit", agentPub, nA)) {
					_ = sess.ReplyError(m.ID, "pairing commitment mismatch")
					reject()
					continue
				}
				shared, err := pairing.Shared(agentPub)
				if err != nil || !p.Confirm(link.SAS(agentPub, pairing.Pub, nA, pairNB)) {
					_ = sess.ReplyError(m.ID, "pairing cancelled on the phone")
					reject()
					continue
				}
				key := link.Key(shared, agentPub, pairing.Pub)
				p.setKey(key)
				seal, pairing = peer.NewSeal(key, p.Device, pairNB, nA, false), nil
				_ = sess.Send(protocol.Message{ID: m.ID, Type: protocol.TypePair4, MAC: link.Hex(link.MAC(key, "confirm", []byte(p.Device)))})
			case protocol.TypeSealed:
				if seal == nil { // phone just (re)connected: make the agent re-authenticate
					_ = sess.ReplyError(m.ID, protocol.ErrUnauthenticated)
					continue
				}
				inner, err := seal.Open(m)
				if err != nil || inner.ID != m.ID {
					continue // forged or replayed by the hub: ignore it, keep serving the real agent
				}
				camera(inner)
			case protocol.TypeCameras, protocol.TypeCommand, protocol.TypeCapture:
				// Unsealed camera requests never count, even after auth: a hub could inject them.
				_ = sess.ReplyError(m.ID, protocol.ErrUnauthenticated)
			}
		}
	}
}

func RenderChart(quality, maxWidth int, facing string, torch bool, zoom float64, code string) ([]byte, int, int, error) {
	if quality <= 0 {
		quality = protocol.DefaultQuality
	}
	if maxWidth <= 0 {
		maxWidth = protocol.DefaultMaxWidth
	}
	if maxWidth < 320 {
		maxWidth = 320
	}
	w := maxWidth
	h := maxWidth * 9 / 16
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{0, 0, 0, 255}}, image.Point{}, draw.Src)

	bars := []color.RGBA{
		{200, 0, 0, 255}, {0, 180, 0, 255}, {0, 80, 220, 255},
		{230, 200, 0, 255}, {180, 180, 180, 255}, {40, 40, 40, 255},
	}
	bw := w / len(bars)
	for i, c := range bars {
		r := image.Rect(i*bw, 0, (i+1)*bw, h/6)
		draw.Draw(img, r, &image.Uniform{C: c}, image.Point{}, draw.Src)
	}
	// grid
	grid := color.RGBA{40, 40, 40, 255}
	for x := 0; x < w; x += 40 {
		draw.Draw(img, image.Rect(x, 0, x+1, h), &image.Uniform{C: grid}, image.Point{}, draw.Src)
	}
	for y := 0; y < h; y += 40 {
		draw.Draw(img, image.Rect(0, y, w, y+1), &image.Uniform{C: grid}, image.Point{}, draw.Src)
	}
	if torch {
		draw.Draw(img, image.Rect(w-80, 12, w-12, 80), &image.Uniform{C: color.RGBA{255, 240, 80, 255}}, image.Point{}, draw.Src)
	}
	lines := []string{
		"AMBIVALENCE",
		fmt.Sprintf("code %s", code),
		fmt.Sprintf("%s  zoom %.1f  %s", facing, zoom, time.Now().UTC().Format("15:04:05")),
		fmt.Sprintf("%dx%d", w, h),
	}
	col := color.RGBA{255, 255, 255, 255}
	y := h/2 - 20
	for _, line := range lines {
		drawString(img, 24, y, line, col)
		y += 18
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, 0, 0, err
	}
	return buf.Bytes(), w, h, nil
}

func drawString(img *image.RGBA, x, y int, s string, col color.RGBA) {
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(col),
		Face: basicfont.Face7x13,
		Dot:  fixed.P(x, y),
	}
	d.DrawString(s)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing means the OS is broken; nothing sane to fall back to
	}
	return hex.EncodeToString(b)
}
