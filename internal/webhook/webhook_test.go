package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo-nada/sfu-engine/internal/store"
)

func newTestDispatcher(t *testing.T, url string) *Dispatcher {
	t.Helper()
	st := store.NewMemory()
	_ = st.CreateTenant(context.Background(), store.Tenant{ID: "acme", WebhookURL: url, WebhookSecret: "whsec_test"})
	d := NewDispatcher(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.backoff = func(int) time.Duration { return 10 * time.Millisecond }
	return d
}

func TestDeliversSignedEvents(t *testing.T) {
	var mu sync.Mutex
	var got []Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sig := r.Header.Get(SignatureHeader)
		var ts int64
		var mac string
		_, _ = fmt.Sscanf(sig, "t=%d,v1=%s", &ts, &mac)
		if sig != Sign("whsec_test", time.Unix(ts, 0), body) {
			t.Errorf("bad signature %q", sig)
		}
		var ev Event
		_ = json.Unmarshal(body, &ev)
		if r.Header.Get(EventHeader) != ev.Event {
			t.Errorf("event header %q, body %q", r.Header.Get(EventHeader), ev.Event)
		}
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	}))
	defer srv.Close()

	d := newTestDispatcher(t, srv.URL)
	d.RoomStarted("acme/standup")
	d.ParticipantJoined("acme/standup", "alice")
	d.TrackPublished("acme/standup", "alice", "camera")
	d.ParticipantLeft("acme/standup", "alice", time.Now().Add(-90*time.Second))
	d.RoomStarted("other/room") // tenant without a webhook: nothing sent
	d.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(got), got)
	}
	for _, ev := range got {
		if ev.Room.Name != "standup" || ev.ID == "" {
			t.Errorf("event = %+v", ev)
		}
		if ev.Event == ParticipantLeft && ev.Participant.DurationSeconds < 89 {
			t.Errorf("duration = %d, want about 90", ev.Participant.DurationSeconds)
		}
		if ev.Event == TrackPublished && (ev.Track == nil || ev.Track.Source != "camera") {
			t.Errorf("track = %+v", ev.Track)
		}
	}
}

func TestRetriesServerErrorsNotClientErrors(t *testing.T) {
	var calls atomic.Int32
	status := atomic.Int32{}
	status.Store(500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()

	d := newTestDispatcher(t, srv.URL)
	d.RoomStarted("acme/r1")
	waitFor(t, func() bool { return calls.Load() == maxAttempts })
	d.Close()
	if n := calls.Load(); n != maxAttempts {
		t.Errorf("500: %d attempts, want %d", n, maxAttempts)
	}

	calls.Store(0)
	status.Store(400)
	d = newTestDispatcher(t, srv.URL)
	d.RoomStarted("acme/r1")
	d.Close()
	if n := calls.Load(); n != 1 {
		t.Errorf("400: %d attempts, want 1", n)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// One tenant's hanging endpoint does not hold up another tenant's webhooks,
// and Close does not wait for it beyond its timeout.
func TestSlowTenantDoesNotBlockOthers(t *testing.T) {
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(block)
	var fastCalls atomic.Int32
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fastCalls.Add(1) }))
	defer fast.Close()

	st := store.NewMemory()
	_ = st.CreateTenant(context.Background(), store.Tenant{ID: "slow", WebhookURL: slow.URL, WebhookSecret: "s"})
	_ = st.CreateTenant(context.Background(), store.Tenant{ID: "fast", WebhookURL: fast.URL, WebhookSecret: "f"})
	d := NewDispatcher(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for range 50 {
		d.RoomStarted("slow/r")
	}
	for range 10 {
		d.RoomStarted("fast/r")
	}
	waitFor(t, func() bool { return fastCalls.Load() == 10 })

	start := time.Now()
	d.Close()
	if took := time.Since(start); took > closeTimeout+requestTimeout+time.Second {
		t.Errorf("Close took %v", took)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var internalHits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { internalHits.Add(1) }))
	defer internal.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	d := newTestDispatcher(t, redirector.URL)
	d.RoomStarted("acme/r1")
	time.Sleep(200 * time.Millisecond)
	d.Close()
	if n := internalHits.Load(); n != 0 {
		t.Errorf("the redirect was followed %d times", n)
	}
}
