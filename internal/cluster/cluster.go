package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Heartbeat timing. A node is considered gone after TTL without a heartbeat.
// A room claim expires after ClaimTTLFactor heartbeat TTLs unless its owner
// renews it, so claims left behind by a crash or a lost release disappear.
const (
	DefaultInterval = 2 * time.Second
	DefaultTTL      = 6 * time.Second
	ClaimTTLFactor  = 10
)

// RoutedHeader marks a request one node forwarded to another. Such a request
// is always handled locally, so it is never forwarded twice.
const RoutedHeader = "X-SFU-Routed"

// ErrNoNode is returned when no live, non-draining node can take a room.
var ErrNoNode = errors.New("cluster: no node available")

// Hooks connect the cluster to the node's rooms.
type Hooks struct {
	// Load reports the node's current load.
	Load func() Load
	// Rooms lists the rooms that exist on this node.
	Rooms func() []string
	// Lost is called for a local room whose claim another live node now
	// holds (for example after this node missed heartbeats): the local copy
	// must be closed, so the room has only one copy.
	Lost func(roomID string)
}

// Cluster is this node's view of the cluster.
type Cluster struct {
	store    Store
	id, url  string
	hooks    Hooks
	logger   *slog.Logger
	interval time.Duration
	ttl      time.Duration

	draining atomic.Bool
	started  atomic.Bool
	stop     chan struct{}
	done     chan struct{}
	releases sync.WaitGroup // claim releases still running

	mu     sync.Mutex
	nodes  map[string]NodeInfo // live nodes from the last heartbeat round
	claims map[string]Claim    // room ID -> this node's current claim
}

// New returns a Cluster for this node.
func New(s Store, id, url string, hooks Hooks, logger *slog.Logger) *Cluster {
	if hooks.Load == nil {
		hooks.Load = func() Load { return Load{} }
	}
	if hooks.Rooms == nil {
		hooks.Rooms = func() []string { return nil }
	}
	if hooks.Lost == nil {
		hooks.Lost = func(string) {}
	}
	return &Cluster{
		store: s, id: id, url: url, hooks: hooks, logger: logger.With("node_id", id),
		interval: DefaultInterval, ttl: DefaultTTL,
		stop: make(chan struct{}), done: make(chan struct{}),
		nodes: make(map[string]NodeInfo), claims: make(map[string]Claim),
	}
}

// SetTiming changes the heartbeat interval and TTL; call it before Start.
func (c *Cluster) SetTiming(interval, ttl time.Duration) { c.interval, c.ttl = interval, ttl }

// SetURL sets the URL other nodes use to reach this node; call it before Start.
func (c *Cluster) SetURL(url string) { c.url = url }

// ID returns this node's ID.
func (c *Cluster) ID() string { return c.id }

func (c *Cluster) claimTTL() time.Duration { return ClaimTTLFactor * c.ttl }

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

// Stop waits for pending claim releases and removes this node from the
// cluster. It does nothing if Start did not succeed; only the first call has
// effect.
func (c *Cluster) Stop() {
	if !c.started.CompareAndSwap(true, false) {
		return
	}
	close(c.stop)
	<-c.done
	waited := make(chan struct{})
	go func() { c.releases.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		c.logger.Warn("claim releases did not finish; they expire with their TTL")
	}
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
			} else {
				c.checkClaims(ctx)
			}
			cancel()
		}
	}
}

// beat publishes this node and refreshes the view of the others.
func (c *Cluster) beat(ctx context.Context) error {
	self := NodeInfo{ID: c.id, URL: c.url, Draining: c.draining.Load(), Load: c.hooks.Load()}
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

// checkClaims renews the claims of this node's rooms. A room whose claim
// another live node now holds is closed here (it lives there now); a room
// whose claim disappeared is claimed again.
func (c *Cluster) checkClaims(ctx context.Context) {
	for _, roomID := range c.hooks.Rooms() {
		c.mu.Lock()
		claim, ok := c.claims[roomID]
		c.mu.Unlock()
		if ok {
			if renewed, err := c.store.RenewRoom(ctx, roomID, claim, c.claimTTL()); err != nil || renewed {
				continue
			}
		}
		cur, err := c.store.RoomOwner(ctx, roomID)
		if err != nil {
			continue
		}
		if cur.Node != "" && cur.Node != c.id {
			if _, live := c.node(cur.Node); live {
				c.logger.Warn("room claimed by another node; closing the local copy", "room_id", roomID, "owner", cur.Node)
				c.forget(roomID)
				c.hooks.Lost(roomID)
				continue
			}
		}
		if _, _, err := c.claim(ctx, roomID); err != nil {
			c.logger.Warn("room reclaim failed", "room_id", roomID, "err", err)
		}
	}
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
	cur, err := c.store.RoomOwner(ctx, roomID)
	if err != nil || cur.Node == "" {
		return "", "", err
	}
	if cur.Node == c.id {
		return c.id, c.url, nil
	}
	if n, ok := c.node(cur.Node); ok {
		return n.ID, n.URL, nil
	}
	// Not live in our last view: ask the store for a fresh one.
	nodes, err := c.store.Nodes(ctx)
	if err != nil {
		return "", "", err
	}
	for _, n := range nodes {
		if n.ID == cur.Node {
			return n.ID, n.URL, nil
		}
	}
	return "", "", nil
}

// ClaimSelf makes this node the owner of roomID, with a new generation, if
// the room has no live owner or this node owns it; it returns the owner's ID
// and URL. Call it right before creating the room locally: the new
// generation makes any pending release of an older copy harmless.
func (c *Cluster) ClaimSelf(ctx context.Context, roomID string) (string, string, error) {
	if c.draining.Load() {
		if id, url, err := c.Owner(ctx, roomID); err != nil || (id != "" && id != c.id) {
			return id, url, err
		}
		if !c.hasClaim(roomID) {
			return "", "", ErrNoNode
		}
	}
	return c.claim(ctx, roomID)
}

func (c *Cluster) hasClaim(roomID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.claims[roomID]
	return ok
}

func (c *Cluster) claim(ctx context.Context, roomID string) (string, string, error) {
	mine := Claim{Node: c.id, Gen: newGen()}
	got, err := c.store.ClaimRoom(ctx, roomID, mine, c.claimTTL())
	if err != nil {
		return "", "", err
	}
	if got == mine {
		c.mu.Lock()
		c.claims[roomID] = mine
		c.mu.Unlock()
		return c.id, c.url, nil
	}
	if n, ok := c.node(got.Node); ok {
		return n.ID, n.URL, nil
	}
	return got.Node, "", errors.New("cluster: owner has no known URL")
}

func (c *Cluster) forget(roomID string) (Claim, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	claim, ok := c.claims[roomID]
	delete(c.claims, roomID)
	return claim, ok
}

// Release gives up this node's current claim on roomID without blocking.
// Only that exact claim is released: if the room was claimed again meanwhile
// (a newer copy), the release does nothing.
func (c *Cluster) Release(roomID string) {
	claim, ok := c.forget(roomID)
	if !ok {
		return
	}
	c.releases.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.store.ReleaseRoom(ctx, roomID, claim); err != nil {
			c.logger.Warn("room release failed", "room_id", roomID, "err", err)
		}
	})
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

// RemoteTenantCounts returns the tenant's rooms and participants on the other
// live nodes, as they last reported them.
func (c *Cluster) RemoteTenantCounts(tenantID string) (rooms, participants int) {
	for _, n := range c.Nodes() {
		if n.ID != c.id {
			rooms += n.TenantRooms[tenantID]
			participants += n.TenantParticipants[tenantID]
		}
	}
	return rooms, participants
}

// TenantRooms returns the tenant's rooms on all nodes.
func (c *Cluster) TenantRooms(tenantID string) int {
	r, _ := c.RemoteTenantCounts(tenantID)
	return r + c.hooks.Load().TenantRooms[tenantID]
}

// TenantParticipants returns the tenant's participants on all nodes.
func (c *Cluster) TenantParticipants(tenantID string) int {
	_, p := c.RemoteTenantCounts(tenantID)
	return p + c.hooks.Load().TenantParticipants[tenantID]
}

func newGen() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
