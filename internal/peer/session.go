package peer

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Microck/ambivalence/internal/link"
	"github.com/Microck/ambivalence/internal/protocol"
)

type Frame struct {
	Meta protocol.Message
	Data []byte
}

type Session struct {
	ws   *websocket.Conn
	mu   sync.Mutex
	code string

	pending map[string]chan Frame
	pendMu  sync.Mutex
	hostCh  chan Frame

	status protocol.Message
	statMu sync.Mutex

	// Agent side of the end-to-end link (package link). authCh is closed once the phone on
	// the other end has proven the shared key; a fresh challenge (phone reconnected) resets it.
	authMu  sync.Mutex
	device  string
	key     []byte
	authCh  chan struct{}
	authed  bool
	authNP  []byte // phone challenge being answered
	authNA  []byte // our nonce for it
	seal    *Seal  // set while authed: all camera traffic goes through it
	authGen int    // bumped on each successful auth

	closed    chan struct{}
	closeOnce sync.Once
	err       error
	errMu     sync.Mutex
}

type DialOptions struct {
	Role    protocol.Role
	Code    string
	Device  string // host: own permanent id; agent: linked phone to join instead of Code
	Key     []byte // agent: link key from pairing, used to answer the phone's challenge
	HubURL  string
	Host    *protocol.HostInfo
	Timeout time.Duration
}

var (
	// ErrPhoneOffline: the linked phone is not connected to this hub right now.
	ErrPhoneOffline = errors.New(protocol.ErrOffline)
	// ErrPhoneBusy: another agent connection holds the phone's slot; retry.
	ErrPhoneBusy = errors.New(protocol.ErrBusy)
)

func Dial(ctx context.Context, opt DialOptions) (*Session, error) {
	if opt.Role == "" {
		return nil, fmt.Errorf("role required")
	}
	if opt.HubURL == "" {
		return nil, fmt.Errorf("hub url required")
	}
	timeout := opt.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: timeout,
	}
	ws, _, err := dialer.DialContext(ctx, opt.HubURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial hub: %w", err)
	}
	s := &Session{
		ws:      ws,
		code:    opt.Code,
		pending: make(map[string]chan Frame),
		closed:  make(chan struct{}),
		device:  opt.Device,
		key:     opt.Key,
		authCh:  make(chan struct{}),
	}
	if opt.Role == protocol.RoleHost {
		s.hostCh = make(chan Frame, 16)
	}
	hello := protocol.Message{
		V:      protocol.Version,
		Type:   protocol.TypeHello,
		Role:   opt.Role,
		Code:   opt.Code,
		Device: opt.Device,
		Host:   opt.Host,
	}
	if err := s.send(hello); err != nil {
		ws.Close()
		return nil, err
	}
	ws.SetReadDeadline(time.Now().Add(timeout))
	mt, data, err := ws.ReadMessage()
	if err != nil {
		ws.Close()
		return nil, fmt.Errorf("hello: %w", err)
	}
	if mt != websocket.TextMessage {
		ws.Close()
		return nil, fmt.Errorf("hello: expected text welcome")
	}
	msg, err := protocol.Decode(data)
	if err != nil {
		ws.Close()
		return nil, err
	}
	if msg.Type == protocol.TypeError {
		ws.Close()
		switch msg.Err {
		case protocol.ErrOffline:
			return nil, ErrPhoneOffline
		case protocol.ErrBusy:
			return nil, ErrPhoneBusy
		}
		return nil, fmt.Errorf("hub: %s", msg.Err)
	}
	if msg.Type != protocol.TypeWelcome {
		ws.Close()
		return nil, fmt.Errorf("hello: unexpected %s", msg.Type)
	}
	s.code = msg.Code
	s.status = msg
	ws.SetReadDeadline(time.Time{})
	go s.readLoop()
	return s, nil

}

func (s *Session) Code() string { return s.code }

func (s *Session) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	_ = s.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(2*time.Second))
	return s.ws.Close()
}

// Alive reports whether the hub connection is still open.
func (s *Session) Alive() bool {
	select {
	case <-s.closed:
		return false
	default:
		return true
	}
}

func (s *Session) lastErr() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *Session) Status() protocol.Message {
	s.statMu.Lock()
	defer s.statMu.Unlock()
	return s.status
}

func (s *Session) Capture(ctx context.Context, quality, maxWidth int) (Frame, error) {
	if quality <= 0 {
		quality = protocol.DefaultQuality
	}
	if maxWidth <= 0 {
		maxWidth = protocol.DefaultMaxWidth
	}
	return s.call(ctx, protocol.Message{
		Type:     protocol.TypeCapture,
		Quality:  quality,
		MaxWidth: maxWidth,
	})
}

// Listen records one clip and returns it as a WAV file. For listening longer than
// MaxOneShotAudioMs, use the session methods below and read chunks as they arrive.
func (s *Session) Listen(ctx context.Context, durationMs, sampleRate int) (Frame, error) {
	durationMs, sampleRate, err := protocol.NormalizeAudio(durationMs, sampleRate, protocol.DefaultAudioMs, protocol.MaxOneShotAudioMs)
	if err != nil {
		return Frame{}, err
	}
	if err := s.requireAudio(ctx); err != nil {
		return Frame{}, err
	}
	return s.call(ctx, protocol.Message{Type: protocol.TypeListen, DurationMs: durationMs, SampleRate: sampleRate})
}

// AudioStart begins continuous recording. The phone buffers audio until AudioRead drains it,
// so an agent may take its time between chunks without losing sound.
func (s *Session) AudioStart(ctx context.Context, maxDurationMs, sampleRate int) (protocol.Message, error) {
	if sampleRate == 0 {
		sampleRate = protocol.DefaultAudioSampleRate
	}
	if sampleRate < protocol.MinAudioSampleRate || sampleRate > protocol.MaxAudioSampleRate {
		return protocol.Message{}, fmt.Errorf("sample rate must be between %d and %d Hz", protocol.MinAudioSampleRate, protocol.MaxAudioSampleRate)
	}
	if maxDurationMs < 0 || maxDurationMs > protocol.MaxSessionMs {
		return protocol.Message{}, fmt.Errorf("max duration must be between 0 and %d ms", protocol.MaxSessionMs)
	}
	if err := s.requireAudio(ctx); err != nil {
		return protocol.Message{}, err
	}
	fr, err := s.call(ctx, protocol.Message{Type: protocol.TypeAudioStart, MaxDurationMs: maxDurationMs, SampleRate: sampleRate})
	if err != nil {
		return protocol.Message{}, err
	}
	if fr.Meta.Type != protocol.TypeAudioStarted || fr.Meta.ListenID == "" {
		return protocol.Message{}, fmt.Errorf("phone did not start an audio session")
	}
	return fr.Meta, nil
}

// AudioRead returns audio recorded since the previous read, as a complete WAV chunk. If no
// new audio is buffered yet it waits up to chunkMs for some to arrive.
func (s *Session) AudioRead(ctx context.Context, listenID string, chunkMs int) (Frame, error) {
	if listenID == "" {
		return Frame{}, fmt.Errorf("listen id required")
	}
	if chunkMs == 0 {
		chunkMs = protocol.DefaultChunkMs
	}
	if chunkMs < 100 || chunkMs > protocol.MaxChunkMs {
		return Frame{}, fmt.Errorf("chunk must be between 100 and %d ms", protocol.MaxChunkMs)
	}
	return s.call(ctx, protocol.Message{Type: protocol.TypeAudioRead, ListenID: listenID, DurationMs: chunkMs})
}

// AudioStop ends a session and returns how much audio it recorded.
func (s *Session) AudioStop(ctx context.Context, listenID string) (protocol.Message, error) {
	if listenID == "" {
		return protocol.Message{}, fmt.Errorf("listen id required")
	}
	fr, err := s.call(ctx, protocol.Message{Type: protocol.TypeAudioStop, ListenID: listenID})
	if err != nil {
		return protocol.Message{}, err
	}
	return fr.Meta, nil
}

// Level measures loudness for durationMs without returning audio.
func (s *Session) Level(ctx context.Context, durationMs int) (Frame, error) {
	durationMs, sampleRate, err := protocol.NormalizeAudio(durationMs, 0, 1000, protocol.MaxChunkMs)
	if err != nil {
		return Frame{}, err
	}
	if err := s.requireAudio(ctx); err != nil {
		return Frame{}, err
	}
	return s.call(ctx, protocol.Message{Type: protocol.TypeLevel, DurationMs: durationMs, SampleRate: sampleRate})
}

// requireAudio asks the phone what it supports before sending audio requests. Older hosts
// ignore unknown fields, so without this an agent would record silence instead of learning
// that the app needs an update.
func (s *Session) requireAudio(ctx context.Context) error {
	info, err := s.Cameras(ctx)
	if err != nil {
		return err
	}
	if info.Meta.Capabilities["audioVersion"] != float64(1) {
		return fmt.Errorf("host does not support audio; update the phone app")
	}
	return nil
}

func (s *Session) Command(ctx context.Context, msg protocol.Message) (Frame, error) {
	if msg.Controls != nil {
		if err := msg.Controls.Validate(); err != nil {
			return Frame{}, err
		}
		// Older hosts silently ignore unknown fields: require capability negotiation.
		info, err := s.Cameras(ctx)
		if err != nil {
			return Frame{}, err
		}
		if info.Meta.Capabilities["controlsVersion"] != float64(1) {
			return Frame{}, fmt.Errorf("host does not support manual controls; update the phone app")
		}
	}
	msg.Type = protocol.TypeCommand
	return s.call(ctx, msg)
}

func (s *Session) Cameras(ctx context.Context) (Frame, error) {
	return s.call(ctx, protocol.Message{Type: protocol.TypeCameras})
}

// call is an RPC to an authenticated phone: it waits until the phone has proven the link and
// retries once if the phone reconnected and had not re-verified this agent yet.
func (s *Session) call(ctx context.Context, msg protocol.Message) (Frame, error) {
	for attempt := 0; ; attempt++ {
		if err := s.WaitAuthed(ctx); err != nil {
			return Frame{}, err
		}
		s.authMu.Lock()
		gen, seal := s.authGen, s.seal
		s.authMu.Unlock()
		if seal == nil { // reset between WaitAuthed and here
			continue
		}
		fr, err := s.rpcWith(ctx, msg, seal, nil)
		if err != nil && err.Error() == protocol.ErrUnauthenticated && attempt == 0 {
			if err := s.waitNewerAuth(ctx, gen); err != nil {
				return Frame{}, err
			}
			continue
		}
		return fr, err
	}
}

// waitNewerAuth waits for an authentication newer than gen. The phone's re-challenge and its
// "unauthenticated" reply can race, so a fresh auth may already have landed: then return at once.
func (s *Session) waitNewerAuth(ctx context.Context, gen int) error {
	s.authMu.Lock()
	if s.authGen > gen {
		s.authMu.Unlock()
		return nil
	}
	if s.authed { // stale: the phone reconnected and has not re-challenged yet
		s.authed, s.seal = false, nil
		s.authCh = make(chan struct{})
	}
	s.authMu.Unlock()
	return s.WaitAuthed(ctx)
}

// WaitAuthed blocks until the phone has proven the link key (or the session/ctx ends).
func (s *Session) WaitAuthed(ctx context.Context) error {
	s.authMu.Lock()
	ch := s.authCh
	s.authMu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("phone did not authenticate: %w", ctx.Err())
	case <-s.closed:
		if err := s.lastErr(); err != nil {
			return fmt.Errorf("phone rejected this agent's link (pair again with the code): %w", err)
		}
		return fmt.Errorf("session closed")
	}
}

func (s *Session) resetAuth() {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.authed {
		s.authed, s.seal = false, nil
		s.authCh = make(chan struct{})
	}
}

func (s *Session) markAuthed(seal *Seal) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if !s.authed {
		s.authed, s.seal = true, seal
		s.authGen++
		close(s.authCh)
	}
}

// Seal is one authenticated session's two sealed channels (package link).
type Seal struct{ tx, rx *link.Channel }

// NewSeal builds the channels for one session; agent picks the side.
func NewSeal(key []byte, device string, nP, nA []byte, agent bool) *Seal {
	toPhone, toAgent := link.Channels(key, device, nP, nA)
	if agent {
		return &Seal{tx: link.NewChannel(toPhone), rx: link.NewChannel(toAgent)}
	}
	return &Seal{tx: link.NewChannel(toAgent), rx: link.NewChannel(toPhone)}
}

// onAuth answers the phone's challenge and verifies its proof. Runs on the read loop.
func (s *Session) onAuth(msg protocol.Message) {
	s.authMu.Lock()
	key, device := s.key, s.device
	s.authMu.Unlock()
	if key == nil {
		return // not linked yet; this session is here to pair
	}
	nP, err := link.Unhex(msg.Nonce)
	if err != nil {
		return
	}
	if msg.Type == protocol.TypeAuth {
		s.resetAuth()
		nA := link.Nonce()
		s.authMu.Lock()
		s.authNP, s.authNA = nP, nA
		s.authMu.Unlock()
		_ = s.send(protocol.Message{Type: protocol.TypeAuth, Nonce: msg.Nonce, AgentNonce: link.Hex(nA),
			MAC: link.Hex(link.MAC(key, "agent", []byte(device), nP, nA))})
		return
	}
	// Consume the pending handshake: each challenge installs channels at most once. Otherwise a
	// hub could replay this authok after forging "unauthenticated" and make us rebuild the same
	// keys with counters at zero (AES-GCM nonce reuse). Only a fresh challenge, answered with a
	// fresh agent nonce, can install new channels.
	s.authMu.Lock()
	wantNP, nA := s.authNP, s.authNA
	s.authNP, s.authNA = nil, nil
	s.authMu.Unlock()
	mac, _ := link.Unhex(msg.MAC)
	if wantNP == nil || !link.Equal(nP, wantNP) || !link.Equal(mac, link.MAC(key, "phone", []byte(device), nP, nA)) {
		s.fail(fmt.Errorf("the phone on this hub could not prove the link"))
		return
	}
	s.markAuthed(NewSeal(key, device, nP, nA, true))
}

// fail closes the session with err as the reason callers see.
func (s *Session) fail(err error) {
	s.errMu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.errMu.Unlock()
	s.Close()
}

// Pair runs first-time pairing over a code session (see package link). show is called with
// the confirmation code once both sides can display it; Pair then waits for the user to tap
// Confirm on the phone. It returns the phone's device id and the link key.
func (s *Session) Pair(ctx context.Context, show func(sas string)) (device string, key []byte, err error) {
	me, err := link.NewParty()
	if err != nil {
		return "", nil, err
	}
	nA := link.Nonce()
	r2, err := s.rpc(ctx, protocol.Message{Type: protocol.TypePair1, Commit: link.Hex(link.H("commit", me.Pub, nA))})
	if err != nil {
		return "", nil, err
	}
	phonePub, err1 := link.Unhex(r2.Meta.Pub)
	nB, err2 := link.Unhex(r2.Meta.Nonce)
	if err1 != nil || err2 != nil || r2.Meta.Device == "" {
		return "", nil, fmt.Errorf("bad pairing reply from phone")
	}
	shared, err := me.Shared(phonePub)
	if err != nil {
		return "", nil, err
	}
	key = link.Key(shared, me.Pub, phonePub)
	r4, err := s.rpcWith(ctx, protocol.Message{Type: protocol.TypePair3, Pub: link.Hex(me.Pub), Nonce: link.Hex(nA)}, nil,
		func() { show(link.SAS(me.Pub, phonePub, nA, nB)) })
	if err != nil {
		return "", nil, err
	}
	mac, _ := link.Unhex(r4.Meta.MAC)
	if !link.Equal(mac, link.MAC(key, "confirm", []byte(r2.Meta.Device))) {
		return "", nil, fmt.Errorf("pairing confirmation did not verify")
	}
	s.authMu.Lock()
	s.device, s.key = r2.Meta.Device, key
	s.authMu.Unlock()
	s.markAuthed(NewSeal(key, r2.Meta.Device, nB, nA, true))
	return r2.Meta.Device, key, nil
}

func (s *Session) Send(msg protocol.Message) error {
	return s.send(msg)
}

// SendSealed seals msg (and payload, sent as the following binary box) under seal. Sealing
// happens under the write lock so sequence order always equals wire order.
func (s *Session) SendSealed(seal *Seal, msg protocol.Message, payload []byte) error {
	msg.V = protocol.Version
	if payload != nil {
		msg.Type = protocol.TypeFrame
		msg.Bytes = len(payload)
		if msg.Mime == "" {
			msg.Mime = "image/jpeg"
		}
	}
	inner, err := protocol.Encode(msg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The outer id is only routing (an "unauthenticated" reply needs it); the inner one is checked.
	if err := s.writeJSON(protocol.Message{Type: protocol.TypeSealed, ID: msg.ID, Box: base64.StdEncoding.EncodeToString(seal.tx.Seal(inner))}); err != nil {
		return err
	}
	if payload == nil {
		return nil
	}
	s.ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return s.ws.WriteMessage(websocket.BinaryMessage, seal.tx.Seal(payload))
}

// Open unseals a sealed message received by a host (hosts never receive binary from agents).
func (seal *Seal) Open(msg protocol.Message) (protocol.Message, error) {
	box, err := base64.StdEncoding.DecodeString(msg.Box)
	if err != nil {
		return protocol.Message{}, err
	}
	inner, err := seal.rx.Open(box)
	if err != nil {
		return protocol.Message{}, err
	}
	return protocol.Decode(inner)
}

func (s *Session) rpc(ctx context.Context, msg protocol.Message) (Frame, error) {
	return s.rpcWith(ctx, msg, nil, nil)
}

// rpcWith sends msg (sealed if seal is set), runs sent (if any) right after, then waits for the
// reply with the same id.
func (s *Session) rpcWith(ctx context.Context, msg protocol.Message, seal *Seal, sent func()) (Frame, error) {
	id, err := newID()
	if err != nil {
		return Frame{}, err
	}
	msg.ID = id
	msg.V = protocol.Version
	ch := make(chan Frame, 1)
	s.pendMu.Lock()
	s.pending[id] = ch
	s.pendMu.Unlock()
	defer func() {
		s.pendMu.Lock()
		delete(s.pending, id)
		s.pendMu.Unlock()
	}()
	if seal != nil {
		err = s.SendSealed(seal, msg, nil)
	} else {
		err = s.send(msg)
	}
	if err != nil {
		return Frame{}, err
	}
	if sent != nil {
		sent()
	}
	select {
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	case <-s.closed:
		if err := s.lastErr(); err != nil {
			return Frame{}, err
		}
		return Frame{}, fmt.Errorf("session closed")
	case fr := <-ch:
		if fr.Meta.Type == protocol.TypeError {
			return Frame{}, fmt.Errorf("%s", fr.Meta.Err)
		}
		return fr, nil
	}
}

func (s *Session) send(msg protocol.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeJSON(msg)
}

func (s *Session) writeJSON(msg protocol.Message) error {
	msg.V = protocol.Version
	b, err := protocol.Encode(msg)
	if err != nil {
		return err
	}
	s.ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return s.ws.WriteMessage(websocket.TextMessage, b)
}

// openSealed opens a sealed message on the read loop; a sealed frame's payload is the next
// binary box. Image and audio payloads have different caps.
func (s *Session) openSealed(msg protocol.Message) (protocol.Message, []byte, error) {
	s.authMu.Lock()
	seal := s.seal
	s.authMu.Unlock()
	if seal == nil {
		return protocol.Message{}, nil, fmt.Errorf("sealed message before authentication")
	}
	inner, err := seal.Open(msg)
	if err != nil || inner.Type != protocol.TypeFrame {
		return inner, nil, err
	}
	limit := protocol.MaxJPEGBytes
	if inner.Mime == protocol.AudioMime {
		limit = protocol.MaxAudioBytes
	}
	s.ws.SetReadDeadline(time.Now().Add(60 * time.Second))
	mt, box, err := s.ws.ReadMessage()
	s.ws.SetReadDeadline(time.Time{})
	if err != nil {
		return inner, nil, err
	}
	if mt != websocket.BinaryMessage || len(box) > limit+64 {
		return inner, nil, fmt.Errorf("bad frame payload")
	}
	payload, err := seal.rx.Open(box)
	return inner, payload, err
}

func (s *Session) readLoop() {
	defer func() {
		s.closeOnce.Do(func() { close(s.closed) })
		if s.hostCh != nil {
			close(s.hostCh)
		}
		s.ws.Close()
	}()
	for {
		mt, data, err := s.ws.ReadMessage()
		if err != nil {
			s.errMu.Lock()
			if s.err == nil { // keep the first, most specific reason
				s.err = err
			}
			s.errMu.Unlock()
			return
		}
		if mt != websocket.TextMessage {
			continue
		}
		msg, err := protocol.Decode(data)
		if err != nil {
			continue
		}
		var payload []byte
		if s.hostCh == nil {
			// Agent: camera traffic counts only if sealed by the authenticated phone. Unsealed
			// messages are hub status, auth, pairing (verified by MAC) or errors (DoS at worst).
			if msg.Type == protocol.TypeSealed {
				outer := msg.ID
				if msg, payload, err = s.openSealed(msg); err == nil && msg.ID != outer {
					err = fmt.Errorf("sealed id mismatch")
				}
				if err != nil {
					s.fail(fmt.Errorf("hub altered traffic from the phone: %w", err))
					return
				}
			} else if msg.ID != "" && msg.Type != protocol.TypeError && msg.Type != protocol.TypePair2 && msg.Type != protocol.TypePair4 {
				continue
			} else if msg.Type == protocol.TypeFrame {
				continue
			}
		}
		if msg.Type == protocol.TypeStatus || msg.Type == protocol.TypeWelcome {
			s.statMu.Lock()
			s.status = msg
			s.statMu.Unlock()
		}
		if s.hostCh == nil && (msg.Type == protocol.TypeAuth || msg.Type == protocol.TypeAuthOK) {
			s.onAuth(msg)
			continue
		}
		if msg.ID != "" {
			s.pendMu.Lock()
			ch, ok := s.pending[msg.ID]
			s.pendMu.Unlock()
			if ok {
				select {
				case ch <- Frame{Meta: msg, Data: payload}:
				default:
				}
				continue
			}
		}
		if s.hostCh != nil {
			select {
			case s.hostCh <- Frame{Meta: msg, Data: payload}:
			default:
			}
		}
	}
}

// HostRequests is the inbound RPC stream for a camera host. Only valid on host sessions.
func (s *Session) HostRequests() <-chan Frame {
	return s.hostCh
}

func (s *Session) ReplyError(id, err string) error {
	return s.send(protocol.Message{ID: id, Type: protocol.TypeError, Err: err})
}

func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
