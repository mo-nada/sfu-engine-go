// Package observability provides structured logging for the SFU.
package observability

import (
	"fmt"
	"io"
	"log/slog"
)

// NewLogger builds a slog.Logger writing to w.
// level is one of "debug", "info", "warn", "error"; format is "text" or "json".
func NewLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q: %w", level, err)
	}

	opts := &slog.HandlerOptions{Level: lvl}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("invalid log format %q", format)
	}
}
