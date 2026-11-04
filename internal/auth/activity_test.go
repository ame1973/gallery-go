package auth

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentActivityNeverMovesIdleClockBackward(t *testing.T) {
	s, _ := testService(t)
	base := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	var tick atomic.Int64
	s.now = func() time.Time { return base.Add(time.Duration(tick.Add(1)) * time.Millisecond) }
	session := login(t, s)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if _, err := s.Authenticate(t.Context(), session.Token); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	var last int64
	if err := s.db.QueryRow(`SELECT last_seen_at FROM sessions WHERE id=?`, session.ID).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if want := base.UnixMilli() + tick.Load(); last != want {
		t.Fatalf("activity moved backward: got %d want %d", last, want)
	}
}
