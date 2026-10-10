// Package store keeps the durable platform data: tenants, their API keys and
// limits, and usage for billing. Memory is for development and tests;
// Postgres is for production. Every node uses the same store, so all nodes
// agree on tenants and keys.
package store

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrNotFound is returned when the tenant or key does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrExists is returned when creating something that already exists.
	ErrExists = errors.New("store: already exists")
)

// Limits caps what a tenant may use at the same time. Zero means no limit.
type Limits struct {
	MaxRooms        int `json:"max_rooms"`        // concurrent rooms
	MaxParticipants int `json:"max_participants"` // concurrent participants, all rooms together
	MaxRoomSize     int `json:"max_room_size"`    // participants in one room
}

// Tenant is a customer of the platform.
type Tenant struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	AutoCreateRooms bool      `json:"auto_create_rooms"`
	WebhookURL      string    `json:"webhook_url"`
	WebhookSecret   string    `json:"-"` // signs webhooks; shown only when created
	Limits          Limits    `json:"limits"`
	Disabled        bool      `json:"disabled"` // a disabled tenant cannot join or call the API
	CreatedAt       time.Time `json:"created_at"`
}

// APIKey is one credential of a tenant. A tenant can have several, so keys
// can be rotated without downtime.
type APIKey struct {
	Key       string    `json:"api_key"`
	Secret    string    `json:"-"` // shown only when created
	TenantID  string    `json:"tenant_id"`
	CreatedAt time.Time `json:"created_at"`
	Revoked   bool      `json:"revoked"`
}

// UsageDay is the usage of one tenant on one day (UTC).
type UsageDay struct {
	Day                time.Time `json:"day"`
	ParticipantSeconds int64     `json:"participant_seconds"`
}

// Store is the durable platform data.
type Store interface {
	CreateTenant(ctx context.Context, t Tenant) error
	UpdateTenant(ctx context.Context, t Tenant) error
	GetTenant(ctx context.Context, id string) (Tenant, error)
	ListTenants(ctx context.Context) ([]Tenant, error)

	CreateAPIKey(ctx context.Context, k APIKey) error
	ListAPIKeys(ctx context.Context, tenantID string) ([]APIKey, error)
	RevokeAPIKey(ctx context.Context, tenantID, key string) error
	// LookupAPIKey returns an active (not revoked) key and its tenant.
	LookupAPIKey(ctx context.Context, key string) (APIKey, Tenant, error)

	// AddUsage adds participant seconds to the tenant's usage on day (UTC).
	AddUsage(ctx context.Context, tenantID string, day time.Time, seconds int64) error
	// Usage returns the days from from to to (inclusive, UTC) that have usage.
	Usage(ctx context.Context, tenantID string, from, to time.Time) ([]UsageDay, error)

	Close()
}

// Day returns t's UTC date at midnight.
func Day(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
