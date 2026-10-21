package mcp

import (
	"context"
	"testing"
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
