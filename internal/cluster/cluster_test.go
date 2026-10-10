package cluster

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestStoreMemory(t *testing.T) { storeConformance(t, NewMemoryStore()) }

// TestStoreRedis needs SFU_TEST_REDIS_URL, for example redis://localhost:6379/0.
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

	_ = s.PutNode(ctx, NodeInfo{ID: a, URL: "http://a"}, time.Minute)
	_ = s.PutNode(ctx, NodeInfo{ID: b, URL: "http://b"}, 300*time.Millisecond)

	if owner, err := s.ClaimRoom(ctx, room, a); err != nil || owner != a {
		t.Fatalf("first claim = %q, %v", owner, err)
	}
	if owner, _ := s.ClaimRoom(ctx, room, b); owner != a {
		t.Errorf("second claim took a live owner's room: owner %q", owner)
	}
	if owner, _ := s.RoomOwner(ctx, room); owner != a {
		t.Errorf("RoomOwner = %q", owner)
	}
	_ = s.ReleaseRoom(ctx, room, b) // not the owner: no effect
	if owner, _ := s.RoomOwner(ctx, room); owner != a {
		t.Errorf("release by non-owner removed the claim")
	}
	_ = s.ReleaseRoom(ctx, room, a)
	if owner, _ := s.RoomOwner(ctx, room); owner != "" {
		t.Errorf("after release owner = %q", owner)
	}

	// b claims, then b stops sending heartbeats: a can take over.
	_, _ = s.ClaimRoom(ctx, room, b)
	time.Sleep(500 * time.Millisecond)
	if owner, _ := s.ClaimRoom(ctx, room, a); owner != a {
		t.Errorf("claim of a dead owner's room = %q, want takeover by a", owner)
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
	_ = s.DeleteNode(ctx, a)
	_ = s.ReleaseRoom(ctx, room, a)
}

// Two nodes sharing a store: placement, owner lookup, picking the less
// loaded node, draining, and cluster-wide tenant counts.
func TestClusterTwoNodes(t *testing.T) {
	s := NewMemoryStore()
	loadA := Load{Participants: 5, TenantRooms: map[string]int{"acme": 1}, TenantParticipants: map[string]int{"acme": 5}}
	loadB := Load{Participants: 1, TenantRooms: map[string]int{"acme": 2}, TenantParticipants: map[string]int{"acme": 1}}
	a := New(s, "a", "http://a", func() Load { return loadA }, discard())
	b := New(s, "b", "http://b", func() Load { return loadB }, discard())
	ctx := context.Background()
	for _, c := range []*Cluster{a, b} {
		c.SetTiming(20*time.Millisecond, time.Second)
		if err := c.Start(ctx); err != nil {
			t.Fatal(err)
		}
	}
	defer a.Stop()
	defer b.Stop()
	time.Sleep(50 * time.Millisecond) // both see each other

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
		t.Errorf("TenantRooms = %d, want 3 (1 on a + 2 on b)", got)
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

	b.Release("acme/r1")
	time.Sleep(20 * time.Millisecond)
	if id, _, _ := a.Owner(ctx, "acme/r1"); id != "" {
		t.Errorf("owner after release = %q", id)
	}
}
