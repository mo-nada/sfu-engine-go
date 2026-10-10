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
