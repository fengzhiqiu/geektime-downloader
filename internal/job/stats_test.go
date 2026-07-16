package job_test

import (
	"sync"
	"testing"

	"github.com/nicoxiang/geektime-downloader/internal/job"
)

func TestStatsIncAndSnapshot(t *testing.T) {
	var s job.Stats
	s.Inc("AUTH_EXPIRED")
	s.Inc("AUTH_EXPIRED")
	s.Inc("RATE_LIMITED")
	s.Inc("TIMEOUT")
	s.Inc("SOMETHING_UNKNOWN") // -> INTERNAL_ERROR

	got := s.Snapshot()
	if got["AUTH_EXPIRED"] != 2 {
		t.Fatalf("AUTH_EXPIRED want 2, got %d", got["AUTH_EXPIRED"])
	}
	if got["RATE_LIMITED"] != 1 {
		t.Fatalf("RATE_LIMITED want 1, got %d", got["RATE_LIMITED"])
	}
	if got["TIMEOUT"] != 1 {
		t.Fatalf("TIMEOUT want 1, got %d", got["TIMEOUT"])
	}
	if got["INTERNAL_ERROR"] != 1 {
		t.Fatalf("INTERNAL_ERROR want 1, got %d", got["INTERNAL_ERROR"])
	}
}

func TestStatsConcurrent(t *testing.T) {
	var s job.Stats
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Inc("AUTH_EXPIRED")
			s.Inc("RATE_LIMITED")
		}()
	}
	wg.Wait()
	got := s.Snapshot()
	if got["AUTH_EXPIRED"] != 100 || got["RATE_LIMITED"] != 100 {
		t.Fatalf("want 100/100, got %v", got)
	}
}
