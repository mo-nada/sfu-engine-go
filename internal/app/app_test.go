package app

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mo-nada/sfu-engine/internal/config"
	"github.com/mo-nada/sfu-engine/internal/signaling"
)

func TestServeHealthzAndShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	cfg := config.Default()
	cfg.ShutdownTimeout = 2 * time.Second
	a := newApp(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx, ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		cancel()
		t.Fatalf("GET /healthz: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("GET /healthz = %d %q, want 200 \"ok\"", resp.StatusCode, body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}

func TestRunListenError(t *testing.T) {
	cfg := config.Default()
	cfg.Addr = "not-a-valid-address"
	a := newApp(t, cfg)
	if err := a.Run(context.Background()); err == nil {
		t.Error("Run returned nil, want listen error")
	}
}

func newApp(t *testing.T, cfg config.Config) *App {
	t.Helper()
	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// On shutdown, connected signaling clients are disconnected, their rooms are
// closed, and Serve returns without error.
func TestShutdownClosesSignalingAndRooms(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cfg := config.Default()
	cfg.STUNURLs = nil
	cfg.ShutdownTimeout = 2 * time.Second
	a := newApp(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx, ln) }()

	conn, _, err := websocket.DefaultDialer.Dial("ws://"+ln.Addr().String()+"/ws", nil)
	if err != nil {
		t.Fatalf("dial /ws: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(signaling.Message{Type: signaling.TypeJoin, Room: "r1", Participant: "alice"}); err != nil {
		t.Fatalf("write join: %v", err)
	}
	var joined signaling.Message
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.ReadJSON(&joined); err != nil || joined.Type != signaling.TypeJoined {
		t.Fatalf("got %+v (err %v), want joined", joined, err)
	}
	if _, ok := a.rooms.Room("r1"); !ok {
		t.Fatal("room not created")
	}

	cancel()

	// The client must see the connection close.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatal("websocket was not closed on shutdown")
			}
			break
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
	if _, ok := a.rooms.Room("r1"); ok {
		t.Error("room still exists after shutdown")
	}
}

func TestServesPagesAndAPIDocs(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	a := newApp(t, config.Default())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx, ln) }()
	defer func() { cancel(); <-done }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "new WebSocket(") {
		t.Errorf("GET / = %d, body does not look like the demo page", resp.StatusCode)
	}

	for path, want := range map[string]string{
		"/docs":              "SwaggerUIBundle",
		"/docs/signaling":    "AsyncApiStandalone",
		"/api/openapi.yaml":  "openapi: 3.1.0",
		"/api/asyncapi.yaml": "asyncapi: 3.0.0",
	} {
		resp, err := http.Get("http://" + ln.Addr().String() + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
			t.Errorf("GET %s = %d, body does not contain %q", path, resp.StatusCode, want)
		}
	}

	resp, err = http.Get("http://" + ln.Addr().String() + "/missing")
	if err != nil {
		t.Fatalf("GET /missing: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /missing = %d, want 404", resp.StatusCode)
	}
}
