package sfu

import (
	"log/slog"
	"time"
)

// Option configures an Engine.
type Option func(*options)

type options struct {
	addr            string
	stunURLs        []string
	logger          *slog.Logger
	shutdownTimeout time.Duration
	tenantsFile     string
	tenants         []Tenant
	dev             bool
}

// Tenant is a customer system allowed to use the engine. Its backend signs
// join tokens (see package token) with APIKey as issuer and Secret as key.
type Tenant struct {
	ID     string // 1–64 letters, digits, '-' or '_'
	APIKey string
	Secret string // at least 32 bytes
	// AutoCreateRooms lets a join create a missing room. When false, rooms
	// must be created through the server API first.
	AutoCreateRooms bool
}

// WithTenant adds a tenant. Can be used several times.
func WithTenant(t Tenant) Option {
	return func(o *options) { o.tenants = append(o.tenants, t) }
}

// WithTenantsFile loads tenants from a JSON file:
// {"tenants": [{"id": "...", "api_key": "...", "secret": "..."}]}.
func WithTenantsFile(path string) Option {
	return func(o *options) { o.tenantsFile = path }
}

// WithDevMode adds a development tenant with a random secret and serves
// GET /dev/token, which gives a join token to anyone. Never use in production.
func WithDevMode() Option {
	return func(o *options) { o.dev = true }
}

// WithAddr sets the HTTP listen address used by Run. Default ":8080".
func WithAddr(addr string) Option {
	return func(o *options) { o.addr = addr }
}

// WithSTUN sets the STUN server URLs used for ICE, for example
// "stun:stun.l.google.com:19302". Calling it with no URLs disables STUN, so
// only host candidates are used. Default: Google's public STUN server.
func WithSTUN(urls ...string) Option {
	return func(o *options) { o.stunURLs = urls }
}

// WithLogger sets the structured logger. Default: slog.Default().
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) { o.logger = logger }
}

// WithShutdownTimeout bounds how long a graceful shutdown may take. Default 10s.
func WithShutdownTimeout(d time.Duration) Option {
	return func(o *options) { o.shutdownTimeout = d }
}
