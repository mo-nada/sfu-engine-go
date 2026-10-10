// Package auth checks join tokens and API tokens against the tenants and API
// keys in the store. Lookups are cached for a few seconds, so a change made
// through the admin API (a revoked key, a disabled tenant, new limits) takes
// effect on every node within CacheTTL.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/mo-nada/sfu-engine/internal/store"
	"github.com/mo-nada/sfu-engine/pkg/token"
)

// MinSecretLength is the shortest accepted secret, in bytes.
const MinSecretLength = 32

// CacheTTL is how long a key lookup is reused before asking the store again.
const CacheTTL = 10 * time.Second

// Tenant is a tenant as seen through one of its API keys. It is also the
// format of a tenants file entry, used to seed the store.
type Tenant struct {
	ID     string `json:"id"`
	APIKey string `json:"api_key"`
	Secret string `json:"secret"`
	// AutoCreateRooms lets a join create a room that does not exist. When
	// false (the default), rooms must be created through the server API
	// first, so their limits always apply and a closed room stays closed.
	AutoCreateRooms bool         `json:"auto_create_rooms"`
	Limits          store.Limits `json:"limits"`
	WebhookURL      string       `json:"webhook_url"`
	WebhookSecret   string       `json:"webhook_secret"`
}

// ErrTenantDisabled is returned for tokens of a disabled tenant.
var ErrTenantDisabled = errors.New("auth: tenant disabled")

// Tenants authenticates tokens against the store. It is safe for concurrent use.
type Tenants struct {
	store store.Store

	mu      sync.Mutex
	cache   map[string]cached    // known API key -> lookup result
	unknown map[string]time.Time // unknown API key -> when looked up (bounded)
}

// maxUnknown bounds the cache of unknown API keys, which anyone can fill with
// made-up keys; when full, it starts over.
const maxUnknown = 10000

type cached struct {
	tenant Tenant
	err    error
	at     time.Time
}

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// NewTenants returns Tenants backed by a new memory store seeded with list.
func NewTenants(list []Tenant) (*Tenants, error) {
	return NewTenantsWithStore(context.Background(), store.NewMemory(), list)
}

// NewTenantsWithStore returns Tenants backed by s, after adding the seed
// tenants and keys that are not in s yet. Seeds are validated: IDs must be
// valid and unique, API keys unique and non-empty, and secrets unique and at
// least MinSecretLength bytes.
func NewTenantsWithStore(ctx context.Context, s store.Store, seed []Tenant) (*Tenants, error) {
	if err := validate(seed); err != nil {
		return nil, err
	}
	for _, tn := range seed {
		err := s.CreateTenant(ctx, store.Tenant{
			ID: tn.ID, Name: tn.ID, AutoCreateRooms: tn.AutoCreateRooms, Limits: tn.Limits,
			WebhookURL: tn.WebhookURL, WebhookSecret: tn.WebhookSecret, CreatedAt: time.Now(),
		})
		if err != nil && !errors.Is(err, store.ErrExists) {
			return nil, fmt.Errorf("seed tenant %q: %w", tn.ID, err)
		}
		err = s.CreateAPIKey(ctx, store.APIKey{Key: tn.APIKey, Secret: tn.Secret, TenantID: tn.ID, CreatedAt: time.Now()})
		if err != nil && !errors.Is(err, store.ErrExists) {
			return nil, fmt.Errorf("seed key of %q: %w", tn.ID, err)
		}
	}
	return &Tenants{store: s, cache: make(map[string]cached), unknown: make(map[string]time.Time)}, nil
}

func validate(list []Tenant) error {
	ids := make(map[string]bool, len(list))
	keys := make(map[string]bool, len(list))
	secrets := make(map[string]bool, len(list))
	var errs []error
	for i, tn := range list {
		switch {
		case !validID.MatchString(tn.ID):
			errs = append(errs, fmt.Errorf("tenant %d: id must be 1-64 letters, digits, '-' or '_'", i))
		case ids[tn.ID]:
			errs = append(errs, fmt.Errorf("tenant %q: duplicate id", tn.ID))
		case tn.APIKey == "":
			errs = append(errs, fmt.Errorf("tenant %q: api_key is empty", tn.ID))
		case keys[tn.APIKey]:
			errs = append(errs, fmt.Errorf("tenant %q: api_key already used", tn.ID))
		case len(tn.Secret) < MinSecretLength:
			errs = append(errs, fmt.Errorf("tenant %q: secret must be at least %d bytes", tn.ID, MinSecretLength))
		case secrets[tn.Secret]:
			// A shared secret would let each tenant sign tokens as the other.
			errs = append(errs, fmt.Errorf("tenant %q: secret already used by another tenant", tn.ID))
		default:
			ids[tn.ID], keys[tn.APIKey], secrets[tn.Secret] = true, true, true
		}
	}
	return errors.Join(errs...)
}

// LoadTenants reads tenants from a JSON file:
// {"tenants": [{"id", "api_key", "secret", "auto_create_rooms", "limits", "webhook_url", "webhook_secret"}]}.
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

// NewSecret returns 32 random bytes, hex encoded.
func NewSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails on supported platforms
	return hex.EncodeToString(b)
}

// DevTenant returns a tenant for local development with a random secret,
// which changes on every start.
func DevTenant() Tenant {
	return Tenant{ID: "dev", APIKey: "devkey", Secret: NewSecret(), AutoCreateRooms: true}
}

// Store returns the store behind t.
func (t *Tenants) Store() store.Store { return t.store }

// ByAPIKey returns the active tenant of an active API key.
func (t *Tenants) ByAPIKey(key string) (Tenant, bool) {
	tn, err := t.lookup(key)
	return tn, err == nil
}

// Forget drops this node's cached lookups, for example after an admin change.
// Other nodes see the change within CacheTTL.
func (t *Tenants) Forget() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cache = make(map[string]cached)
	t.unknown = make(map[string]time.Time)
}

func (t *Tenants) lookup(key string) (Tenant, error) {
	t.mu.Lock()
	c, ok := t.cache[key]
	at, isUnknown := t.unknown[key]
	t.mu.Unlock()
	if ok && time.Since(c.at) < CacheTTL {
		return c.tenant, c.err
	}
	if isUnknown && time.Since(at) < CacheTTL {
		return Tenant{}, token.ErrUnknownIssuer
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	k, st, err := t.store.LookupAPIKey(ctx, key)
	var tn Tenant
	switch {
	case errors.Is(err, store.ErrNotFound):
		t.mu.Lock()
		if len(t.unknown) >= maxUnknown {
			t.unknown = make(map[string]time.Time)
		}
		t.unknown[key] = time.Now()
		delete(t.cache, key) // a revoked key
		t.mu.Unlock()
		return Tenant{}, token.ErrUnknownIssuer
	case err != nil:
		return Tenant{}, err // store unavailable: do not cache
	case st.Disabled:
		err = ErrTenantDisabled
	default:
		tn = Tenant{
			ID: st.ID, APIKey: k.Key, Secret: k.Secret, AutoCreateRooms: st.AutoCreateRooms,
			Limits: st.Limits, WebhookURL: st.WebhookURL, WebhookSecret: st.WebhookSecret,
		}
	}
	t.mu.Lock()
	t.cache[key] = cached{tenant: tn, err: err, at: time.Now()}
	t.mu.Unlock()
	return tn, err
}

// secretFor returns the secret of an active key of an active tenant.
func (t *Tenants) secretFor(key string) (string, bool) {
	tn, err := t.lookup(key)
	return tn.Secret, err == nil
}

// VerifyJoinToken checks tok and returns its tenant and claims.
func (t *Tenants) VerifyJoinToken(tok string, now time.Time) (Tenant, token.Claims, error) {
	c, err := token.Verify(tok, t.secretFor, now)
	if err != nil {
		return Tenant{}, token.Claims{}, err
	}
	tn, err := t.lookup(c.Issuer)
	return tn, c, err
}

// VerifyAPIToken checks an API token (see token.VerifyAPI) and returns its tenant.
func (t *Tenants) VerifyAPIToken(tok string, now time.Time) (Tenant, error) {
	c, err := token.VerifyAPI(tok, t.secretFor, now)
	if err != nil {
		return Tenant{}, err
	}
	return t.lookup(c.Issuer)
}
