package store

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Memory is an in-memory Store for development and tests. Data is lost on
// restart, and it is not shared between processes.
type Memory struct {
	mu      sync.Mutex
	tenants map[string]Tenant
	keys    map[string]APIKey
	usage   map[string]map[time.Time]int64 // tenant -> day -> seconds
}

// NewMemory returns an empty Memory store.
func NewMemory() *Memory {
	return &Memory{
		tenants: make(map[string]Tenant),
		keys:    make(map[string]APIKey),
		usage:   make(map[string]map[time.Time]int64),
	}
}

func (m *Memory) CreateTenant(_ context.Context, t Tenant) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tenants[t.ID]; ok {
		return ErrExists
	}
	m.tenants[t.ID] = t
	return nil
}

func (m *Memory) UpdateTenant(_ context.Context, t Tenant) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.tenants[t.ID]
	if !ok {
		return ErrNotFound
	}
	t.CreatedAt = old.CreatedAt
	if t.WebhookSecret == "" {
		t.WebhookSecret = old.WebhookSecret
	}
	m.tenants[t.ID] = t
	return nil
}

func (m *Memory) GetTenant(_ context.Context, id string) (Tenant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tenants[id]
	if !ok {
		return Tenant{}, ErrNotFound
	}
	return t, nil
}

func (m *Memory) ListTenants(_ context.Context) ([]Tenant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Tenant, 0, len(m.tenants))
	for _, t := range m.tenants {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Memory) CreateAPIKey(_ context.Context, k APIKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tenants[k.TenantID]; !ok {
		return ErrNotFound
	}
	if _, ok := m.keys[k.Key]; ok {
		return ErrExists
	}
	m.keys[k.Key] = k
	return nil
}

func (m *Memory) ListAPIKeys(_ context.Context, tenantID string) ([]APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []APIKey
	for _, k := range m.keys {
		if k.TenantID == tenantID {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) RevokeAPIKey(_ context.Context, tenantID, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[key]
	if !ok || k.TenantID != tenantID {
		return ErrNotFound
	}
	k.Revoked = true
	m.keys[key] = k
	return nil
}

func (m *Memory) LookupAPIKey(_ context.Context, key string) (APIKey, Tenant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[key]
	if !ok || k.Revoked {
		return APIKey{}, Tenant{}, ErrNotFound
	}
	t, ok := m.tenants[k.TenantID]
	if !ok {
		return APIKey{}, Tenant{}, ErrNotFound
	}
	return k, t, nil
}

func (m *Memory) AddUsage(_ context.Context, tenantID string, day time.Time, seconds int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.usage[tenantID] == nil {
		m.usage[tenantID] = make(map[time.Time]int64)
	}
	m.usage[tenantID][Day(day)] += seconds
	return nil
}

func (m *Memory) Usage(_ context.Context, tenantID string, from, to time.Time) ([]UsageDay, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	from, to = Day(from), Day(to)
	var out []UsageDay
	for day, sec := range m.usage[tenantID] {
		if !day.Before(from) && !day.After(to) {
			out = append(out, UsageDay{Day: day, ParticipantSeconds: sec})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day.Before(out[j].Day) })
	return out, nil
}

func (m *Memory) Close() {}
