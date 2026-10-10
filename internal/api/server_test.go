package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mo-nada/sfu-engine/internal/auth"
	"github.com/mo-nada/sfu-engine/internal/room"
	"github.com/mo-nada/sfu-engine/pkg/token"
)

var (
	acme   = auth.Tenant{ID: "acme", APIKey: "acme-key", Secret: "acme-secret-acme-secret-acme-sec"}
	globex = auth.Tenant{ID: "globex", APIKey: "globex-key", Secret: "globex-secret-globex-secret-glob"}
)

type fixture struct {
	srv   *httptest.Server
	rooms *room.Manager
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	tenants, err := auth.NewTenants([]auth.Tenant{acme, globex})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rooms := room.NewManager(logger)
	srv := httptest.NewServer(NewServer(tenants, LocalRooms{Manager: rooms}, logger).Handler())
	t.Cleanup(func() { srv.Close(); rooms.Close() })
	return fixture{srv: srv, rooms: rooms}
}

func mustAPI(t *testing.T, secret, key string) string {
	t.Helper()
	tok, err := token.SignAPI(secret, token.NewAPI(key, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// do sends a request as tenant tn and decodes the JSON response into out (if not nil).
func (f fixture) do(t *testing.T, tn auth.Tenant, method, path, body string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+mustAPI(t, tn.Secret, tn.APIKey))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func TestAuthentication(t *testing.T) {
	f := newFixture(t)
	get := func(header string) (int, string) {
		req, _ := http.NewRequest("GET", f.srv.URL+"/v1/rooms", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var e struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return resp.StatusCode, e.Error.Code
	}
	join, _ := token.Sign(acme.Secret, token.New(acme.APIKey, "alice", "r1", time.Minute))
	for name, header := range map[string]string{
		"missing":      "",
		"not bearer":   "Basic abc",
		"garbage":      "Bearer abc",
		"join token":   "Bearer " + join,
		"wrong secret": "Bearer " + mustAPI(t, globex.Secret, acme.APIKey),
	} {
		if status, code := get(header); status != http.StatusUnauthorized || code != "unauthorized" {
			t.Errorf("%s: got %d %q, want 401 unauthorized", name, status, code)
		}
	}
	if status, _ := get("Bearer " + mustAPI(t, acme.Secret, acme.APIKey)); status != http.StatusOK {
		t.Errorf("valid token: status %d", status)
	}
}

func TestRoomLifecycle(t *testing.T) {
	f := newFixture(t)

	var created roomJSON
	if s := f.do(t, acme, "POST", "/v1/rooms", `{"name":"standup","max_participants":3,"empty_timeout_seconds":60}`, &created); s != http.StatusCreated {
		t.Fatalf("create: status %d", s)
	}
	if created.Name != "standup" || created.MaxParticipants != 3 || created.EmptyTimeoutSeconds != 60 {
		t.Errorf("created = %+v", created)
	}
	if s := f.do(t, acme, "POST", "/v1/rooms", `{"name":"standup"}`, nil); s != http.StatusConflict {
		t.Errorf("duplicate create: status %d, want 409", s)
	}

	// A participant joins through the room layer directly; no media is needed here.
	_, _ = f.rooms.Join("acme/standup", room.NewParticipant("alice", nil, nil, nil))
	_ = f.rooms.SetMuted("acme/standup", "alice", "microphone", true)

	var list struct{ Rooms []roomJSON }
	f.do(t, acme, "GET", "/v1/rooms", "", &list)
	if len(list.Rooms) != 1 || list.Rooms[0].NumParticipants != 1 {
		t.Errorf("list = %+v", list)
	}

	var got roomJSON
	if s := f.do(t, acme, "GET", "/v1/rooms/standup", "", &got); s != http.StatusOK {
		t.Fatalf("get: status %d", s)
	}
	if len(got.Participants) != 1 || got.Participants[0].Identity != "alice" || !got.Participants[0].Muted["microphone"] {
		t.Errorf("get = %+v", got)
	}

	var parts struct{ Participants []participantJSON }
	f.do(t, acme, "GET", "/v1/rooms/standup/participants", "", &parts)
	if len(parts.Participants) != 1 {
		t.Errorf("participants = %+v", parts)
	}

	if s := f.do(t, acme, "DELETE", "/v1/rooms/standup/participants/alice", "", nil); s != http.StatusNoContent {
		t.Errorf("remove: status %d", s)
	}
	if s := f.do(t, acme, "DELETE", "/v1/rooms/standup/participants/alice", "", nil); s != http.StatusNotFound {
		t.Errorf("remove again: status %d, want 404", s)
	}
	if s := f.do(t, acme, "DELETE", "/v1/rooms/standup", "", nil); s != http.StatusNoContent {
		t.Errorf("delete: status %d", s)
	}
	if s := f.do(t, acme, "GET", "/v1/rooms/standup", "", nil); s != http.StatusNotFound {
		t.Errorf("get deleted: status %d, want 404", s)
	}
}

// Tenants never see each other's rooms: 404, the same as a missing room.
func TestTenantIsolation(t *testing.T) {
	f := newFixture(t)
	f.do(t, acme, "POST", "/v1/rooms", `{"name":"secret-room"}`, nil)

	var list struct{ Rooms []roomJSON }
	f.do(t, globex, "GET", "/v1/rooms", "", &list)
	if len(list.Rooms) != 0 {
		t.Errorf("globex sees %d rooms, want 0", len(list.Rooms))
	}
	for _, req := range [][2]string{
		{"GET", "/v1/rooms/secret-room"},
		{"GET", "/v1/rooms/secret-room/participants"},
		{"DELETE", "/v1/rooms/secret-room"},
	} {
		if s := f.do(t, globex, req[0], req[1], "", nil); s != http.StatusNotFound {
			t.Errorf("globex %s %s: status %d, want 404", req[0], req[1], s)
		}
	}
	if _, err := f.rooms.Summary("acme/secret-room"); err != nil {
		t.Error("acme's room was affected by globex")
	}
}

func TestValidation(t *testing.T) {
	f := newFixture(t)
	for name, body := range map[string]string{
		"bad name":      `{"name":"a b"}`,
		"negative max":  `{"name":"r","max_participants":-1}`,
		"zero timeout":  `{"name":"r","empty_timeout_seconds":0}`,
		"huge timeout":  `{"name":"r","empty_timeout_seconds":9223372036}`,
		"unknown field": `{"name":"r","colour":"blue"}`,
		"not json":      `{`,
	} {
		if s := f.do(t, acme, "POST", "/v1/rooms", body, nil); s != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, s)
		}
	}
	if s := f.do(t, acme, "GET", "/v1/rooms/bad%20name", "", nil); s != http.StatusBadRequest {
		t.Errorf("bad room in path: status %d, want 400", s)
	}
	if s := f.do(t, acme, "GET", "/v1/nothing", "", nil); s != http.StatusNotFound {
		t.Errorf("unknown endpoint: status %d, want 404", s)
	}
}

func TestCreateToken(t *testing.T) {
	f := newFixture(t)
	var resp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	body := `{"room":"standup","identity":"bob","ttl_seconds":600,"grants":{"publish":false,"sources":["camera"]}}`
	if s := f.do(t, acme, "POST", "/v1/tokens", body, &resp); s != http.StatusCreated {
		t.Fatalf("status %d", s)
	}
	c, err := token.Verify(resp.Token, func(k string) (string, bool) { return acme.Secret, k == acme.APIKey }, time.Now())
	if err != nil {
		t.Fatalf("token does not verify: %v", err)
	}
	if c.Subject != "bob" || c.Room != "standup" || c.Grants.Publish || !c.Grants.Subscribe || len(c.Grants.Sources) != 1 {
		t.Errorf("claims = %+v", c)
	}
	if d := time.Until(resp.ExpiresAt); d < 9*time.Minute || d > 11*time.Minute {
		t.Errorf("expires_at %v is not about 10 minutes ahead", resp.ExpiresAt)
	}

	for name, body := range map[string]string{
		"bad identity": `{"room":"r","identity":"a b"}`,
		"bad source":   `{"room":"r","identity":"a","grants":{"sources":["hologram"]}}`,
		"long ttl":     `{"room":"r","identity":"a","ttl_seconds":999999}`,
		"huge ttl":     `{"room":"r","identity":"a","ttl_seconds":9223372036}`,
	} {
		if s := f.do(t, acme, "POST", "/v1/tokens", body, nil); s != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, s)
		}
	}
}

func TestServerMuteAndPermissions(t *testing.T) {
	f := newFixture(t)
	f.do(t, acme, "POST", "/v1/rooms", `{"name":"r1"}`, nil)
	_, _ = f.rooms.Join("acme/r1", room.NewParticipant("alice", nil, nil, nil))

	if s := f.do(t, acme, "POST", "/v1/rooms/r1/participants/alice/mute", `{"source":"microphone","muted":true}`, nil); s != http.StatusNoContent {
		t.Fatalf("mute: status %d", s)
	}
	// The client cannot unmute a server mute.
	if err := f.rooms.SetMuted("acme/r1", "alice", "microphone", false); err == nil {
		t.Error("client unmuted a server mute")
	}

	var p participantJSON
	if s := f.do(t, acme, "PATCH", "/v1/rooms/r1/participants/alice", `{"grants":{"sources":["camera"]}}`, &p); s != http.StatusOK {
		t.Fatalf("patch: status %d", s)
	}
	// Only sources changed; publish and subscribe kept their values.
	if !p.Grants.Publish || !p.Grants.Subscribe || len(p.Grants.Sources) != 1 || p.Grants.Sources[0] != "camera" {
		t.Errorf("grants = %+v", p.Grants)
	}
	if !p.Muted["microphone"] {
		t.Errorf("muted = %+v, want microphone muted", p.Muted)
	}

	for name, req := range map[string][3]string{
		"bad source":    {"POST", "/v1/rooms/r1/participants/alice/mute", `{"source":"hologram","muted":true}`},
		"missing muted": {"POST", "/v1/rooms/r1/participants/alice/mute", `{"source":"camera"}`},
		"bad grant":     {"PATCH", "/v1/rooms/r1/participants/alice", `{"grants":{"sources":["x"]}}`},
		"unknown field": {"PATCH", "/v1/rooms/r1/participants/alice", `{"role":"admin"}`},
	} {
		if s := f.do(t, acme, req[0], req[1], req[2], nil); s != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, s)
		}
	}
	for name, req := range map[string][3]string{
		"mute unknown":  {"POST", "/v1/rooms/r1/participants/bob/mute", `{"source":"camera","muted":true}`},
		"patch unknown": {"PATCH", "/v1/rooms/r1/participants/bob", `{"grants":{}}`},
		"other room":    {"PATCH", "/v1/rooms/nope/participants/alice", `{"grants":{}}`},
	} {
		if s := f.do(t, acme, req[0], req[1], req[2], nil); s != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", name, s)
		}
	}
	// Another tenant cannot touch acme's participant.
	if s := f.do(t, globex, "POST", "/v1/rooms/r1/participants/alice/mute", `{"source":"camera","muted":true}`, nil); s != http.StatusNotFound {
		t.Errorf("globex mute: status %d, want 404", s)
	}
}

// Two PATCH requests that change different fields at the same time both take effect.
func TestConcurrentPatchesKeepBothChanges(t *testing.T) {
	f := newFixture(t)
	f.do(t, acme, "POST", "/v1/rooms", `{"name":"r1"}`, nil)
	for range 20 {
		_, _ = f.rooms.Join("acme/r1", room.NewParticipant("alice", nil, nil, nil))
		done := make(chan struct{}, 2)
		go func() {
			f.do(t, acme, "PATCH", "/v1/rooms/r1/participants/alice", `{"grants":{"subscribe":false}}`, nil)
			done <- struct{}{}
		}()
		go func() {
			f.do(t, acme, "PATCH", "/v1/rooms/r1/participants/alice", `{"grants":{"sources":["camera"]}}`, nil)
			done <- struct{}{}
		}()
		<-done
		<-done
		d, _ := f.rooms.Details("acme/r1")
		if g := d[0].Grants; g.Subscribe || len(g.Sources) != 1 {
			t.Fatalf("grants = %+v, want subscribe=false and sources=[camera]", g)
		}
		f.rooms.Leave("acme/r1", "alice")
	}
}
