// Package app wires the SFU components together and manages startup and shutdown.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	apidocs "github.com/mo-nada/sfu-engine/api"
	"github.com/mo-nada/sfu-engine/internal/admin"
	serverapi "github.com/mo-nada/sfu-engine/internal/api"
	"github.com/mo-nada/sfu-engine/internal/auth"
	"github.com/mo-nada/sfu-engine/internal/cluster"
	"github.com/mo-nada/sfu-engine/internal/config"
	"github.com/mo-nada/sfu-engine/internal/room"
	"github.com/mo-nada/sfu-engine/internal/signaling"
	"github.com/mo-nada/sfu-engine/internal/store"
	"github.com/mo-nada/sfu-engine/internal/transport"
	"github.com/mo-nada/sfu-engine/internal/usage"
	"github.com/mo-nada/sfu-engine/internal/web"
	"github.com/mo-nada/sfu-engine/internal/webhook"
	"github.com/mo-nada/sfu-engine/pkg/token"
)

// App is the running SFU server.
type App struct {
	cfg        config.Config
	logger     *slog.Logger
	server     *http.Server
	rooms      *room.Manager
	signaling  *signaling.Handler
	store      store.Store
	ownStore   bool // the app opened the store and closes it
	cluster    *cluster.Cluster
	clStore    cluster.Store
	ownClStore bool
	meter      *usage.Meter
	webhooks   *webhook.Dispatcher
}

// New creates an App from a validated config. It opens the stores and wires
// the Pion API, rooms, signaling (/ws), the server API (/v1), the admin API
// (/admin/v1, if an admin secret is set), usage metering, webhooks, the
// cluster, the demo page (/) and the API documents (/api, /docs).
func New(cfg config.Config, logger *slog.Logger) (*App, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	api, err := transport.NewAPI()
	if err != nil {
		return nil, fmt.Errorf("webrtc api: %w", err)
	}
	a := &App{cfg: cfg, logger: logger}

	// Durable data: tenants, keys, usage.
	a.store = cfg.Store
	if a.store == nil {
		if cfg.DatabaseURL != "" {
			pg, err := store.OpenPostgres(ctx, cfg.DatabaseURL)
			if err != nil {
				return nil, err
			}
			a.store = pg
			logger.Info("using PostgreSQL for tenants, keys and usage")
		} else {
			a.store = store.NewMemory()
			logger.Warn("no -database-url: tenants, keys and usage are kept in memory and lost on restart")
		}
		a.ownStore = true
	}

	seed := append([]auth.Tenant(nil), cfg.Tenants...)
	if cfg.TenantsFile != "" {
		fromFile, err := auth.LoadTenants(cfg.TenantsFile)
		if err != nil {
			a.closeStores()
			return nil, err
		}
		seed = append(seed, fromFile...)
	}
	var dev auth.Tenant
	if cfg.Dev {
		dev = auth.DevTenant()
		seed = append(seed, dev)
		logger.Warn("development mode: /dev/token gives join tokens to anyone; never use in production")
	}
	tenants, err := auth.NewTenantsWithStore(ctx, a.store, seed)
	if err != nil {
		a.closeStores()
		return nil, fmt.Errorf("tenants: %w", err)
	}
	if cfg.Dev {
		// A persistent store keeps the dev secret of an earlier start: use it.
		if k, _, err := a.store.LookupAPIKey(ctx, dev.APIKey); err == nil {
			dev.Secret = k.Secret
		}
	}

	// Cluster state: node registry and room placement.
	a.clStore = cfg.ClusterStore
	if a.clStore == nil {
		if cfg.RedisURL != "" {
			rs, err := cluster.OpenRedis(ctx, cfg.RedisURL)
			if err != nil {
				a.closeStores()
				return nil, err
			}
			a.clStore = rs
			logger.Info("cluster mode: using Redis/Valkey for nodes and room placement")
		} else {
			a.clStore = cluster.NewMemoryStore()
		}
		a.ownClStore = true
	}
	nodeID := cfg.NodeID
	if nodeID == "" {
		host, _ := os.Hostname()
		b := make([]byte, 3)
		_, _ = rand.Read(b)
		nodeID = host + "-" + hex.EncodeToString(b)
	}

	a.rooms = room.NewManager(logger)
	a.cluster = cluster.New(a.clStore, nodeID, cfg.NodeURL, cluster.Hooks{
		Load: func() cluster.Load {
			s := a.rooms.Stats()
			return cluster.Load{Rooms: s.Rooms, Participants: s.Participants, TenantRooms: s.TenantRooms, TenantParticipants: s.TenantParticipants}
		},
		Rooms: a.rooms.RoomIDs,
		// Another node owns the room now: close this copy; its participants
		// reconnect and are forwarded to the owner.
		Lost: func(roomID string) { _ = a.rooms.Delete(roomID, "room moved to another node, reconnect") },
	}, logger)
	a.rooms.SetRemoteCounts(a.cluster.RemoteTenantCounts)
	if cfg.HeartbeatInterval > 0 && cfg.HeartbeatTTL > 0 {
		a.cluster.SetTiming(cfg.HeartbeatInterval, cfg.HeartbeatTTL)
	}
	a.meter = usage.NewMeter(a.store, logger, usage.DefaultInterval)
	a.webhooks = webhook.NewDispatcher(a.store, logger)
	a.rooms.SetObserver(room.Observers{a.meter, a.webhooks, releaseOnFinish{c: a.cluster}})

	a.signaling = signaling.NewHandler(api, transport.Configuration(cfg.STUNURLs), a.rooms, tenants, logger)
	a.signaling.SetRouting(joinRouting{c: a.cluster, rooms: a.rooms})
	apiServer := serverapi.NewServer(tenants, serverapi.LocalRooms{Manager: a.rooms}, logger)
	apiServer.SetCluster(a.cluster)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("GET /ws", a.signaling)
	mux.Handle("/v1/", apiServer.Handler())
	if cfg.AdminSecret != "" {
		mux.Handle("/admin/v1/", admin.NewServer(cfg.AdminSecret, a.store, tenants, logger).Handler())
		logger.Info("admin API enabled at /admin/v1")
	}
	if cfg.Dev {
		mux.Handle("GET /dev/token", devTokenHandler(dev))
	}
	mux.Handle("GET /{$}", web.Handler())
	mux.Handle("GET /docs", web.Docs())
	mux.Handle("GET /docs/signaling", web.SignalingDocs())
	mux.Handle("GET /api/openapi.yaml", web.File("application/yaml", apidocs.OpenAPI))
	mux.Handle("GET /api/asyncapi.yaml", web.File("application/yaml", apidocs.AsyncAPI))

	a.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return a, nil
}

// Run listens on the configured address and serves until ctx is cancelled,
// then shuts down gracefully.
func (a *App) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", a.cfg.Addr)
	if err != nil {
		a.shutdownParts()
		return fmt.Errorf("listen on %s: %w", a.cfg.Addr, err)
	}
	return a.Serve(ctx, ln)
}

// Serve serves on ln until ctx is cancelled, then shuts down gracefully.
// It takes ownership of ln.
//
// Shutdown order: optionally drain (no new rooms here, wait for rooms to
// empty), stop accepting HTTP connections, close every WebSocket session
// (http.Server.Shutdown does not track them), close all rooms (closing every
// PeerConnection), write the last usage, send the last webhooks, leave the
// cluster, and close the stores.
func (a *App) Serve(ctx context.Context, ln net.Listener) error {
	if a.cfg.NodeURL == "" {
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		a.cluster.SetURL("http://127.0.0.1:" + port)
	}
	if err := a.cluster.Start(ctx); err != nil {
		_ = ln.Close()
		a.shutdownParts()
		return fmt.Errorf("cluster: %w", err)
	}
	a.logger.Info("server started", "addr", ln.Addr().String(), "node_id", a.cluster.ID())
	defer func() {
		a.shutdownParts()
		a.logger.Info("server stopped")
	}()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- a.server.Serve(ln)
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	if a.cfg.DrainTimeout > 0 {
		a.drain()
	}
	a.logger.Info("server shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
	defer cancel()

	if err := a.server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// drain stops taking new rooms and waits for this node's rooms to empty, up
// to DrainTimeout.
func (a *App) drain() {
	a.cluster.SetDraining(true)
	a.logger.Info("draining: no new rooms on this node", "timeout", a.cfg.DrainTimeout)
	deadline := time.Now().Add(a.cfg.DrainTimeout)
	for time.Now().Before(deadline) && a.rooms.Stats().Participants > 0 {
		time.Sleep(500 * time.Millisecond)
	}
}

// shutdownParts stops everything except the HTTP server, in order. It is
// safe to call once.
func (a *App) shutdownParts() {
	a.signaling.Close()
	a.rooms.Close()
	a.meter.Close()
	a.webhooks.Close()
	a.cluster.Stop()
	a.closeStores()
}

func (a *App) closeStores() {
	if a.ownClStore && a.clStore != nil {
		_ = a.clStore.Close()
	}
	if a.ownStore && a.store != nil {
		a.store.Close()
	}
}

// releaseOnFinish gives up the cluster claim of a room when it finishes.
type releaseOnFinish struct {
	room.NopObserver
	c *cluster.Cluster
}

func (r releaseOnFinish) RoomFinished(roomID string) { r.c.Release(roomID) }

// joinRouting decides where a join is handled: on this node, or on the node
// that owns (or will own) the room.
type joinRouting struct {
	c     *cluster.Cluster
	rooms *room.Manager
}

// errRoomElsewhere means a forwarded join reached a node that does not own
// the room (it moved meanwhile); the client should retry.
var errRoomElsewhere = errors.New("room is on another node")

func (j joinRouting) RouteJoin(ctx context.Context, roomID string, autoCreate, routed bool) (string, error) {
	id, url, err := j.c.Owner(ctx, roomID)
	if err != nil {
		return "", err
	}
	switch {
	case id == j.c.ID():
		if !j.rooms.HasRoom(roomID) && autoCreate {
			// A new copy of the room: claim it again with a new generation,
			// so a late release of the old copy cannot remove this claim.
			if id, url, err = j.c.ClaimSelf(ctx, roomID); err != nil {
				return "", err
			}
			if id != j.c.ID() {
				if routed {
					return "", errRoomElsewhere
				}
				return url, nil
			}
		}
		return "", nil
	case id != "":
		if routed {
			return "", errRoomElsewhere
		}
		return url, nil
	case !autoCreate:
		return "", nil // no such room: the local join reports "room not found"
	}
	// A new room: put it on the least loaded node.
	if !routed {
		n, err := j.c.PickNode()
		if err != nil {
			return "", err
		}
		if n.ID != j.c.ID() {
			return n.URL, nil
		}
	}
	id, url, err = j.c.ClaimSelf(ctx, roomID)
	if err != nil {
		return "", err
	}
	if id == j.c.ID() {
		return "", nil
	}
	if routed {
		return "", errRoomElsewhere
	}
	return url, nil
}

// Release gives up a claim made for a join that then failed.
func (j joinRouting) Release(roomID string) {
	if !j.rooms.HasRoom(roomID) {
		j.c.Release(roomID)
	}
}

// devTokenHandler gives a one-hour join token with all permissions for
// ?room=...&participant=..., signed for the dev tenant. Development only.
func devTokenHandler(dev auth.Tenant) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := token.New(dev.APIKey, r.URL.Query().Get("participant"), r.URL.Query().Get("room"), time.Hour)
		tok, err := token.Sign(dev.Secret, c)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]string{"token": tok})
	})
}
