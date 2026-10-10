package admin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mo-nada/sfu-engine/internal/auth"
	"github.com/mo-nada/sfu-engine/internal/store"
	"github.com/mo-nada/sfu-engine/pkg/token"
)

const adminSecret = "admin-secret-admin-secret-admin-s"

func setup(t *testing.T) (*httptest.Server, *auth.Tenants, store.Store) {
	t.Helper()
	st := store.NewMemory()
	tenants, err := auth.NewTenantsWithStore(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewServer(adminSecret, st, tenants, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	t.Cleanup(srv.Close)
	return srv, tenants, st
}

func call(t *testing.T, srv *httptest.Server, secret, method, path, body string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
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

func TestAdminAuth(t *testing.T) {
	srv, _, _ := setup(t)
	if s := call(t, srv, "wrong", "GET", "/admin/v1/tenants", "", nil); s != http.StatusUnauthorized {
		t.Errorf("wrong secret: %d", s)
	}
	if s := call(t, srv, adminSecret, "GET", "/admin/v1/tenants", "", nil); s != http.StatusOK {
		t.Errorf("right secret: %d", s)
	}
}

func TestTenantLifecycle(t *testing.T) {
	srv, tenants, st := setup(t)

	var created struct {
		Tenant struct {
			ID            string `json:"id"`
			WebhookSecret string `json:"webhook_secret"`
		}
		APIKey    string `json:"api_key"`
		APISecret string `json:"api_secret"`
	}
	body := `{"id":"acme","name":"Acme","webhook_url":"https://acme.example/hook","limits":{"max_rooms":5,"max_room_size":10}}`
	if s := call(t, srv, adminSecret, "POST", "/admin/v1/tenants", body, &created); s != http.StatusCreated {
		t.Fatalf("create: %d", s)
	}
	if created.APIKey == "" || len(created.APISecret) < 32 || !strings.HasPrefix(created.Tenant.WebhookSecret, "whsec_") {
		t.Fatalf("created = %+v", created)
	}
	if s := call(t, srv, adminSecret, "POST", "/admin/v1/tenants", body, nil); s != http.StatusConflict {
		t.Errorf("duplicate: %d", s)
	}

	// The new key works for API tokens at once.
	apiTok, _ := token.SignAPI(created.APISecret, token.NewAPI(created.APIKey, time.Minute))
	tn, err := tenants.VerifyAPIToken(apiTok, time.Now())
	if err != nil || tn.ID != "acme" || tn.Limits.MaxRooms != 5 {
		t.Fatalf("VerifyAPIToken = %+v, %v", tn, err)
	}

	// Rotate: a second key, then revoke the first.
	var second struct {
		APIKey    string `json:"api_key"`
		APISecret string `json:"api_secret"`
	}
	call(t, srv, adminSecret, "POST", "/admin/v1/tenants/acme/keys", "", &second)
	if s := call(t, srv, adminSecret, "DELETE", "/admin/v1/tenants/acme/keys/"+created.APIKey, "", nil); s != http.StatusNoContent {
		t.Fatalf("revoke: %d", s)
	}
	if _, err := tenants.VerifyAPIToken(apiTok, time.Now()); err == nil {
		t.Error("a revoked key still works")
	}
	tok2, _ := token.SignAPI(second.APISecret, token.NewAPI(second.APIKey, time.Minute))
	if _, err := tenants.VerifyAPIToken(tok2, time.Now()); err != nil {
		t.Errorf("the new key does not work: %v", err)
	}
	var keys struct{ Keys []map[string]any }
	call(t, srv, adminSecret, "GET", "/admin/v1/tenants/acme/keys", "", &keys)
	if len(keys.Keys) != 2 {
		t.Errorf("keys = %+v", keys)
	}
	for _, k := range keys.Keys {
		if _, ok := k["secret"]; ok {
			t.Error("a listed key shows its secret")
		}
	}

	// Disable: the tenant's tokens stop working.
	if s := call(t, srv, adminSecret, "PATCH", "/admin/v1/tenants/acme", `{"disabled":true}`, nil); s != http.StatusOK {
		t.Fatalf("disable: %d", s)
	}
	if _, err := tenants.VerifyAPIToken(tok2, time.Now()); err == nil {
		t.Error("a disabled tenant's token still works")
	}

	// Usage report.
	_ = st.AddUsage(context.Background(), "acme", time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC), 600)
	var usage struct {
		Days  []struct{ Day string }
		Total float64 `json:"total_participant_minutes"`
	}
	if s := call(t, srv, adminSecret, "GET", "/admin/v1/tenants/acme/usage?from=2026-05-01&to=2026-05-31", "", &usage); s != http.StatusOK {
		t.Fatalf("usage: %d", s)
	}
	if usage.Total != 10 || len(usage.Days) != 1 || usage.Days[0].Day != "2026-05-03" {
		t.Errorf("usage = %+v", usage)
	}
}

func TestValidation(t *testing.T) {
	srv, _, _ := setup(t)
	for name, body := range map[string]string{
		"bad id":          `{"id":"a b"}`,
		"bad webhook":     `{"id":"a","webhook_url":"ftp://x"}`,
		"negative limits": `{"id":"a","limits":{"max_rooms":-1}}`,
		"unknown field":   `{"id":"a","plan":"gold"}`,
	} {
		if s := call(t, srv, adminSecret, "POST", "/admin/v1/tenants", body, nil); s != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, s)
		}
	}
	for _, path := range []string{"/admin/v1/tenants/missing", "/admin/v1/tenants/missing/keys", "/admin/v1/tenants/missing/usage"} {
		if s := call(t, srv, adminSecret, "GET", path, "", nil); s != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, s)
		}
	}
	if s := call(t, srv, adminSecret, "GET", "/admin/v1/tenants/x/usage?from=yesterday", "", nil); s != http.StatusNotFound && s != http.StatusBadRequest {
		t.Errorf("bad date: %d", s)
	}
}
