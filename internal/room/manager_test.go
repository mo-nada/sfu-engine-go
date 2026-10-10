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

	r, err := m.Join("r1", NewParticipant("alice", nil, nil))
	if err != nil {
		t.Fatalf("Join alice: %v", err)
	}
	if _, err := m.Join("r1", NewParticipant("bob", nil, nil)); err != nil {
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
	if _, err := m.Join("r1", NewParticipant("alice", nil, nil)); err != nil {
		t.Fatalf("Join: %v", err)
	}
	if _, err := m.Join("r1", NewParticipant("alice", nil, nil)); !errors.Is(err, ErrDuplicateParticipant) {
		t.Errorf("err = %v, want ErrDuplicateParticipant", err)
	}
	// The same ID in another room is fine.
	if _, err := m.Join("r2", NewParticipant("alice", nil, nil)); err != nil {
		t.Errorf("Join other room: %v", err)
	}
}

func TestRejoinAfterRoomRemovedCreatesNewRoom(t *testing.T) {
	m := NewManager(discardLogger())
	first, _ := m.Join("r1", NewParticipant("alice", nil, nil))
	m.Leave("r1", "alice")

	second, err := m.Join("r1", NewParticipant("alice", nil, nil))
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
	_, _ = m.Join("r1", NewParticipant("alice", nil, nil))
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
			if _, err := m.Join(roomID, NewParticipant(id, nil, nil)); err != nil {
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
	_, _ = m.Join("r1", NewParticipant("alice", pub, sub))
	_, _ = m.Join("r2", NewParticipant("bob", nil, nil))

	m.Close()

	if _, ok := m.Room("r1"); ok {
		t.Error("room r1 still exists after Close")
	}
	for _, p := range []*peer.Peer{pub, sub} {
		if p.ConnectionState() != webrtc.PeerConnectionStateClosed {
			t.Errorf("peer state = %s, want closed", p.ConnectionState())
		}
	}
	if _, err := m.Join("r1", NewParticipant("carol", nil, nil)); !errors.Is(err, ErrManagerClosed) {
		t.Errorf("Join after Close: err = %v, want ErrManagerClosed", err)
	}
}

// When a peer closes on its own, its OnClose callback calls Leave, and Leave
// closes the peer again. This must not deadlock.
func TestPeerCloseTriggersLeave(t *testing.T) {
	m := NewManager(discardLogger())
	pub, sub := newPeer(t), newPeer(t)
	_, _ = m.Join("r1", NewParticipant("alice", pub, sub))
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
