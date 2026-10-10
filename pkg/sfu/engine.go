// Package sfu is the public Go API for embedding the SFU media engine.
//
// An Engine serves WebSocket signaling at /ws, a health check at /healthz and
// a browser demo page at /. Clients join rooms over the signaling protocol
// (see the README) with a join token signed by a tenant (see package
// token); every published track is forwarded to all other participants in
// the same room. At least one tenant (or dev mode) is required.
//
//	eng, err := sfu.New(sfu.WithAddr(":8080"), sfu.WithTenantsFile("tenants.json"))
//	if err != nil { ... }
//	err = eng.Run(ctx) // blocks until ctx is cancelled, then shuts down
package sfu

import (
	"context"
	"log/slog"
	"net"

	"github.com/mo-nada/sfu-engine/internal/app"
	"github.com/mo-nada/sfu-engine/internal/auth"
	"github.com/mo-nada/sfu-engine/internal/config"
)

// Engine is an SFU server. An Engine runs once: after Run or Serve returns,
// create a new Engine to serve again.
type Engine struct {
	app *app.App
}

// New creates an Engine with the given options.
func New(opts ...Option) (*Engine, error) {
	cfg := config.Default()
	o := options{
		addr:            cfg.Addr,
		stunURLs:        cfg.STUNURLs,
		logger:          slog.Default(),
		shutdownTimeout: cfg.ShutdownTimeout,
	}
	for _, opt := range opts {
		opt(&o)
	}
	if o.logger == nil {
		o.logger = slog.Default()
	}

	cfg.Addr = o.addr
	cfg.STUNURLs = o.stunURLs
	cfg.ShutdownTimeout = o.shutdownTimeout
	cfg.TenantsFile = o.tenantsFile
	cfg.Dev = o.dev
	for _, t := range o.tenants {
		cfg.Tenants = append(cfg.Tenants, auth.Tenant{ID: t.ID, APIKey: t.APIKey, Secret: t.Secret})
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	a, err := app.New(cfg, o.logger)
	if err != nil {
		return nil, err
	}
	return &Engine{app: a}, nil
}

// Run listens on the configured address and serves until ctx is cancelled,
// then shuts down gracefully: signaling connections, rooms and
// PeerConnections are all closed before it returns.
func (e *Engine) Run(ctx context.Context) error {
	return e.app.Run(ctx)
}

// Serve is like Run but uses an existing listener, for example one on a
// random port in tests. It takes ownership of ln.
func (e *Engine) Serve(ctx context.Context, ln net.Listener) error {
	return e.app.Serve(ctx, ln)
}
