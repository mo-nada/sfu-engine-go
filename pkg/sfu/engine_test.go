package sfu

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestNewRejectsInvalidOptions(t *testing.T) {
	tests := map[string]Option{
		"empty addr":       WithAddr(""),
		"bad stun url":     WithSTUN("http://example.com"),
		"zero shutdown":    WithShutdownTimeout(0),
		"negative timeout": WithShutdownTimeout(-time.Second),
	}
	for name, opt := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := New(opt); err == nil {
				t.Error("New returned nil error")
			}
		})
	}
}

func TestServeAndShutdown(t *testing.T) {
	eng, err := New(
		WithSTUN(),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithShutdownTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.Serve(ctx, ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		cancel()
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}
