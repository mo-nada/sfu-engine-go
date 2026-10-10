package room

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/mo-nada/sfu-engine/internal/peer"
	"github.com/mo-nada/sfu-engine/internal/transport"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestJoinAndLeave(t *testing.T) {
	m := NewManager(discardLogger())

	r, err := m.Join("r1", NewParticipant("alice", nil, nil, nil))
	if err != nil {
		t.Fatalf("Join alice: %v", err)
	}
	if _, err := m.Join("r1", NewParticipant("bob", nil, nil, nil)); err != nil {
		t.Fatalf("Join bob: %v", err)
	}
	if got := len(r.Participants()); got != 2 {
		t.Fatalf("participants = %d, want 2", got)
	}
	if _, ok := r.Participant("alice"); !ok {
		t.Error("alice not found")
	}

	m.Leave("r1", "alice")
	if _, ok := r.Participant("alice"); ok {
		t.Error("alice still in room after Leave")
	}
	if _, ok := m.Room("r1"); !ok {
		t.Error("room removed while bob is still inside")
	}

	m.Leave("r1", "bob")
	if _, ok := m.Room("r1"); ok {
		t.Error("empty room was not removed")
	}
}

func TestDuplicateParticipantRejected(t *testing.T) {
	m := NewManager(discardLogger())
	if _, err := m.Join("r1", NewParticipant("alice", nil, nil, nil)); err != nil {
		t.Fatalf("Join: %v", err)
	}
	if _, err := m.Join("r1", NewParticipant("alice", nil, nil, nil)); !errors.Is(err, ErrDuplicateParticipant) {
		t.Errorf("err = %v, want ErrDuplicateParticipant", err)
	}
	// The same ID in another room is fine.
	if _, err := m.Join("r2", NewParticipant("alice", nil, nil, nil)); err != nil {
		t.Errorf("Join other room: %v", err)
	}
}

func TestRejoinAfterRoomRemovedCreatesNewRoom(t *testing.T) {
	m := NewManager(discardLogger())
	first, _ := m.Join("r1", NewParticipant("alice", nil, nil, nil))
	m.Leave("r1", "alice")

	second, err := m.Join("r1", NewParticipant("alice", nil, nil, nil))
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if first == second {
		t.Error("rejoin reused the removed room")
	}
}

func TestLeaveUnknownIsNoop(t *testing.T) {
	m := NewManager(discardLogger())
	m.Leave("missing", "nobody")
	_, _ = m.Join("r1", NewParticipant("alice", nil, nil, nil))
	m.Leave("r1", "nobody")
	if _, ok := m.Room("r1"); !ok {
		t.Error("room removed by leaving an unknown participant")
	}
}

func TestConcurrentJoinLeave(t *testing.T) {
	m := NewManager(discardLogger())
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("p%d", i)
			roomID := fmt.Sprintf("r%d", i%3)
			if _, err := m.Join(roomID, NewParticipant(id, nil, nil, nil)); err != nil {
				t.Errorf("Join %s: %v", id, err)
				return
			}
			if r, ok := m.Room(roomID); ok {
				_ = r.Participants()
			}
			m.Leave(roomID, id)
		}()
	}
	wg.Wait()

	for i := range 3 {
		if _, ok := m.Room(fmt.Sprintf("r%d", i)); ok {
			t.Errorf("room r%d still exists after everyone left", i)
		}
	}
}

func TestCloseClosesEveryone(t *testing.T) {
	m := NewManager(discardLogger())
	pub, sub := newPeer(t), newPeer(t)
	_, _ = m.Join("r1", NewParticipant("alice", pub, sub, nil))
	_, _ = m.Join("r2", NewParticipant("bob", nil, nil, nil))

	m.Close()

	if _, ok := m.Room("r1"); ok {
		t.Error("room r1 still exists after Close")
	}
	for _, p := range []*peer.Peer{pub, sub} {
		if p.ConnectionState() != webrtc.PeerConnectionStateClosed {
			t.Errorf("peer state = %s, want closed", p.ConnectionState())
		}
	}
	if _, err := m.Join("r1", NewParticipant("carol", nil, nil, nil)); !errors.Is(err, ErrManagerClosed) {
		t.Errorf("Join after Close: err = %v, want ErrManagerClosed", err)
	}
}

// When a peer closes on its own, its OnClose callback calls Leave, and Leave
// closes the peer again. This must not deadlock.
func TestPeerCloseTriggersLeave(t *testing.T) {
	m := NewManager(discardLogger())
	pub, sub := newPeer(t), newPeer(t)
	_, _ = m.Join("r1", NewParticipant("alice", pub, sub, nil))
	pub.OnClose(func() { m.Leave("r1", "alice") })
	sub.OnClose(func() { m.Leave("r1", "alice") })

	done := make(chan struct{})
	go func() {
		_ = pub.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: peer Close did not return")
	}

	if _, ok := m.Room("r1"); ok {
		t.Error("room still exists after its only participant's peer closed")
	}
	if sub.ConnectionState() != webrtc.PeerConnectionStateClosed {
		t.Errorf("subscriber state = %s, want closed", sub.ConnectionState())
	}
}

func newPeer(t *testing.T) *peer.Peer {
	t.Helper()
	api, err := transport.NewAPI()
	if err != nil {
		t.Fatalf("NewAPI: %v", err)
	}
	p, err := peer.New(api, transport.Configuration(nil), discardLogger(), t.Name())
	if err != nil {
		t.Fatalf("peer.New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// recorder is a Notifier that keeps every event.
type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) notify(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) all() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

func TestRosterJoinLeaveAndMuteEvents(t *testing.T) {
	m := NewManager(discardLogger())
	alice, bob := &recorder{}, &recorder{}

	if _, err := m.Join("r1", NewParticipant("alice", nil, nil, alice.notify)); err != nil {
		t.Fatal(err)
	}
	if err := m.SetMuted("r1", "alice", "microphone", true); err != nil {
		t.Fatalf("SetMuted: %v", err)
	}
	if _, err := m.Join("r1", NewParticipant("bob", nil, nil, bob.notify)); err != nil {
		t.Fatal(err)
	}

	// Bob's roster shows alice with her microphone muted.
	got := bob.all()
	if len(got) != 1 || got[0].Kind != EventRoster || len(got[0].Roster) != 1 ||
		got[0].Roster[0].ID != "alice" || !got[0].Roster[0].Muted["microphone"] {
		t.Fatalf("bob events = %+v, want roster with muted alice", got)
	}

	if err := m.SetMuted("r1", "bob", "camera", true); err != nil {
		t.Fatalf("SetMuted: %v", err)
	}
	m.Leave("r1", "bob")

	want := []Event{
		{Kind: EventRoster, Roster: []ParticipantInfo{}},
		{Kind: EventJoined, Participant: "bob"},
		{Kind: EventMuted, Participant: "bob", Source: "camera", Muted: true},
		{Kind: EventLeft, Participant: "bob"},
	}
	got = alice.all()
	if len(got) != len(want) {
		t.Fatalf("alice events = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Kind != want[i].Kind || got[i].Participant != want[i].Participant ||
			got[i].Source != want[i].Source || got[i].Muted != want[i].Muted {
			t.Errorf("alice event %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	if err := m.SetMuted("r1", "nobody", "camera", true); !errors.Is(err, ErrNotInRoom) {
		t.Errorf("SetMuted unknown: err = %v, want ErrNotInRoom", err)
	}
}

func TestCreateExplicitRoomAndEmptyTimeout(t *testing.T) {
	m := NewManager(discardLogger())
	if _, err := m.Create("t/r1", Options{MaxParticipants: 2, EmptyTimeout: 150 * time.Millisecond}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Create("t/r1", Options{}); !errors.Is(err, ErrRoomExists) {
		t.Errorf("second Create: err = %v, want ErrRoomExists", err)
	}

	// Joining stops the empty timer; leaving restarts it instead of removing the room.
	_, _ = m.Join("t/r1", NewParticipant("alice", nil, nil, nil))
	time.Sleep(250 * time.Millisecond)
	if _, ok := m.Room("t/r1"); !ok {
		t.Fatal("room with a participant was removed by the empty timer")
	}
	m.Leave("t/r1", "alice")
	if _, ok := m.Room("t/r1"); !ok {
		t.Fatal("explicit room removed right after the last participant left")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := m.Room("t/r1"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("empty explicit room not removed after EmptyTimeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMaxParticipants(t *testing.T) {
	m := NewManager(discardLogger())
	_, _ = m.Create("t/r1", Options{MaxParticipants: 1, EmptyTimeout: time.Minute})
	if _, err := m.Join("t/r1", NewParticipant("alice", nil, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Join("t/r1", NewParticipant("bob", nil, nil, nil)); !errors.Is(err, ErrRoomFull) {
		t.Errorf("err = %v, want ErrRoomFull", err)
	}
}

func TestRemoveAndDeleteNotifyAndClose(t *testing.T) {
	m := NewManager(discardLogger())
	alice, bob, carol := &recorder{}, &recorder{}, &recorder{}
	_, _ = m.Join("t/r1", NewParticipant("alice", nil, nil, alice.notify))
	_, _ = m.Join("t/r1", NewParticipant("bob", nil, nil, bob.notify))

	if err := m.Remove("t/r1", "bob", "kicked"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := bob.all(); got[len(got)-1].Kind != EventRemoved || got[len(got)-1].Reason != "kicked" {
		t.Errorf("bob's last event = %+v, want removed/kicked", got[len(got)-1])
	}
	if got := alice.all(); got[len(got)-1].Kind != EventLeft || got[len(got)-1].Participant != "bob" {
		t.Errorf("alice's last event = %+v, want bob left", got[len(got)-1])
	}
	if err := m.Remove("t/r1", "bob", "again"); !errors.Is(err, ErrNotInRoom) {
		t.Errorf("Remove twice: err = %v, want ErrNotInRoom", err)
	}

	_, _ = m.Join("t/r1", NewParticipant("carol", nil, nil, carol.notify))
	if err := m.Delete("t/r1", "room closed"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for name, r := range map[string]*recorder{"alice": alice, "carol": carol} {
		if got := r.all(); got[len(got)-1].Kind != EventRemoved {
			t.Errorf("%s's last event = %+v, want removed", name, got[len(got)-1])
		}
	}
	if _, ok := m.Room("t/r1"); ok {
		t.Error("room still exists after Delete")
	}
	if err := m.Delete("t/r1", "x"); !errors.Is(err, ErrRoomNotFound) {
		t.Errorf("Delete twice: err = %v, want ErrRoomNotFound", err)
	}
}

func TestListSummaryDetails(t *testing.T) {
	m := NewManager(discardLogger())
	_, _ = m.Create("acme/b", Options{MaxParticipants: 5, EmptyTimeout: time.Minute})
	_, _ = m.Join("acme/a", NewParticipant("alice", nil, nil, nil))
	_, _ = m.Join("globex/a", NewParticipant("eve", nil, nil, nil))
	_ = m.SetMuted("acme/a", "alice", "microphone", true)

	list := m.List("acme/")
	if len(list) != 2 || list[0].ID != "acme/a" || list[1].ID != "acme/b" || list[0].NumParticipants != 1 {
		t.Fatalf("List = %+v", list)
	}
	s, err := m.Summary("acme/b")
	if err != nil || s.Options.MaxParticipants != 5 || !s.Explicit {
		t.Errorf("Summary = %+v, %v", s, err)
	}
	d, err := m.Details("acme/a")
	if err != nil || len(d) != 1 || d[0].ID != "alice" || !d[0].Muted["microphone"] || d[0].JoinedAt.IsZero() {
		t.Errorf("Details = %+v, %v", d, err)
	}
	if _, err := m.Details("acme/missing"); !errors.Is(err, ErrRoomNotFound) {
		t.Errorf("Details missing: err = %v", err)
	}
}

func TestCreateWithoutTimeoutUsesDefault(t *testing.T) {
	m := NewManager(discardLogger())
	r, err := m.Create("t/r1", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r.opts.EmptyTimeout != DefaultEmptyTimeout {
		t.Errorf("EmptyTimeout = %v, want default", r.opts.EmptyTimeout)
	}
	time.Sleep(50 * time.Millisecond)
	if _, ok := m.Room("t/r1"); !ok {
		t.Error("room without a timeout was removed at once")
	}
}

func TestDuplicateReportedBeforeFull(t *testing.T) {
	m := NewManager(discardLogger())
	_, _ = m.Create("t/r1", Options{MaxParticipants: 1})
	_, _ = m.Join("t/r1", NewParticipant("alice", nil, nil, nil))
	if _, err := m.Join("t/r1", NewParticipant("alice", nil, nil, nil)); !errors.Is(err, ErrDuplicateParticipant) {
		t.Errorf("err = %v, want ErrDuplicateParticipant", err)
	}
}

// A late leave of an old session must not remove a newer session that uses
// the same identity.
func TestLeaveParticipantOnlyRemovesItself(t *testing.T) {
	m := NewManager(discardLogger())
	old := NewParticipant("alice", nil, nil, nil)
	_, _ = m.Join("t/r1", old)
	_, _ = m.Join("t/r1", NewParticipant("bob", nil, nil, nil)) // keeps the room open
	m.Leave("t/r1", "alice")

	fresh := NewParticipant("alice", nil, nil, nil)
	if _, err := m.Join("t/r1", fresh); err != nil {
		t.Fatal(err)
	}
	m.LeaveParticipant("t/r1", old) // the old session ends late
	r, _ := m.Room("t/r1")
	if p, ok := r.Participant("alice"); !ok || p != fresh {
		t.Error("the old session's leave removed the new session")
	}
}
