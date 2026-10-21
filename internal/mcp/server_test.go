package mcp

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Microck/ambivalence/internal/hub"
	"github.com/Microck/ambivalence/internal/link"
	"github.com/Microck/ambivalence/internal/peer"
	"github.com/Microck/ambivalence/internal/protocol"
)

func TestInvalidAudioArgsPreserveActiveListeningSession(t *testing.T) {
	s := &Server{audioListenID: "listen-active-1"}
	ctx := context.Background()

	badDuration := -1
	res, _, err := s.audioRecord(ctx, nil, audioArgs{DurationMs: &badDuration})
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("expected validation error from audioRecord, got res=%v err=%v", res, err)
	}
	if s.audioListenID != "listen-active-1" {
		t.Fatalf("audioRecord with invalid duration cleared audioListenID: got %q", s.audioListenID)
	}

	badMax := -1
	res, _, err = s.audioListenStart(ctx, nil, listenStartArgs{MaxDurationMs: &badMax})
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("expected validation error from audioListenStart, got res=%v err=%v", res, err)
	}
	if s.audioListenID != "listen-active-1" {
		t.Fatalf("audioListenStart with invalid maxDurationMs cleared audioListenID: got %q", s.audioListenID)
	}

	badChunk := 50
	res, _, err = s.audioListenNext(ctx, nil, listenNextArgs{ChunkMs: &badChunk})
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("expected validation error from audioListenNext, got res=%v err=%v", res, err)
	}
	if s.audioListenID != "listen-active-1" {
		t.Fatalf("audioListenNext with invalid chunkMs cleared audioListenID: got %q", s.audioListenID)
	}

	badLevel := 60_000
	res, _, err = s.audioLevel(ctx, nil, levelArgs{DurationMs: &badLevel})
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("expected validation error from audioLevel, got res=%v err=%v", res, err)
	}
	if s.audioListenID != "listen-active-1" {
		t.Fatalf("audioLevel with invalid durationMs cleared audioListenID: got %q", s.audioListenID)
	}
}

func TestTimedOutAudioListenStartDropsAgentAndNotifiesHostDeparture(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := hub.New("127.0.0.1:0")
	go func() { _ = h.Start(ctx) }()
	if err := waitListen(h, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	hubURL := "ws://" + h.ListenAddr() + protocol.Path
	device := "dev-timeout-test"
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}

	hostSess, err := peer.Dial(ctx, peer.DialOptions{
		Role:   protocol.RoleHost,
		Device: device,
		HubURL: hubURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer hostSess.Close()

	agentSess, err := peer.Dial(ctx, peer.DialOptions{
		Role:   protocol.RoleAgent,
		Device: device,
		Key:    key,
		HubURL: hubURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer agentSess.Close()

	var hostSeal *peer.Seal
	nP := link.Nonce()
	audioStartSeen := make(chan struct{})
	departureAfterTimeout := make(chan struct{})

	go func() {
		for fr := range hostSess.HostRequests() {
			switch fr.Meta.Type {
			case protocol.TypeStatus:
				if fr.Meta.Connected {
					_ = hostSess.Send(protocol.Message{Type: protocol.TypeAuth, Nonce: link.Hex(nP)})
				} else {
					select {
					case <-audioStartSeen:
						close(departureAfterTimeout)
						return
					default:
					}
				}
			case protocol.TypeAuth:
				nA, _ := link.Unhex(fr.Meta.AgentNonce)
				hostSeal = peer.NewSeal(key, device, nP, nA, false)
				mac := link.MAC(key, "phone", []byte(device), nP, nA)
				_ = hostSess.Send(protocol.Message{Type: protocol.TypeAuthOK, Nonce: fr.Meta.Nonce, MAC: link.Hex(mac)})
			case protocol.TypeSealed:
				if hostSeal == nil {
					continue
				}
				inner, err := hostSeal.Open(fr.Meta)
				if err != nil {
					continue
				}
				switch inner.Type {
				case protocol.TypeCameras:
					_ = hostSess.SendSealed(hostSeal, protocol.Message{
						ID:           inner.ID,
						Type:         protocol.TypeCameras,
						Capabilities: map[string]any{"audioVersion": float64(1)},
					}, nil)
				case protocol.TypeAudioStart:
					// Simulate getUserMedia awaiting permission past the caller's timeout.
					close(audioStartSeen)
				}
			}
		}
	}()

	s := &Server{agent: agentSess, hubURL: hubURL}
	callCtx, callCancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer callCancel()

	res, _, err := s.audioListenStart(callCtx, nil, listenStartArgs{})
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("expected timeout error result from audioListenStart, got res=%v err=%v", res, err)
	}

	select {
	case <-departureAfterTimeout:
	case <-time.After(3 * time.Second):
		t.Fatal("host did not receive agent departure after timed-out audioListenStart")
	}

	s.mu.Lock()
	stillAgent := s.agent
	listenID := s.audioListenID
	s.mu.Unlock()
	if stillAgent != nil {
		t.Fatal("expected s.agent to be dropped after timed-out audioListenStart")
	}
	if listenID != "" {
		t.Fatalf("expected empty audioListenID after timed-out audioListenStart, got %q", listenID)
	}
}

func TestConcurrentAudioListenStartsSerializeAndTrackLatestSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := hub.New("127.0.0.1:0")
	go func() { _ = h.Start(ctx) }()
	if err := waitListen(h, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	hubURL := "ws://" + h.ListenAddr() + protocol.Path
	device := "dev-concurrent-start-test"
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}

	hostSess, err := peer.Dial(ctx, peer.DialOptions{
		Role:   protocol.RoleHost,
		Device: device,
		HubURL: hubURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer hostSess.Close()

	agentSess, err := peer.Dial(ctx, peer.DialOptions{
		Role:   protocol.RoleAgent,
		Device: device,
		Key:    key,
		HubURL: hubURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer agentSess.Close()

	var (
		hostSeal *peer.Seal
		nP       = link.Nonce()
		mu       sync.Mutex
		starts   []string
		stops    []string
		activeID string
	)

	go func() {
		for fr := range hostSess.HostRequests() {
			switch fr.Meta.Type {
			case protocol.TypeStatus:
				if fr.Meta.Connected {
					_ = hostSess.Send(protocol.Message{Type: protocol.TypeAuth, Nonce: link.Hex(nP)})
				}
			case protocol.TypeAuth:
				nA, _ := link.Unhex(fr.Meta.AgentNonce)
				hostSeal = peer.NewSeal(key, device, nP, nA, false)
				mac := link.MAC(key, "phone", []byte(device), nP, nA)
				_ = hostSess.Send(protocol.Message{Type: protocol.TypeAuthOK, Nonce: fr.Meta.Nonce, MAC: link.Hex(mac)})
			case protocol.TypeSealed:
				if hostSeal == nil {
					continue
				}
				inner, err := hostSeal.Open(fr.Meta)
				if err != nil {
					continue
				}
				switch inner.Type {
				case protocol.TypeCameras:
					_ = hostSess.SendSealed(hostSeal, protocol.Message{
						ID:           inner.ID,
						Type:         protocol.TypeCameras,
						Capabilities: map[string]any{"audioVersion": float64(1)},
					}, nil)
				case protocol.TypeAudioStart:
					mu.Lock()
					id := fmt.Sprintf("listen-%d", len(starts)+1)
					starts = append(starts, id)
					activeID = id
					mu.Unlock()
					_ = hostSess.SendSealed(hostSeal, protocol.Message{
						ID:         inner.ID,
						Type:       protocol.TypeAudioStarted,
						ListenID:   id,
						SampleRate: 16000,
					}, nil)
				case protocol.TypeAudioStop:
					mu.Lock()
					stops = append(stops, inner.ListenID)
					mu.Unlock()
					_ = hostSess.SendSealed(hostSeal, protocol.Message{
						ID:    inner.ID,
						Type:  protocol.TypeStatus,
						Ended: true,
					}, nil)
				}
			}
		}
	}()

	s := &Server{agent: agentSess, hubURL: hubURL}
	var wg sync.WaitGroup
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			res, _, err := s.audioListenStart(ctx, nil, listenStartArgs{})
			if err != nil || res == nil || res.IsError {
				t.Errorf("audioListenStart failed: res=%v err=%v", res, err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	wantID := activeID
	gotStarts := append([]string(nil), starts...)
	gotStops := append([]string(nil), stops...)
	mu.Unlock()

	if len(gotStarts) != 2 {
		t.Fatalf("starts = %v, want 2 starts", gotStarts)
	}
	if len(gotStops) != 1 || gotStops[0] != gotStarts[0] {
		t.Fatalf("stops = %v, want first session %q stopped before second start", gotStops, gotStarts[0])
	}
	s.mu.Lock()
	trackedID := s.audioListenID
	s.mu.Unlock()
	if trackedID != wantID {
		t.Fatalf("tracked audioListenID = %q, want latest active session %q", trackedID, wantID)
	}
}
