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
