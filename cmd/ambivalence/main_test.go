package main

import (
	"testing"
	"time"
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
