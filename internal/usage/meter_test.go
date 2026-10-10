package usage

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/mo-nada/sfu-engine/internal/store"
)

// fakeClock is a clock tests move by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func TestMeterCountsParticipantTime(t *testing.T) {
	st := store.NewMemory()
	_ = st.CreateTenant(context.Background(), store.Tenant{ID: "acme"})
	clock := &fakeClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	m := NewMeter(st, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Hour)
	m.now = clock.now

	m.ParticipantJoined("acme/r1", "alice")
	m.ParticipantJoined("acme/r1", "bob")
	clock.add(90 * time.Second)
	m.ParticipantLeft("acme/r1", "bob", time.Time{})
	clock.add(30 * time.Second)
	m.Flush() // alice still connected: 120 s so far
	clock.add(60 * time.Second)
	m.Close() // alice's last 60 s are written on close

	days, _ := st.Usage(context.Background(), "acme", clock.now(), clock.now())
	// alice 180 s + bob 90 s
	if len(days) != 1 || days[0].ParticipantSeconds != 270 {
		t.Errorf("usage = %+v, want 270 participant-seconds", days)
	}
}

func TestMeterSplitsByDay(t *testing.T) {
	st := store.NewMemory()
	clock := &fakeClock{t: time.Date(2026, 3, 1, 23, 59, 0, 0, time.UTC)}
	m := NewMeter(st, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Hour)
	m.now = clock.now

	m.ParticipantJoined("acme/r1", "alice")
	clock.add(30 * time.Second)
	m.Flush() // 30 s on March 1
	clock.add(60 * time.Second)
	m.ParticipantLeft("acme/r1", "alice", time.Time{}) // 60 s more, counted on March 2
	m.Close()

	days, _ := st.Usage(context.Background(), "acme", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC))
	// 23:59:00-00:00:00 is March 1 (60 s); 00:00:00-00:00:30 is March 2 (30 s).
	if len(days) != 2 || days[0].ParticipantSeconds != 60 || days[1].ParticipantSeconds != 30 {
		t.Errorf("usage = %+v, want 60 s on March 1 and 30 s on March 2", days)
	}
}
