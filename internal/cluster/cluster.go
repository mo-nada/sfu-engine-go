package cluster

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Heartbeat timing. A node is considered gone after TTL without a heartbeat.
const (
	DefaultInterval = 2 * time.Second
	DefaultTTL      = 6 * time.Second
)

// ErrNoNode is returned when no live, non-draining node can take a room.
var ErrNoNode = errors.New("cluster: no node available")

// Cluster is this node's view of the cluster.
type Cluster struct {
	store    Store
	id, url  string
	load     func() Load
	logger   *slog.Logger
	interval time.Duration
	ttl      time.Duration

	draining atomic.Bool
	started  atomic.Bool
	stop     chan struct{}
	done     chan struct{}

	mu    sync.Mutex
	nodes map[string]NodeInfo // live nodes from the last heartbeat round
}

// New returns a Cluster for this node. load reports the node's current load.
func New(s Store, id, url string, load func() Load, logger *slog.Logger) *Cluster {
	return &Cluster{
		store: s, id: id, url: url, load: load, logger: logger.With("node_id", id),
		interval: DefaultInterval, ttl: DefaultTTL,
		stop: make(chan struct{}), done: make(chan struct{}),
		nodes: make(map[string]NodeInfo),
	}
}

// SetTiming changes the heartbeat interval and TTL; call it before Start.
func (c *Cluster) SetTiming(interval, ttl time.Duration) { c.interval, c.ttl = interval, ttl }

// ID returns this node's ID.
func (c *Cluster) ID() string { return c.id }

// Start publishes this node and keeps it published until Stop.
func (c *Cluster) Start(ctx context.Context) error {
	if err := c.beat(ctx); err != nil {
		return err
	}
	c.started.Store(true)
	go c.loop()
	c.logger.Info("cluster node started", "url", c.url)
	return nil
}

// Stop removes this node from the cluster. It does nothing if Start did not
// succeed, and only the first call has effect.
func (c *Cluster) Stop() {
	if !c.started.CompareAndSwap(true, false) {
		return
	}
	close(c.stop)
	<-c.done
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.store.DeleteNode(ctx, c.id); err != nil {
		c.logger.Warn("cluster node delete failed", "err", err)
	}
}

// SetDraining marks this node as draining: it keeps its rooms but gets no new ones.
func (c *Cluster) SetDraining(on bool) {
	c.draining.Store(on)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.beat(ctx)
}

func (c *Cluster) loop() {
	defer close(c.done)
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), c.interval)
			if err := c.beat(ctx); err != nil {
				c.logger.Warn("cluster heartbeat failed", "err", err)
			}
			cancel()
		}
	}
}

// beat publishes this node and refreshes the view of the others.
func (c *Cluster) beat(ctx context.Context) error {
	self := NodeInfo{ID: c.id, URL: c.url, Draining: c.draining.Load(), Load: c.load()}
	if err := c.store.PutNode(ctx, self, c.ttl); err != nil {
		return err
	}
	nodes, err := c.store.Nodes(ctx)
	if err != nil {
		return err
	}
	view := make(map[string]NodeInfo, len(nodes))
	for _, n := range nodes {
		view[n.ID] = n
	}
	view[c.id] = self
	c.mu.Lock()
	c.nodes = view
	c.mu.Unlock()
	return nil
}

// Nodes returns the live nodes (from the last heartbeat round).
func (c *Cluster) Nodes() []NodeInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]NodeInfo, 0, len(c.nodes))
	for _, n := range c.nodes {
		out = append(out, n)
	}
	return out
}

func (c *Cluster) node(id string) (NodeInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.nodes[id]
	return n, ok
}

// Owner returns the live owner of roomID: its ID and URL, or "" if the room
// has no live owner.
func (c *Cluster) Owner(ctx context.Context, roomID string) (string, string, error) {
	id, err := c.store.RoomOwner(ctx, roomID)
	if err != nil || id == "" {
		return "", "", err
	}
	if id == c.id {
		return c.id, c.url, nil
	}
	if n, ok := c.node(id); ok {
		return n.ID, n.URL, nil
	}
	// The owner is not live (per our last view): ask the store for a fresh view.
	nodes, err := c.store.Nodes(ctx)
	if err != nil {
		return "", "", err
	}
	for _, n := range nodes {
		if n.ID == id {
			return n.ID, n.URL, nil
		}
	}
	return "", "", nil
}

// ClaimSelf makes this node the owner of roomID if it has no live owner,
// and returns the owner's ID and URL.
func (c *Cluster) ClaimSelf(ctx context.Context, roomID string) (string, string, error) {
	if c.draining.Load() {
		if id, url, err := c.Owner(ctx, roomID); err != nil || id != "" {
			return id, url, err
		}
		return "", "", ErrNoNode
	}
	owner, err := c.store.ClaimRoom(ctx, roomID, c.id)
	if err != nil {
		return "", "", err
	}
	if owner == c.id {
		return c.id, c.url, nil
	}
	if n, ok := c.node(owner); ok {
		return n.ID, n.URL, nil
	}
	return owner, "", errors.New("cluster: owner has no known URL")
}

// Release gives up this node's claim on roomID. It does not block the caller.
func (c *Cluster) Release(roomID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.store.ReleaseRoom(ctx, roomID, c.id); err != nil {
			c.logger.Warn("room release failed", "room_id", roomID, "err", err)
		}
	}()
}

// PickNode returns the live, non-draining node with the fewest participants
// (this node on a tie), for a new room.
func (c *Cluster) PickNode() (NodeInfo, error) {
	var best *NodeInfo
	for _, n := range c.Nodes() {
		if n.Draining {
			continue
		}
		if best == nil || n.Participants < best.Participants ||
			(n.Participants == best.Participants && (n.ID == c.id || (best.ID != c.id && n.Rooms < best.Rooms))) {
			nn := n
			best = &nn
		}
	}
	if best == nil {
		return NodeInfo{}, ErrNoNode
	}
	return *best, nil
}

// TenantRooms returns the tenant's rooms on all nodes: this node's exact
// count plus the others' last reported counts.
func (c *Cluster) TenantRooms(tenantID string) int {
	return c.sum(func(l Load) int { return l.TenantRooms[tenantID] })
}

// TenantParticipants returns the tenant's participants on all nodes.
func (c *Cluster) TenantParticipants(tenantID string) int {
	return c.sum(func(l Load) int { return l.TenantParticipants[tenantID] })
}

func (c *Cluster) sum(f func(Load) int) int {
	total := f(c.load())
	for _, n := range c.Nodes() {
		if n.ID != c.id {
			total += f(n.Load)
		}
	}
	return total
}

// RoomFinished releases the claim; it makes Cluster usable as part of a
// room.Observer chain (only this method matters).
func (c *Cluster) RoomFinished(roomID string) { c.Release(roomID) }

// RoutedHeader marks a request one node forwarded to another. Such a request
// is always handled locally, so it is never forwarded twice.
const RoutedHeader = "X-SFU-Routed"

// SetURL sets the URL other nodes use to reach this node; call it before Start.
func (c *Cluster) SetURL(url string) { c.url = url }
