package course

import (
	"math/rand"
	"testing"
)

func TestJitterMillisRange(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	for i := 0; i < 1000; i++ {
		got := jitterMillis(1, rnd) // interval=1s → [1000, 2000)
		if got < 1000 || got >= 2000 {
			t.Fatalf("jitter out of [1000,2000): %d", got)
		}
	}
	// interval=0 仍保证至少 1s 抖动窗口
	if got := jitterMillis(0, rnd); got < 1000 || got >= 2000 {
		t.Fatalf("interval=0 jitter out of range: %d", got)
	}
}
