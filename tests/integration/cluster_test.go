package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/mo-nada/sfu-engine/internal/app"
	"github.com/mo-nada/sfu-engine/internal/auth"
	"github.com/mo-nada/sfu-engine/internal/cluster"
	"github.com/mo-nada/sfu-engine/internal/config"
	"github.com/mo-nada/sfu-engine/internal/store"
	"github.com/mo-nada/sfu-engine/pkg/sfu"
	"github.com/mo-nada/sfu-engine/pkg/token"
)

// The cluster tests run two (or more) real nodes in this process. By default
// they share an in-memory store and cluster store. With SFU_TEST_DATABASE_URL
// and SFU_TEST_REDIS_URL set, each node opens its own connections to the real
// PostgreSQL and Redis/Valkey, like separate servers would. Give the tests
// their own Redis database (for example redis://localhost:6379/15): nodes of
// a real server running on the same database would join the test cluster.

type testNode struct {
	id   string
	url  string // http://127.0.0.1:port
	stop func() // stops at once
	quit func() // asks the node to shut down (it drains if drain > 0) and returns
}

type testCluster struct {
	t       *testing.T
	tenant  sfu.Tenant
	shared  store.Store
	sharedC cluster.Store
}

func newTestCluster(t *testing.T, autoCreate bool) *testCluster {
	t.Helper()
	id := fmt.Sprintf("c%d", time.Now().UnixNano()%1e12) // unique per run: the real stores persist
	tc := &testCluster{t: t, tenant: sfu.Tenant{
		ID: id, APIKey: id + "-key", Secret: id + "-secret-" + strings.Repeat("x", 32), AutoCreateRooms: autoCreate,
	}}
	if os.Getenv("SFU_TEST_DATABASE_URL") == "" || os.Getenv("SFU_TEST_REDIS_URL") == "" {
		tc.shared, tc.sharedC = store.NewMemory(), cluster.NewMemoryStore()
	}
	return tc
}

// start runs one node and returns it. The node is stopped when the test ends.
func (tc *testCluster) start(name string, limits store.Limits) *testNode {
	return tc.startDrain(name, limits, 0)
}

func (tc *testCluster) startDrain(name string, limits store.Limits, drain time.Duration) *testNode {
	t := tc.t
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.STUNURLs = nil
	cfg.ShutdownTimeout = 2 * time.Second
	cfg.NodeID = tc.tenant.ID + "-" + name
	cfg.Tenants = []auth.Tenant{{ID: tc.tenant.ID, APIKey: tc.tenant.APIKey, Secret: tc.tenant.Secret,
		AutoCreateRooms: tc.tenant.AutoCreateRooms, Limits: limits}}
	cfg.HeartbeatInterval, cfg.HeartbeatTTL = 100*time.Millisecond, 600*time.Millisecond
	cfg.DrainTimeout = drain
	if tc.shared != nil {
		cfg.Store, cfg.ClusterStore = tc.shared, tc.sharedC
	} else {
		cfg.DatabaseURL, cfg.RedisURL = os.Getenv("SFU_TEST_DATABASE_URL"), os.Getenv("SFU_TEST_REDIS_URL")
	}
	a, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("node %s: %v", name, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx, ln) }()
	n := &testNode{id: cfg.NodeID, url: "http://" + ln.Addr().String(), quit: cancel}
	stopped := false
	n.stop = func() {
		if stopped {
			return
		}
		stopped = true
		http.DefaultClient.CloseIdleConnections()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("node %s did not stop", name)
		}
	}
	t.Cleanup(n.stop)
	time.Sleep(250 * time.Millisecond) // let the nodes see each other
	return n
}

func (tc *testCluster) api(n *testNode, method, path, body string, out any) int {
	tc.t.Helper()
	req, _ := http.NewRequest(method, n.url+path, strings.NewReader(body))
	tok, _ := token.SignAPI(tc.tenant.Secret, token.NewAPI(tc.tenant.APIKey, time.Minute))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		tc.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// owner returns the node ID that owns the tenant's room.
func (tc *testCluster) owner(room string) string {
	tc.t.Helper()
	cs := tc.sharedC
	if cs == nil {
		rs, err := cluster.OpenRedis(context.Background(), os.Getenv("SFU_TEST_REDIS_URL"))
		if err != nil {
			tc.t.Fatal(err)
		}
		defer rs.Close()
		cs = rs
	}
	id, err := cs.RoomOwner(context.Background(), tc.tenant.ID+"/"+room)
	if err != nil {
		tc.t.Fatal(err)
	}
	return id.Node
}

func (tc *testCluster) client(n *testNode, name string) *client {
	return newClient(tc.t, "ws"+strings.TrimPrefix(n.url, "http")+"/ws", name)
}

// A room lives on one node. Clients and API calls that reach the other node
// are forwarded, and media flows between clients on both nodes.
func TestClusterForwardsJoinsAndAPICalls(t *testing.T) {
	tc := newTestCluster(t, false)
	a := tc.start("a", store.Limits{})
	b := tc.start("b", store.Limits{})

	// Created through node a: it lives on a (equal load, a prefers itself).
	if s := tc.api(a, "POST", "/v1/rooms", `{"name":"standup"}`, nil); s != http.StatusCreated {
		t.Fatalf("create room: %d", s)
	}
	if s := tc.api(b, "POST", "/v1/rooms", `{"name":"standup"}`, nil); s != http.StatusConflict {
		t.Errorf("create the same room through b: %d, want 409", s)
	}

	alice := tc.client(a, "alice") // connects to a
	alice.joinWith(tc.tenant, "standup", allGrants)
	bob := tc.client(b, "bob") // connects to b, forwarded to a
	bob.joinWith(tc.tenant, "standup", allGrants)

	alice.publish("camera", "microphone")
	bob.publish("camera")
	eventually(t, "media flows both ways across nodes", func() bool {
		return bob.receivedKind("alice/media", webrtc.RTPCodecTypeAudio) >= 10 &&
			alice.receivedKind("bob/media", webrtc.RTPCodecTypeVideo) >= 10
	})

	// API calls through b reach the room on a.
	var room struct {
		Participants []struct{ Identity string }
	}
	if s := tc.api(b, "GET", "/v1/rooms/standup", "", &room); s != http.StatusOK || len(room.Participants) != 2 {
		t.Errorf("get room through b: %d %+v", s, room)
	}
	var list struct{ Rooms []struct{ Name string } }
	tc.api(b, "GET", "/v1/rooms", "", &list)
	if len(list.Rooms) != 1 || list.Rooms[0].Name != "standup" {
		t.Errorf("list through b = %+v", list)
	}
	if s := tc.api(b, "DELETE", "/v1/rooms/standup/participants/bob", "", nil); s != http.StatusNoContent {
		t.Fatalf("remove bob through b: %d", s)
	}
	expectRemoved(t, bob) // the "removed" message travels back through b
}

// New rooms go to the less loaded node, a draining node takes no new rooms
// (it sends them to another node), and plan limits count rooms on all nodes.
func TestClusterPlacementDrainAndLimits(t *testing.T) {
	tc := newTestCluster(t, true)
	limits := store.Limits{MaxRooms: 3}
	a := tc.start("a", limits)
	b := tc.startDrain("b", limits, 3*time.Second)

	// Load a with a participant; a new room asked of a goes to b.
	alice := tc.client(a, "alice")
	alice.joinWith(tc.tenant, "busy", allGrants)
	if o := tc.owner("busy"); o != a.id {
		t.Fatalf("room busy on %q, want %q", o, a.id)
	}
	time.Sleep(300 * time.Millisecond) // a's load reaches b's view and back
	carol := tc.client(a, "carol")
	carol.joinWith(tc.tenant, "fresh", allGrants)
	if o := tc.owner("fresh"); o != b.id {
		t.Errorf("new room fresh on %q, want the less loaded %q", o, b.id)
	}

	// b starts draining (it still serves carol); new rooms asked of b go to a.
	b.quit()
	time.Sleep(300 * time.Millisecond)
	dave := tc.client(b, "dave")
	dave.joinWith(tc.tenant, "late", allGrants)
	if o := tc.owner("late"); o != a.id {
		t.Errorf("room created while b drains is on %q, want %q", o, a.id)
	}

	// Three rooms exist on two nodes: the plan allows 3, so a fourth is refused.
	if s := tc.api(a, "POST", "/v1/rooms", `{"name":"fourth"}`, nil); s != http.StatusForbidden {
		t.Errorf("fourth room: %d, want 403 (limit counts rooms on both nodes)", s)
	}
}

// A node that stops sending heartbeats loses its rooms: a new join for the
// same room name is placed on a live node.
func TestClusterNodeFailover(t *testing.T) {
	tc := newTestCluster(t, true)
	a := tc.start("a", store.Limits{})
	b := tc.start("b", store.Limits{})

	alice := tc.client(a, "alice")
	alice.joinWith(tc.tenant, "standup", allGrants)
	a.stop() // the node goes away (alice is disconnected)

	time.Sleep(800 * time.Millisecond) // longer than the heartbeat TTL
	bob := tc.client(b, "bob")
	bob.joinWith(tc.tenant, "standup", allGrants)
	var room struct{ Participants []struct{ Identity string } }
	if s := tc.api(b, "GET", "/v1/rooms/standup", "", &room); s != http.StatusOK || len(room.Participants) != 1 || room.Participants[0].Identity != "bob" {
		t.Errorf("room after failover: %d %+v", s, room)
	}
}
