// Package usage measures participant time per tenant for billing.
//
// Every participant's time in a room is counted in participant-seconds and
// added to the store per tenant and UTC day. Time is written every Interval
// for everyone still connected (so a crash loses at most one interval) and
// when a participant leaves.
package usage

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mo-nada/sfu-engine/internal/room"
	"github.com/mo-nada/sfu-engine/internal/store"
)

// DefaultInterval is how often time of connected participants is written.
const DefaultInterval = time.Minute

// Meter is a room.Observer that records participant time.
type Meter struct {
	room.NopObserver
	store    store.Store
	logger   *slog.Logger
	interval time.Duration
	now      func() time.Time

	mu      sync.Mutex
	active  map[string]*segment          // room ID + "\x00" + participant ID
	pending map[pendingKey]time.Duration // time not written yet
	stop    chan struct{}
	done    chan struct{}
}

type segment struct {
	tenant string
	since  time.Time // start of the time not yet counted
}

type pendingKey struct {
	tenant string
	day    time.Time
}

// NewMeter starts a Meter that writes to s every interval.
func NewMeter(s store.Store, logger *slog.Logger, interval time.Duration) *Meter {
	m := &Meter{
		store: s, logger: logger, interval: interval, now: time.Now,
		active:  make(map[string]*segment),
		pending: make(map[pendingKey]time.Duration),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go m.loop()
	return m
}

func (m *Meter) ParticipantJoined(roomID, participantID string) {
	tenant, _ := room.SplitRoomID(roomID)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active[roomID+"\x00"+participantID] = &segment{tenant: tenant, since: m.now()}
}

func (m *Meter) ParticipantLeft(roomID, participantID string, _ time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := roomID + "\x00" + participantID
	if seg, ok := m.active[key]; ok {
		m.countLocked(seg, m.now())
		delete(m.active, key)
	}
}

// countLocked moves seg's time up to now into pending. m.mu must be held.
func (m *Meter) countLocked(seg *segment, now time.Time) {
	if d := now.Sub(seg.since); d > 0 {
		m.pending[pendingKey{seg.tenant, store.Day(now)}] += d
	}
	seg.since = now
}

func (m *Meter) loop() {
	defer close(m.done)
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			m.Flush()
		case <-m.stop:
			m.Flush()
			return
		}
	}
}

// Flush counts the time of connected participants and writes everything
// pending to the store. Whole seconds are written; the rest waits for the
// next flush.
func (m *Meter) Flush() {
	m.mu.Lock()
	now := m.now()
	for _, seg := range m.active {
		m.countLocked(seg, now)
	}
	batch := make(map[pendingKey]int64)
	for k, d := range m.pending {
		if sec := int64(d / time.Second); sec > 0 {
			batch[k] = sec
			m.pending[k] = d - time.Duration(sec)*time.Second
		}
	}
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for k, sec := range batch {
		if err := m.store.AddUsage(ctx, k.tenant, k.day, sec); err != nil {
			// Put it back, so it is retried at the next flush.
			m.mu.Lock()
			m.pending[k] += time.Duration(sec) * time.Second
			m.mu.Unlock()
			m.logger.Error("usage write failed", "tenant_id", k.tenant, "err", err)
		}
	}
}

// Close writes everything and stops the Meter.
func (m *Meter) Close() {
	close(m.stop)
	<-m.done
}
