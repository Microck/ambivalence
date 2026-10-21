package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Microck/ambivalence/internal/audio"
	"github.com/Microck/ambivalence/internal/peer"
	"github.com/Microck/ambivalence/internal/protocol"
)

func TestAudioDrainDeadlineLeavesRoomForTheBufferedTail(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

	got := audioDrainDeadline(5000, now)
	want := now.Add(5*time.Second + audioDrainGrace)
	if !got.Equal(want) {
		t.Fatalf("deadline = %s, want %s", got, want)
	}
	if !got.After(now.Add(5 * time.Second)) {
		t.Fatal("deadline must not fire at the phone's cap: that truncates a tail still in flight")
	}
}

func TestAudioDrainDeadlineWithoutACapHasNoBackstop(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	for _, maxMs := range []int{0, -1} {
		if got := audioDrainDeadline(maxMs, now); !got.IsZero() {
			t.Fatalf("audioDrainDeadline(%d) = %s, want the zero time", maxMs, got)
		}
	}
}

type fakeStreamSession struct {
	reads   int
	stopped bool
	onRead  func(ctx context.Context, call int) (peer.Frame, error)
}

func (f *fakeStreamSession) AudioStart(ctx context.Context, maxDurationMs, sampleRate int) (protocol.Message, error) {
	return protocol.Message{Type: protocol.TypeAudioStarted, ListenID: "s1", SampleRate: 16000}, nil
}

func (f *fakeStreamSession) AudioRead(ctx context.Context, listenID string, chunkMs int) (peer.Frame, error) {
	f.reads++
	return f.onRead(ctx, f.reads)
}

func (f *fakeStreamSession) AudioStop(ctx context.Context, listenID string) (protocol.Message, error) {
	f.stopped = true
	return protocol.Message{Type: protocol.TypeStatus, Ended: true}, nil
}

func TestStreamAudioInterruptKeepsInFlightChunk(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pcm := make([]byte, 3200) // 100ms at 16 kHz mono 16-bit
	for i := range pcm {
		pcm[i] = byte(i)
	}
	sess := &fakeStreamSession{
		onRead: func(rctx context.Context, call int) (peer.Frame, error) {
			// Simulate Ctrl+C arriving while the first chunk read is in flight.
			cancel()
			if err := rctx.Err(); err != nil {
				return peer.Frame{}, err
			}
			return peer.Frame{
				Meta: protocol.Message{Type: protocol.TypeFrame, Mime: protocol.AudioMime, SampleRate: 16000},
				Data: audio.EncodeWAV(pcm, 16000),
			}, nil
		},
	}
	out := filepath.Join(t.TempDir(), "interrupted.wav")
	if err := streamAudioWithContext(ctx, sess, out, 0, 16000, 100); err != nil {
		t.Fatalf("streamAudioWithContext: %v", err)
	}
	if !sess.stopped {
		t.Fatal("expected session to be stopped")
	}
	if sess.reads != 1 {
		t.Fatalf("reads = %d, want 1", sess.reads)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	gotPCM, err := audio.PCMFromWAV(raw)
	if err != nil {
		t.Fatalf("PCMFromWAV: %v", err)
	}
	if len(gotPCM) != len(pcm) {
		t.Fatalf("recorded PCM bytes = %d, want %d", len(gotPCM), len(pcm))
	}
}

func TestStreamAudioReadFailurePatchesWAVHeader(t *testing.T) {
	pcm := make([]byte, 3200)
	sess := &fakeStreamSession{
		onRead: func(rctx context.Context, call int) (peer.Frame, error) {
			if call == 1 {
				return peer.Frame{
					Meta: protocol.Message{Type: protocol.TypeFrame, Mime: protocol.AudioMime, SampleRate: 16000},
					Data: audio.EncodeWAV(pcm, 16000),
				}, nil
			}
			return peer.Frame{}, errors.New("connection lost")
		},
	}
	out := filepath.Join(t.TempDir(), "partial.wav")
	err := streamAudioWithContext(context.Background(), sess, out, 0, 16000, 100)
	if err == nil {
		t.Fatal("expected read failure error")
	}
	raw, readErr := os.ReadFile(out)
	if readErr != nil {
		t.Fatal(readErr)
	}
	gotPCM, wavErr := audio.PCMFromWAV(raw)
	if wavErr != nil {
		t.Fatalf("PCMFromWAV: %v", wavErr)
	}
	if len(gotPCM) != len(pcm) {
		t.Fatalf("header reported %d PCM bytes after read failure, want %d", len(gotPCM), len(pcm))
	}
}
