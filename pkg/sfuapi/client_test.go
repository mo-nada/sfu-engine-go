package sfuapi_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mo-nada/sfu-engine/pkg/sfu"
	"github.com/mo-nada/sfu-engine/pkg/sfuapi"
	"github.com/mo-nada/sfu-engine/pkg/token"
)

const adminSecret = "go-sdk-test-admin-secret-0123456789"

// startEngine runs a real engine (memory stores) with the admin API.
func startEngine(t *testing.T) string {
	t.Helper()
	eng, err := sfu.New(sfu.WithAdminSecret(adminSecret), sfu.WithSTUN(),
		sfu.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))), sfu.WithShutdownTimeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.Serve(ctx, ln) }()
	t.Cleanup(func() { http.DefaultClient.CloseIdleConnections(); cancel(); <-done })
	return "http://" + ln.Addr().String()
}

func TestGoSDKAgainstRealServer(t *testing.T) {
	ctx := context.Background()
	host := startEngine(t)

	var mu sync.Mutex
	var events []string
	hook := httptest.NewServer(nil)
	defer hook.Close()

	admin := sfuapi.NewAdmin(host, adminSecret)
	created, err := admin.CreateTenant(ctx, sfuapi.NewTenant{ID: "acme", WebhookURL: hook.URL, Limits: sfuapi.Limits{MaxRooms: 2}})
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	hook.Config.Handler = sfuapi.WebhookHandler(created.Tenant.WebhookSecret, func(ev sfuapi.WebhookEvent) {
		mu.Lock()
		events = append(events, ev.Event)
		mu.Unlock()
	})

	rooms := sfuapi.NewRoomService(host, created.APIKey, created.APISecret)
	if _, err := rooms.CreateRoom(ctx, sfuapi.CreateRoom{Name: "standup", MaxParticipants: 5}); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	_, err = rooms.CreateRoom(ctx, sfuapi.CreateRoom{Name: "standup"})
	var apiErr *sfuapi.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "conflict" {
		t.Errorf("duplicate room: err = %v, want conflict", err)
	}

	// Join over signaling with a join token from package token.
	tok, _ := token.Sign(created.APISecret, token.New(created.APIKey, "alice", "standup", time.Minute))
	ws, _, err := websocket.DefaultDialer.Dial("ws"+host[4:]+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	_ = ws.WriteJSON(map[string]string{"type": "join", "token": tok})
	var joined struct{ Type string }
	_ = ws.ReadJSON(&joined)
	if joined.Type != "joined" {
		t.Fatalf("join: %+v", joined)
	}

	parts, err := rooms.ListParticipants(ctx, "standup")
	if err != nil || len(parts) != 1 || parts[0].Identity != "alice" {
		t.Fatalf("ListParticipants = %+v, %v", parts, err)
	}
	if err := rooms.MutePublishedTrack(ctx, "standup", "alice", "microphone", true); err != nil {
		t.Errorf("Mute: %v", err)
	}
	f := false
	p, err := rooms.UpdateParticipant(ctx, "standup", "alice", sfuapi.GrantsUpdate{Subscribe: &f})
	if err != nil || p.Grants.Subscribe {
		t.Errorf("UpdateParticipant = %+v, %v", p.Grants, err)
	}
	if err := rooms.RemoveParticipant(ctx, "standup", "alice"); err != nil {
		t.Errorf("Remove: %v", err)
	}
	if err := rooms.DeleteRoom(ctx, "standup"); err != nil {
		t.Errorf("DeleteRoom: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := append([]string(nil), events...)
		mu.Unlock()
		if len(got) >= 4 || time.Now().After(deadline) {
			want := map[string]bool{"room_started": true, "participant_joined": true, "participant_left": true, "room_finished": true}
			for _, e := range got {
				delete(want, e)
			}
			if len(want) != 0 {
				t.Errorf("webhooks %v, missing %v", got, want)
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if u, err := admin.Usage(ctx, "acme", "", ""); err != nil || u.TenantID != "acme" {
		t.Errorf("Usage = %+v, %v", u, err)
	}
}

func TestVerifyWebhookRejects(t *testing.T) {
	if _, err := sfuapi.VerifyWebhook("s", []byte("{}"), "garbage", time.Minute); !errors.Is(err, sfuapi.ErrBadWebhook) {
		t.Errorf("garbage: %v", err)
	}
	if _, err := sfuapi.VerifyWebhook("s", []byte("{}"), "t=1,v1=00", time.Minute); !errors.Is(err, sfuapi.ErrBadWebhook) {
		t.Errorf("old: %v", err)
	}
}
