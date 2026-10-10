package cluster

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestStoreMemory(t *testing.T) { storeConformance(t, NewMemoryStore()) }

// TestStoreRedis needs SFU_TEST_REDIS_URL, for example redis://localhost:6379/15.
func TestStoreRedis(t *testing.T) {
	url := os.Getenv("SFU_TEST_REDIS_URL")
	if url == "" {
		t.Skip("set SFU_TEST_REDIS_URL to run against Redis or Valkey")
	}
	s, err := OpenRedis(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	storeConformance(t, s)
}

func storeConformance(t *testing.T, s Store) {
	ctx := context.Background()
	p := fmt.Sprintf("t%d-", time.Now().UnixNano()) // unique names in a shared store
	a, b, room := p+"a", p+"b", p+"tenant/room"
	ttl := time.Minute

	_ = s.PutNode(ctx, NodeInfo{ID: a, URL: "http://a"}, time.Minute)
	_ = s.PutNode(ctx, NodeInfo{ID: b, URL: "http://b"}, 300*time.Millisecond)

	a1 := Claim{Node: a, Gen: "1"}
	if got, err := s.ClaimRoom(ctx, room, a1, ttl); err != nil || got != a1 {
		t.Fatalf("first claim = %+v, %v", got, err)
	}
	if got, _ := s.ClaimRoom(ctx, room, Claim{Node: b, Gen: "x"}, ttl); got != a1 {
		t.Errorf("b took a live owner's room: %+v", got)
	}
	// The same node claims again: a new generation replaces the old one.
	a2 := Claim{Node: a, Gen: "2"}
	if got, _ := s.ClaimRoom(ctx, room, a2, ttl); got != a2 {
		t.Errorf("re-claim by the owner = %+v, want %+v", got, a2)
	}
	_ = s.ReleaseRoom(ctx, room, a1) // an old copy's late release: no effect
	if got, _ := s.RoomOwner(ctx, room); got != a2 {
		t.Errorf("a stale release removed the newer claim: owner %+v", got)
	}
	if ok, _ := s.RenewRoom(ctx, room, a1, ttl); ok {
		t.Error("a stale claim was renewed")
	}
	if ok, _ := s.RenewRoom(ctx, room, a2, ttl); !ok {
		t.Error("the current claim was not renewed")
	}
	_ = s.ReleaseRoom(ctx, room, a2)
	if got, _ := s.RoomOwner(ctx, room); got.Node != "" {
		t.Errorf("after release owner = %+v", got)
	}

	// b claims, then stops sending heartbeats: a can take over.
	_, _ = s.ClaimRoom(ctx, room, Claim{Node: b, Gen: "b1"}, ttl)
	time.Sleep(500 * time.Millisecond)
	if got, _ := s.ClaimRoom(ctx, room, a1, ttl); got != a1 {
		t.Errorf("claim of a dead owner's room = %+v, want takeover by a", got)
	}
	nodes, _ := s.Nodes(ctx)
	var seenA, seenB bool
	for _, n := range nodes {
		seenA = seenA || n.ID == a
		seenB = seenB || n.ID == b
	}
	if !seenA || seenB {
		t.Errorf("Nodes: a live=%v, b live=%v (b expired)", seenA, seenB)
	}

	// A claim that is not renewed expires.
	short := p + "tenant/short"
	_, _ = s.ClaimRoom(ctx, short, a1, 200*time.Millisecond)
	time.Sleep(400 * time.Millisecond)
	if got, _ := s.RoomOwner(ctx, short); got.Node != "" {
		t.Errorf("claim did not expire: %+v", got)
	}
	_ = s.DeleteNode(ctx, a)
	_ = s.ReleaseRoom(ctx, room, a1)
}

type fakeNode struct {
	mu    sync.Mutex
	load  Load
	rooms []string
	lost  []string
}

func (f *fakeNode) hooks() Hooks {
	return Hooks{
		Load:  func() Load { f.mu.Lock(); defer f.mu.Unlock(); return f.load },
		Rooms: func() []string { f.mu.Lock(); defer f.mu.Unlock(); return append([]string(nil), f.rooms...) },
		Lost: func(r string) { // like a real node: the local room is closed
			f.mu.Lock()
			defer f.mu.Unlock()
			f.lost = append(f.lost, r)
			for i, x := range f.rooms {
				if x == r {
					f.rooms = append(f.rooms[:i], f.rooms[i+1:]...)
					break
				}
			}
		},
	}
}

// Two nodes sharing a store: placement, owner lookup, picking the less
// loaded node, draining, and cluster-wide tenant counts.
func TestClusterTwoNodes(t *testing.T) {
	s := NewMemoryStore()
	fa := &fakeNode{load: Load{Participants: 5, TenantRooms: map[string]int{"acme": 1}, TenantParticipants: map[string]int{"acme": 5}}}
	fb := &fakeNode{load: Load{Participants: 1, TenantRooms: map[string]int{"acme": 2}, TenantParticipants: map[string]int{"acme": 1}}}
	a := New(s, "a", "http://a", fa.hooks(), discard())
	b := New(s, "b", "http://b", fb.hooks(), discard())
	ctx := context.Background()
	for _, c := range []*Cluster{a, b} {
		c.SetTiming(20*time.Millisecond, time.Second)
		if err := c.Start(ctx); err != nil {
			t.Fatal(err)
		}
	}
	defer a.Stop()
	defer b.Stop()
	time.Sleep(50 * time.Millisecond)

	if n, err := a.PickNode(); err != nil || n.ID != "b" {
		t.Errorf("PickNode = %+v, %v; want b (fewer participants)", n, err)
	}
	if id, url, err := b.ClaimSelf(ctx, "acme/r1"); err != nil || id != "b" || url != "http://b" {
		t.Fatalf("ClaimSelf = %q %q %v", id, url, err)
	}
	if id, url, _ := a.Owner(ctx, "acme/r1"); id != "b" || url != "http://b" {
		t.Errorf("a sees owner %q %q, want b", id, url)
	}
	if id, _, _ := a.ClaimSelf(ctx, "acme/r1"); id != "b" {
		t.Errorf("a claimed b's live room: owner %q", id)
	}
	if got := a.TenantRooms("acme"); got != 3 {
		t.Errorf("TenantRooms = %d, want 3", got)
	}
	if got := a.TenantParticipants("acme"); got != 6 {
		t.Errorf("TenantParticipants = %d, want 6", got)
	}

	b.SetDraining(true)
	time.Sleep(50 * time.Millisecond)
	if n, _ := a.PickNode(); n.ID != "a" {
		t.Errorf("PickNode with b draining = %s, want a", n.ID)
	}
	if _, _, err := b.ClaimSelf(ctx, "acme/new-room"); err == nil {
		t.Error("a draining node claimed a new room")
	}
	if id, _, err := b.ClaimSelf(ctx, "acme/r1"); err != nil || id != "b" {
		t.Errorf("a draining node could not re-claim its own room: %q %v", id, err)
	}

	b.Release("acme/r1")
	time.Sleep(20 * time.Millisecond)
	if id, _, _ := a.Owner(ctx, "acme/r1"); id != "" {
		t.Errorf("owner after release = %q", id)
	}
}

// A late release of an old copy must not remove the claim of a newer copy
// created on the same node (the split-brain the generation prevents).
func TestLateReleaseKeepsNewerCopy(t *testing.T) {
	s := NewMemoryStore()
	a := New(s, "a", "http://a", Hooks{}, discard())
	ctx := context.Background()
	_ = a.Start(ctx)
	defer a.Stop()

	_, _, _ = a.ClaimSelf(ctx, "acme/x") // first copy
	old, _ := a.forget("acme/x")         // its room finished; release pending
	_, _, _ = a.ClaimSelf(ctx, "acme/x") // a rejoin creates a new copy
	_ = s.ReleaseRoom(ctx, "acme/x", old)
	if id, _, _ := a.Owner(ctx, "acme/x"); id != "a" {
		t.Errorf("owner = %q after a late release of the old copy, want a", id)
	}
}

// After a heartbeat gap, another node took this node's room: the heartbeat
// check closes the local copy. A room whose claim vanished is claimed again.
func TestCheckClaimsLostAndReclaimed(t *testing.T) {
	s := NewMemoryStore()
	fa := &fakeNode{rooms: []string{"acme/taken", "acme/vanished"}}
	a := New(s, "a", "http://a", fa.hooks(), discard())
	b := New(s, "b", "http://b", Hooks{}, discard())
	ctx := context.Background()
	for _, c := range []*Cluster{a, b} {
		c.SetTiming(20*time.Millisecond, time.Second)
		_ = c.Start(ctx)
	}
	defer a.Stop()
	defer b.Stop()
	_, _, _ = a.ClaimSelf(ctx, "acme/taken")
	_, _, _ = a.ClaimSelf(ctx, "acme/vanished")

	// b took "taken" over (as if a had missed its heartbeats and its claim
	// had expired), and the claim of "vanished" is gone.
	s.mu.Lock()
	s.rooms["acme/taken"] = memClaim{claim: Claim{Node: "b", Gen: "g"}, expires: time.Now().Add(time.Minute)}
	delete(s.rooms, "acme/vanished")
	s.mu.Unlock()
	time.Sleep(100 * time.Millisecond) // a few heartbeats

	fa.mu.Lock()
	lost := append([]string(nil), fa.lost...)
	fa.mu.Unlock()
	if len(lost) != 1 || lost[0] != "acme/taken" {
		t.Errorf("lost = %v, want [acme/taken]", lost)
	}
	if id, _, _ := a.Owner(ctx, "acme/vanished"); id != "a" {
		t.Errorf("vanished claim not taken back: owner %q", id)
	}
}
