package audio

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/Microck/ambivalence/internal/protocol"
)

func TestWAVRoundTrip(t *testing.T) {
	pcm := make([]byte, 320) // 160 samples of silence
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], uint16(int16(i%200-100)))
	}
	wav := EncodeWAV(pcm, 16000)
	if string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" || string(wav[36:40]) != "data" {
		t.Fatalf("bad header: %q", wav[:44])
	}
	if got := binary.LittleEndian.Uint32(wav[40:44]); got != uint32(len(pcm)) {
		t.Fatalf("data size %d, want %d", got, len(pcm))
	}
	got, err := PCMFromWAV(wav)
	if err != nil || len(got) != len(pcm) || string(got) != string(pcm) {
		t.Fatalf("round trip: %v (%d bytes)", err, len(got))
	}
}

func TestPCMFromWAVRejectsForeignData(t *testing.T) {
	if _, err := PCMFromWAV([]byte("not a wav")); err == nil {
		t.Fatal("accepted junk")
	}
	wav := EncodeWAV(make([]byte, 8), 16000)
	wav[20] = 3 // not PCM
	if _, err := PCMFromWAV(wav); err == nil {
		t.Fatal("accepted a non-PCM format")
	}
}

func TestLevel(t *testing.T) {
	if got := Level(nil); got.RMSDB != -100 || got.PeakDB != -100 || got.Clipping {
		t.Fatalf("silence: %+v", got)
	}
	full := make([]byte, 200)
	for i := 0; i < len(full); i += 2 {
		binary.LittleEndian.PutUint16(full[i:], uint16(math.MaxInt16))
	}
	got := Level(full)
	if got.PeakDB != 0 || got.RMSDB != 0 || !got.Clipping {
		t.Fatalf("full scale: %+v", got)
	}
	half := make([]byte, 200)
	for i := 0; i < len(half); i += 2 {
		binary.LittleEndian.PutUint16(half[i:], uint16(int16(16384)))
	}
	got = Level(half)
	if math.Abs(got.PeakDB-(-6.0)) > 0.2 || got.Clipping {
		t.Fatalf("half scale: %+v", got)
	}
}

func TestPCMBytes(t *testing.T) {
	if got := PCMBytes(1000, 16000); got != 32000 {
		t.Fatalf("PCMBytes = %d", got)
	}
	if got := PCMBytes(0, 16000); got != 0 {
		t.Fatalf("PCMBytes(0) = %d", got)
	}
	if protocol.MaxOneShotAudioMs < 1000 || protocol.DefaultAudioSampleRate != 16000 {
		t.Fatal("audio constants changed unexpectedly")
	}
}
