// Package auth holds the tenants and checks join tokens.
//
// Tenants come from a JSON file that every node loads, so all nodes agree.
// (A later stage moves tenants into the shared store.)
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/mo-nada/sfu-engine/pkg/token"
)

// MinSecretLength is the shortest accepted tenant secret, in bytes.
const MinSecretLength = 32

// Tenant is one customer system using the SFU.
type Tenant struct {
	ID     string `json:"id"`
	APIKey string `json:"api_key"`
	Secret string `json:"secret"`
}

// Tenants is an immutable set of tenants, safe for concurrent use.
type Tenants struct {
	byKey map[string]Tenant
}

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// NewTenants validates list: IDs must be valid and unique, API keys unique
// and non-empty, and secrets at least MinSecretLength bytes.
func NewTenants(list []Tenant) (*Tenants, error) {
	t := &Tenants{byKey: make(map[string]Tenant, len(list))}
	ids := make(map[string]bool, len(list))
	var errs []error
	for i, tn := range list {
		switch {
		case !validID.MatchString(tn.ID):
			errs = append(errs, fmt.Errorf("tenant %d: id must be 1-64 letters, digits, '-' or '_'", i))
		case ids[tn.ID]:
			errs = append(errs, fmt.Errorf("tenant %q: duplicate id", tn.ID))
		case tn.APIKey == "":
			errs = append(errs, fmt.Errorf("tenant %q: api_key is empty", tn.ID))
		case t.byKey[tn.APIKey].ID != "":
			errs = append(errs, fmt.Errorf("tenant %q: api_key already used", tn.ID))
		case len(tn.Secret) < MinSecretLength:
			errs = append(errs, fmt.Errorf("tenant %q: secret must be at least %d bytes", tn.ID, MinSecretLength))
		default:
			ids[tn.ID] = true
			t.byKey[tn.APIKey] = tn
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return t, nil
}

// LoadTenants reads tenants from a JSON file: {"tenants": [{"id", "api_key", "secret"}]}.
func LoadTenants(path string) ([]Tenant, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read tenants: %w", err)
	}
	var file struct {
		Tenants []Tenant `json:"tenants"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse tenants %s: %w", path, err)
	}
	return file.Tenants, nil
}

// DevTenant returns a tenant for local development with a random secret,
// which changes on every start.
func DevTenant() Tenant {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails on supported platforms
	return Tenant{ID: "dev", APIKey: "devkey", Secret: hex.EncodeToString(b)}
}

// ByAPIKey returns the tenant with the given API key.
func (t *Tenants) ByAPIKey(key string) (Tenant, bool) {
	tn, ok := t.byKey[key]
	return tn, ok
}

// Len returns the number of tenants.
func (t *Tenants) Len() int { return len(t.byKey) }

// VerifyJoinToken checks tok and returns its tenant and claims.
func (t *Tenants) VerifyJoinToken(tok string, now time.Time) (Tenant, token.Claims, error) {
	c, err := token.Verify(tok, func(key string) (string, bool) {
		tn, ok := t.byKey[key]
		return tn.Secret, ok
	}, now)
	if err != nil {
		return Tenant{}, token.Claims{}, err
	}
	return t.byKey[c.Issuer], c, nil
}
