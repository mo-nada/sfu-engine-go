// Package observability provides structured logging for the SFU.
package observability

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/lmittmann/tint"
)

// NewLogger builds a slog.Logger writing to w.
// level is one of "debug", "info", "warn", "error"; format is "text" or "json".
//
// Text output is colored only when w is a terminal and the NO_COLOR
// environment variable is not set, so files and pipes stay plain.
func NewLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q: %w", level, err)
	}

	switch format {
	case "text":
		return slog.New(tint.NewTextHandler(w, &tint.Options{
			Level:      lvl,
			TimeFormat: "15:04:05.000",
			NoColor:    !useColor(w),
		})), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})), nil
	default:
		return nil, fmt.Errorf("invalid log format %q", format)
	}
}

// useColor reports whether colored output should be written to w.
// See https://no-color.org for the NO_COLOR convention.
func useColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
