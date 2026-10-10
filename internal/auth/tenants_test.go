package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mo-nada/sfu-engine/pkg/token"
)

const secretA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const secretB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestNewTenantsValidates(t *testing.T) {
	tests := map[string][]Tenant{
		"bad id":       {{ID: "a b", APIKey: "k", Secret: secretA}},
		"dup id":       {{ID: "a", APIKey: "k1", Secret: secretA}, {ID: "a", APIKey: "k2", Secret: secretB}},
		"empty key":    {{ID: "a", Secret: secretA}},
		"dup key":      {{ID: "a", APIKey: "k", Secret: secretA}, {ID: "b", APIKey: "k", Secret: secretB}},
		"short secret": {{ID: "a", APIKey: "k", Secret: "short"}},
		"dup secret":   {{ID: "a", APIKey: "k1", Secret: secretA}, {ID: "b", APIKey: "k2", Secret: secretA}},
	}
	for name, list := range tests {
		if _, err := NewTenants(list); err == nil {
			t.Errorf("%s: NewTenants accepted invalid tenants", name)
		}
	}
}

func TestLoadTenants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tenants.json")
	content := `{"tenants":[{"id":"acme","api_key":"acme-key","secret":"` + secretA + `"}]}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := LoadTenants(path)
	if err != nil {
		t.Fatalf("LoadTenants: %v", err)
	}
	tenants, err := NewTenants(list)
	if err != nil {
		t.Fatalf("NewTenants: %v", err)
	}
	if tn, ok := tenants.ByAPIKey("acme-key"); !ok || tn.ID != "acme" {
		t.Errorf("ByAPIKey = %+v, %v", tn, ok)
	}

	if _, err := LoadTenants(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("missing file accepted")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte("{"), 0o600)
	if _, err := LoadTenants(bad); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("bad JSON: err = %v", err)
	}
}

// A token signed by one tenant identifies that tenant; a token signed with
// another tenant's secret is rejected.
func TestVerifyJoinTokenPerTenant(t *testing.T) {
	tenants, err := NewTenants([]Tenant{
		{ID: "acme", APIKey: "acme-key", Secret: secretA},
		{ID: "globex", APIKey: "globex-key", Secret: secretB},
	})
	if err != nil {
		t.Fatal(err)
	}

	tok, _ := token.Sign(secretB, token.New("globex-key", "alice", "room1", time.Minute))
	tn, c, err := tenants.VerifyJoinToken(tok, time.Now())
	if err != nil || tn.ID != "globex" || c.Subject != "alice" {
		t.Fatalf("got tenant %+v claims %+v err %v", tn, c, err)
	}

	// Claims acme's key but signed with globex's secret.
	forged, _ := token.Sign(secretB, token.New("acme-key", "alice", "room1", time.Minute))
	if _, _, err := tenants.VerifyJoinToken(forged, time.Now()); !errors.Is(err, token.ErrSignature) {
		t.Errorf("forged token: err = %v, want ErrSignature", err)
	}
}

func TestDevTenantIsValid(t *testing.T) {
	a, b := DevTenant(), DevTenant()
	if _, err := NewTenants([]Tenant{a}); err != nil {
		t.Errorf("dev tenant invalid: %v", err)
	}
	if a.Secret == b.Secret {
		t.Error("dev secrets are not random")
	}
}

// Made-up API keys cannot grow memory without limit.
func TestUnknownKeysCacheIsBounded(t *testing.T) {
	tenants, _ := NewTenants(nil)
	for i := range maxUnknown + 50 {
		tok, _ := token.Sign(secretA, token.New(fmt.Sprintf("fake-%d", i), "a", "r", time.Minute))
		if _, _, err := tenants.VerifyJoinToken(tok, time.Now()); err == nil {
			t.Fatal("a made-up key was accepted")
		}
	}
	tenants.mu.Lock()
	n := len(tenants.unknown)
	tenants.mu.Unlock()
	if n > maxUnknown {
		t.Errorf("unknown-key cache has %d entries, more than %d", n, maxUnknown)
	}
}
