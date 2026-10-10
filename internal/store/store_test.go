package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestMemory and TestPostgres run the same checks. TestPostgres needs
// SFU_TEST_DATABASE_URL (for example postgres://sfu:sfu@localhost:5432/sfu).
func TestMemory(t *testing.T) { conformance(t, NewMemory()) }

func TestPostgres(t *testing.T) {
	url := os.Getenv("SFU_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set SFU_TEST_DATABASE_URL to run against PostgreSQL")
	}
	s, err := OpenPostgres(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	conformance(t, s)
}

func conformance(t *testing.T, s Store) {
	ctx := context.Background()
	// Unique IDs, so a shared database can be reused between runs.
	id := fmt.Sprintf("t%d", time.Now().UnixNano())
	now := time.Now().UTC().Truncate(time.Second)

	tn := Tenant{ID: id, Name: "Acme", WebhookSecret: "whsec", Limits: Limits{MaxRooms: 3}, CreatedAt: now}
	if err := s.CreateTenant(ctx, tn); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if err := s.CreateTenant(ctx, tn); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate tenant: err = %v, want ErrExists", err)
	}
	got, err := s.GetTenant(ctx, id)
	if err != nil || got.Name != "Acme" || got.Limits.MaxRooms != 3 || got.WebhookSecret != "whsec" {
		t.Errorf("GetTenant = %+v, %v", got, err)
	}

	got.Name, got.Limits.MaxParticipants, got.WebhookSecret = "Acme Inc", 50, ""
	if err := s.UpdateTenant(ctx, got); err != nil {
		t.Fatalf("UpdateTenant: %v", err)
	}
	got, _ = s.GetTenant(ctx, id)
	if got.Name != "Acme Inc" || got.Limits.MaxParticipants != 50 || got.WebhookSecret != "whsec" {
		t.Errorf("after update = %+v (an empty webhook secret must keep the old one)", got)
	}
	if err := s.UpdateTenant(ctx, Tenant{ID: id + "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update missing: err = %v", err)
	}

	key := APIKey{Key: id + "-key", Secret: "s1", TenantID: id, CreatedAt: now}
	if err := s.CreateAPIKey(ctx, key); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if err := s.CreateAPIKey(ctx, APIKey{Key: id + "-k2", TenantID: id + "missing", CreatedAt: now}); !errors.Is(err, ErrNotFound) {
		t.Errorf("key for missing tenant: err = %v, want ErrNotFound", err)
	}
	k, tt, err := s.LookupAPIKey(ctx, key.Key)
	if err != nil || k.Secret != "s1" || tt.ID != id {
		t.Errorf("LookupAPIKey = %+v %+v %v", k, tt, err)
	}
	if err := s.RevokeAPIKey(ctx, id+"other", key.Key); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoke with wrong tenant: err = %v, want ErrNotFound", err)
	}
	if err := s.RevokeAPIKey(ctx, id, key.Key); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	if _, _, err := s.LookupAPIKey(ctx, key.Key); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked key: err = %v, want ErrNotFound", err)
	}
	keys, _ := s.ListAPIKeys(ctx, id)
	if len(keys) != 1 || !keys[0].Revoked {
		t.Errorf("ListAPIKeys = %+v", keys)
	}

	day1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	_ = s.AddUsage(ctx, id, day1, 60)
	_ = s.AddUsage(ctx, id, day1.Add(5*time.Hour), 30)
	_ = s.AddUsage(ctx, id, day1.AddDate(0, 0, 2), 15)
	usage, err := s.Usage(ctx, id, day1, day1.AddDate(0, 0, 1))
	if err != nil || len(usage) != 1 || usage[0].ParticipantSeconds != 90 || !usage[0].Day.Equal(Day(day1)) {
		t.Errorf("Usage = %+v, %v", usage, err)
	}
}
