package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres is the production Store.
type Postgres struct {
	pool *pgxpool.Pool
}

// schema is applied on start. Every statement is idempotent.
const schema = `
CREATE TABLE IF NOT EXISTS tenants (
	id                TEXT PRIMARY KEY,
	name              TEXT NOT NULL DEFAULT '',
	auto_create_rooms BOOLEAN NOT NULL DEFAULT FALSE,
	webhook_url       TEXT NOT NULL DEFAULT '',
	webhook_secret    TEXT NOT NULL DEFAULT '',
	max_rooms         INTEGER NOT NULL DEFAULT 0,
	max_participants  INTEGER NOT NULL DEFAULT 0,
	max_room_size     INTEGER NOT NULL DEFAULT 0,
	disabled          BOOLEAN NOT NULL DEFAULT FALSE,
	created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS api_keys (
	key        TEXT PRIMARY KEY,
	secret     TEXT NOT NULL,
	tenant_id  TEXT NOT NULL REFERENCES tenants(id),
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	revoked    BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE INDEX IF NOT EXISTS api_keys_tenant ON api_keys (tenant_id);
CREATE TABLE IF NOT EXISTS usage_days (
	tenant_id           TEXT NOT NULL REFERENCES tenants(id),
	day                 DATE NOT NULL,
	participant_seconds BIGINT NOT NULL DEFAULT 0,
	PRIMARY KEY (tenant_id, day)
);
`

// OpenPostgres connects to url (for example
// "postgres://sfu:sfu@localhost:5432/sfu") and creates the tables if needed.
func OpenPostgres(ctx context.Context, url string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres schema: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

const tenantColumns = `id, name, auto_create_rooms, webhook_url, webhook_secret,
	max_rooms, max_participants, max_room_size, disabled, created_at`

func scanTenant(row pgx.Row) (Tenant, error) {
	var t Tenant
	err := row.Scan(&t.ID, &t.Name, &t.AutoCreateRooms, &t.WebhookURL, &t.WebhookSecret,
		&t.Limits.MaxRooms, &t.Limits.MaxParticipants, &t.Limits.MaxRoomSize, &t.Disabled, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, ErrNotFound
	}
	return t, err
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23503"
}

func (p *Postgres) CreateTenant(ctx context.Context, t Tenant) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO tenants (`+tenantColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		t.ID, t.Name, t.AutoCreateRooms, t.WebhookURL, t.WebhookSecret,
		t.Limits.MaxRooms, t.Limits.MaxParticipants, t.Limits.MaxRoomSize, t.Disabled, t.CreatedAt)
	if isUniqueViolation(err) {
		return ErrExists
	}
	return err
}

func (p *Postgres) UpdateTenant(ctx context.Context, t Tenant) error {
	tag, err := p.pool.Exec(ctx, `UPDATE tenants SET name=$2, auto_create_rooms=$3, webhook_url=$4,
		webhook_secret=COALESCE(NULLIF($5, ''), webhook_secret),
		max_rooms=$6, max_participants=$7, max_room_size=$8, disabled=$9 WHERE id=$1`,
		t.ID, t.Name, t.AutoCreateRooms, t.WebhookURL, t.WebhookSecret,
		t.Limits.MaxRooms, t.Limits.MaxParticipants, t.Limits.MaxRoomSize, t.Disabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) GetTenant(ctx context.Context, id string) (Tenant, error) {
	return scanTenant(p.pool.QueryRow(ctx, `SELECT `+tenantColumns+` FROM tenants WHERE id=$1`, id))
}

func (p *Postgres) ListTenants(ctx context.Context) ([]Tenant, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+tenantColumns+` FROM tenants ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (p *Postgres) CreateAPIKey(ctx context.Context, k APIKey) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO api_keys (key, secret, tenant_id, created_at, revoked) VALUES ($1,$2,$3,$4,$5)`,
		k.Key, k.Secret, k.TenantID, k.CreatedAt, k.Revoked)
	switch {
	case isUniqueViolation(err):
		return ErrExists
	case isForeignKeyViolation(err):
		return ErrNotFound
	}
	return err
}

func (p *Postgres) ListAPIKeys(ctx context.Context, tenantID string) ([]APIKey, error) {
	rows, err := p.pool.Query(ctx, `SELECT key, secret, tenant_id, created_at, revoked FROM api_keys
		WHERE tenant_id=$1 ORDER BY created_at`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.Key, &k.Secret, &k.TenantID, &k.CreatedAt, &k.Revoked); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (p *Postgres) RevokeAPIKey(ctx context.Context, tenantID, key string) error {
	tag, err := p.pool.Exec(ctx, `UPDATE api_keys SET revoked=TRUE WHERE key=$1 AND tenant_id=$2`, key, tenantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) LookupAPIKey(ctx context.Context, key string) (APIKey, Tenant, error) {
	var k APIKey
	err := p.pool.QueryRow(ctx, `SELECT key, secret, tenant_id, created_at, revoked FROM api_keys
		WHERE key=$1 AND NOT revoked`, key).Scan(&k.Key, &k.Secret, &k.TenantID, &k.CreatedAt, &k.Revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIKey{}, Tenant{}, ErrNotFound
	}
	if err != nil {
		return APIKey{}, Tenant{}, err
	}
	t, err := p.GetTenant(ctx, k.TenantID)
	if err != nil {
		return APIKey{}, Tenant{}, err
	}
	return k, t, nil
}

func (p *Postgres) AddUsage(ctx context.Context, tenantID string, day time.Time, seconds int64) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO usage_days (tenant_id, day, participant_seconds) VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, day) DO UPDATE SET participant_seconds = usage_days.participant_seconds + EXCLUDED.participant_seconds`,
		tenantID, Day(day), seconds)
	return err
}

func (p *Postgres) Usage(ctx context.Context, tenantID string, from, to time.Time) ([]UsageDay, error) {
	rows, err := p.pool.Query(ctx, `SELECT day, participant_seconds FROM usage_days
		WHERE tenant_id=$1 AND day BETWEEN $2 AND $3 ORDER BY day`, tenantID, Day(from), Day(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageDay
	for rows.Next() {
		var u UsageDay
		if err := rows.Scan(&u.Day, &u.ParticipantSeconds); err != nil {
			return nil, err
		}
		u.Day = Day(u.Day)
		out = append(out, u)
	}
	return out, rows.Err()
}

func (p *Postgres) Close() { p.pool.Close() }
