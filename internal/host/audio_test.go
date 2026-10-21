package host

import (
	"testing"

	"github.com/Microck/ambivalence/internal/protocol"
)

func TestChunkDurationNormalizesLikeThePhones(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, protocol.DefaultChunkMs},
		{-1, protocol.DefaultChunkMs},
		{1, 100},
		{10000, 10000},
		{protocol.MaxChunkMs, protocol.MaxChunkMs},
		{protocol.MaxChunkMs + 1, protocol.MaxChunkMs},
	}
	for _, c := range cases {
		if got := chunkDuration(c.in); got != c.want {
			t.Errorf("chunkDuration(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
