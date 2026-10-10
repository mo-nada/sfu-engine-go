// Package cluster lets several SFU nodes work as one service.
//
// Each room lives on exactly one node (its owner). Nodes publish themselves
// and their load to a shared store with a short TTL (heartbeats). Room
// placement is a claim in the same store: the first node to claim a room
// owns it; a claim of a node that stopped sending heartbeats can be taken
// over. Joins and API calls that arrive at another node are forwarded to
// the owner, so clients can reach any node.
package cluster

import (
	"context"
	"sort"
	"sync"
	"time"
)

// NodeInfo is what a node publishes about itself.
type NodeInfo struct {
	ID       string `json:"id"`
	URL      string `json:"url"` // base URL other nodes use to reach it, e.g. http://10.0.0.5:8080
	Draining bool   `json:"draining"`
	Load
}

// Load is a node's current usage.
type Load struct {
	Rooms              int            `json:"rooms"`
	Participants       int            `json:"participants"`
	TenantRooms        map[string]int `json:"tenant_rooms,omitempty"`
	TenantParticipants map[string]int `json:"tenant_participants,omitempty"`
}

// Store is the shared cluster state.
type Store interface {
	// PutNode publishes n; it disappears after ttl without another PutNode.
	PutNode(ctx context.Context, n NodeInfo, ttl time.Duration) error
	DeleteNode(ctx context.Context, id string) error
	// Nodes returns the live nodes.
	Nodes(ctx context.Context) ([]NodeInfo, error)
	// ClaimRoom makes claim the owner of roomID if the room has no owner,
	// its owner is not live, or its owner is claim.Node (then the generation
	// is replaced). It returns the claim in force after the call. Claims
	// expire after ttl unless renewed.
	ClaimRoom(ctx context.Context, roomID string, claim Claim, ttl time.Duration) (Claim, error)
	// RoomOwner returns the claim of roomID; Node is "" if it has none. The
	// owner may be a node that is no longer live.
	RoomOwner(ctx context.Context, roomID string) (Claim, error)
	// RenewRoom extends the claim's ttl if claim is still in force, and
	// reports whether it is.
	RenewRoom(ctx context.Context, roomID string, claim Claim, ttl time.Duration) (bool, error)
	// ReleaseRoom removes the claim only if it is exactly claim (same node
	// and generation), so an old copy of a room never releases a new one.
	ReleaseRoom(ctx context.Context, roomID string, claim Claim) error
	Close() error
}

// Claim is a room's owner. Gen is unique for each copy of the room, so
// releases and renewals of an old copy never touch a newer one.
type Claim struct {
	Node string
	Gen  string
}

func (c Claim) encode() string { return c.Node + "|" + c.Gen }

func decodeClaim(v string) Claim {
	for i := len(v) - 1; i >= 0; i-- {
		if v[i] == '|' {
			return Claim{Node: v[:i], Gen: v[i+1:]}
		}
	}
	return Claim{Node: v}
}

// MemoryStore is a Store in one process, for a single node and for tests
// that run several nodes in one process.
type MemoryStore struct {
	mu    sync.Mutex
	nodes map[string]memNode
	rooms map[string]memClaim
	now   func() time.Time
}

type memClaim struct {
	claim   Claim
	expires time.Time
}

type memNode struct {
	info    NodeInfo
	expires time.Time
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{nodes: make(map[string]memNode), rooms: make(map[string]memClaim), now: time.Now}
}

func (m *MemoryStore) PutNode(_ context.Context, n NodeInfo, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nodes[n.ID] = memNode{info: n, expires: m.now().Add(ttl)}
	return nil
}

func (m *MemoryStore) DeleteNode(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.nodes, id)
	return nil
}

// alive must be called with m.mu held.
func (m *MemoryStore) alive(id string) bool {
	n, ok := m.nodes[id]
	return ok && m.now().Before(n.expires)
}

func (m *MemoryStore) Nodes(_ context.Context) ([]NodeInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []NodeInfo
	for id, n := range m.nodes {
		if m.alive(id) {
			out = append(out, n.info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// current must be called with m.mu held; it drops an expired claim.
func (m *MemoryStore) current(roomID string) (Claim, bool) {
	c, ok := m.rooms[roomID]
	if !ok {
		return Claim{}, false
	}
	if !m.now().Before(c.expires) {
		delete(m.rooms, roomID)
		return Claim{}, false
	}
	return c.claim, true
}

func (m *MemoryStore) ClaimRoom(_ context.Context, roomID string, claim Claim, ttl time.Duration) (Claim, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.current(roomID); ok && cur.Node != claim.Node && m.alive(cur.Node) {
		return cur, nil
	}
	m.rooms[roomID] = memClaim{claim: claim, expires: m.now().Add(ttl)}
	return claim, nil
}

func (m *MemoryStore) RoomOwner(_ context.Context, roomID string) (Claim, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, _ := m.current(roomID)
	return c, nil
}

func (m *MemoryStore) RenewRoom(_ context.Context, roomID string, claim Claim, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.current(roomID); ok && cur == claim {
		m.rooms[roomID] = memClaim{claim: claim, expires: m.now().Add(ttl)}
		return true, nil
	}
	return false, nil
}

func (m *MemoryStore) ReleaseRoom(_ context.Context, roomID string, claim Claim) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.current(roomID); ok && cur == claim {
		delete(m.rooms, roomID)
	}
	return nil
}

func (m *MemoryStore) Close() error { return nil }
