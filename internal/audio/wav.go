// Package audio holds the wire-neutral audio helpers shared by the fake and real hosts: raw
// 16-bit mono PCM in, a canonical WAV file or a loudness summary out.
package audio

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/Microck/ambivalence/internal/protocol"
)

const (
	// BytesPerSample is one mono 16-bit sample.
	BytesPerSample = 2
	headerBytes    = 44
)

// PCMBytes is the number of raw PCM bytes a recording of durationMs at sampleRate occupies.
func PCMBytes(durationMs, sampleRate int) int {
	if durationMs <= 0 || sampleRate <= 0 {
		return 0
	}
	return durationMs * sampleRate / 1000 * BytesPerSample
}

// WAVHeader is the 44-byte canonical header for a mono 16-bit PCM data chunk of dataBytes.
func WAVHeader(dataBytes, sampleRate int) []byte {
	if sampleRate <= 0 {
		sampleRate = protocol.DefaultAudioSampleRate
	}
	out := make([]byte, headerBytes)
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(36+dataBytes))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16) // fmt chunk size
	binary.LittleEndian.PutUint16(out[20:22], 1)  // PCM
	binary.LittleEndian.PutUint16(out[22:24], 1)  // mono
	binary.LittleEndian.PutUint32(out[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(out[28:32], uint32(sampleRate*BytesPerSample))
	binary.LittleEndian.PutUint16(out[32:34], BytesPerSample) // block align
	binary.LittleEndian.PutUint16(out[34:36], 16)             // bits per sample
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(dataBytes))
	return out
}

// EncodeWAV wraps raw little-endian 16-bit mono PCM in a RIFF/WAVE header. It never mutates
// pcm. Chunks between reads are independent files, so each one plays on its own.
func EncodeWAV(pcm []byte, sampleRate int) []byte {
	n := len(pcm) &^ 1 // a trailing odd byte cannot be a sample
	out := make([]byte, headerBytes+n)
	copy(out, WAVHeader(n, sampleRate))
	copy(out[headerBytes:], pcm[:n])
	return out
}

// Level summarises raw little-endian 16-bit PCM for the agent. It returns -100 dB for silence
// so JSON never carries -Inf.
func Level(pcm []byte) protocol.AudioLevel {
	n := len(pcm) / 2
	if n == 0 {
		return protocol.AudioLevel{RMSDB: -100, PeakDB: -100}
	}
	var sumSquares float64
	peak := 0
	for i := 0; i < n; i++ {
		v := int(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
		f := float64(v)
		sumSquares += f * f
		if v == math.MinInt16 {
			v = math.MaxInt16 // |−32768| is 32768, which still fits after this clamp
		}
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	rms := math.Sqrt(sumSquares / float64(n))
	return protocol.AudioLevel{
		RMSDB:    db(rms, 32768),
		PeakDB:   db(float64(peak), 32768),
		Clipping: peak >= 32767,
	}
}

func db(v, fullScale float64) float64 {
	if v <= 0 {
		return -100
	}
	d := 20 * math.Log10(v/fullScale)
	if d < -100 {
		return -100
	}
	return math.Round(d*10) / 10
}

// PCMFromWAV extracts the data chunk from a canonical 16-bit mono WAV file. It rejects files
// whose fmt chunk is not the format this protocol produces, so a corrupt or foreign file can
// never be spliced into a recording.
func PCMFromWAV(wav []byte) ([]byte, error) {
	if len(wav) < headerBytes || string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return nil, fmt.Errorf("not a WAV file")
	}
	if binary.LittleEndian.Uint16(wav[20:22]) != 1 || binary.LittleEndian.Uint16(wav[22:24]) != 1 || binary.LittleEndian.Uint16(wav[34:36]) != 16 {
		return nil, fmt.Errorf("unsupported WAV format")
	}
	// Walk chunks instead of assuming the data chunk starts at byte 44.
	off := 12
	for off+8 <= len(wav) {
		id := string(wav[off : off+4])
		size := int(binary.LittleEndian.Uint32(wav[off+4 : off+8]))
		start := off + 8
		if start+size > len(wav) {
			return nil, fmt.Errorf("truncated WAV chunk %q", id)
		}
		if id == "data" {
			return wav[start : start+size], nil
		}
		off = start + size + size%2
	}
	return nil, fmt.Errorf("WAV has no data chunk")
}
