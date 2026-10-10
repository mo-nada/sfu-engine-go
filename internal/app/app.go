// Package app wires the SFU components together and manages startup and shutdown.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	apidocs "github.com/mo-nada/sfu-engine/api"
	"github.com/mo-nada/sfu-engine/internal/auth"
	"github.com/mo-nada/sfu-engine/internal/config"
	"github.com/mo-nada/sfu-engine/internal/room"
	"github.com/mo-nada/sfu-engine/internal/signaling"
	"github.com/mo-nada/sfu-engine/internal/transport"
	"github.com/mo-nada/sfu-engine/internal/web"
	"github.com/mo-nada/sfu-engine/pkg/token"
)

// App is the running SFU server.
type App struct {
	cfg       config.Config
	logger    *slog.Logger
	server    *http.Server
	rooms     *room.Manager
	signaling *signaling.Handler
}

// New creates an App from a validated config. It wires the Pion API, the
// room manager, the WebSocket signaling handler at /ws, the browser demo
// page at /, and the API documents and their viewers at /api and /docs.
func New(cfg config.Config, logger *slog.Logger) (*App, error) {
	api, err := transport.NewAPI()
	if err != nil {
		return nil, fmt.Errorf("webrtc api: %w", err)
	}
	list := append([]auth.Tenant(nil), cfg.Tenants...)
	if cfg.TenantsFile != "" {
		fromFile, err := auth.LoadTenants(cfg.TenantsFile)
		if err != nil {
			return nil, err
		}
		list = append(list, fromFile...)
	}
	var dev auth.Tenant
	if cfg.Dev {
		dev = auth.DevTenant()
		list = append(list, dev)
		logger.Warn("development mode: /dev/token gives join tokens to anyone; never use in production")
	}
	tenants, err := auth.NewTenants(list)
	if err != nil {
		return nil, fmt.Errorf("tenants: %w", err)
	}
	logger.Info("tenants loaded", "count", tenants.Len())

	rooms := room.NewManager(logger)
	sig := signaling.NewHandler(api, transport.Configuration(cfg.STUNURLs), rooms, tenants, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("GET /ws", sig)
	if cfg.Dev {
		mux.Handle("GET /dev/token", devTokenHandler(dev))
	}
	mux.Handle("GET /{$}", web.Handler())
	mux.Handle("GET /docs", web.Docs())
	mux.Handle("GET /docs/signaling", web.SignalingDocs())
	mux.Handle("GET /api/openapi.yaml", web.File("application/yaml", apidocs.OpenAPI))
	mux.Handle("GET /api/asyncapi.yaml", web.File("application/yaml", apidocs.AsyncAPI))

	return &App{
		cfg:       cfg,
		logger:    logger,
		rooms:     rooms,
		signaling: sig,
		server: &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		},
	}, nil
}

// Run listens on the configured address and serves until ctx is cancelled,
// then shuts down gracefully.
func (a *App) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", a.cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", a.cfg.Addr, err)
	}
	return a.Serve(ctx, ln)
}

// Serve serves on ln until ctx is cancelled, then shuts down gracefully.
// It takes ownership of ln.
//
// Shutdown order: stop accepting HTTP connections, close every WebSocket
// session (http.Server.Shutdown does not track them), then close all rooms,
// which closes every remaining PeerConnection.
func (a *App) Serve(ctx context.Context, ln net.Listener) error {
	a.logger.Info("server started", "addr", ln.Addr().String())
	defer func() {
		a.signaling.Close()
		a.rooms.Close()
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
