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
	// ClaimRoom makes nodeID the owner of roomID if it has no live owner,
	// and returns the owner after the call.
	ClaimRoom(ctx context.Context, roomID, nodeID string) (string, error)
	// RoomOwner returns the owner of roomID, or "" if it has none. The owner
	// may be a node that is no longer live.
	RoomOwner(ctx context.Context, roomID string) (string, error)
	// ReleaseRoom removes the claim if nodeID owns roomID.
	ReleaseRoom(ctx context.Context, roomID, nodeID string) error
	Close() error
}

// MemoryStore is a Store in one process, for a single node and for tests
// that run several nodes in one process.
type MemoryStore struct {
	mu    sync.Mutex
	nodes map[string]memNode
	rooms map[string]string // room ID -> node ID
	now   func() time.Time
}

type memNode struct {
	info    NodeInfo
	expires time.Time
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{nodes: make(map[string]memNode), rooms: make(map[string]string), now: time.Now}
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

func (m *MemoryStore) ClaimRoom(_ context.Context, roomID, nodeID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.rooms[roomID]; ok && m.alive(cur) {
		return cur, nil
	}
	m.rooms[roomID] = nodeID
	return nodeID, nil
}

func (m *MemoryStore) RoomOwner(_ context.Context, roomID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rooms[roomID], nil
}

func (m *MemoryStore) ReleaseRoom(_ context.Context, roomID, nodeID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rooms[roomID] == nodeID {
		delete(m.rooms, roomID)
	}
	return nil
}

func (m *MemoryStore) Close() error { return nil }
