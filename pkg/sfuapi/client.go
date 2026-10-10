// Package sfuapi is the Go server SDK: it calls the SFU's server API (/v1)
// and admin API (/admin/v1), and verifies webhooks. Join tokens are made with
// package token.
//
//	rooms := sfuapi.NewRoomService("https://sfu.example.com", apiKey, apiSecret)
//	room, err := rooms.CreateRoom(ctx, sfuapi.CreateRoom{Name: "standup", MaxParticipants: 10})
package sfuapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mo-nada/sfu-engine/pkg/token"
)

// Error is an error answer from the SFU ({"error": {"code", "message"}}).
type Error struct {
	Status  int
	Code    string // unauthorized, invalid, limit_reached, not_found, conflict, internal, unavailable
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("sfu: %d %s: %s", e.Status, e.Code, e.Message) }

type client struct {
	base string
	http *http.Client
	auth func() (string, error)
}

func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	tok, err := c.auth()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return &Error{Status: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func esc(s string) string { return url.PathEscape(s) }

// ---------- server API ----------

// RoomService calls the server API as one tenant.
type RoomService struct{ c client }

// NewRoomService returns a client for host (for example
// "https://sfu.example.com") signing short-lived API tokens with the key.
func NewRoomService(host, apiKey, apiSecret string) *RoomService {
	return &RoomService{c: client{
		base: strings.TrimRight(host, "/"),
		http: &http.Client{Timeout: 15 * time.Second},
		auth: func() (string, error) { return token.SignAPI(apiSecret, token.NewAPI(apiKey, time.Minute)) },
	}}
}

// Room is a room as the API returns it.
type Room struct {
	Name                string        `json:"name"`
	CreatedAt           time.Time     `json:"created_at"`
	NumParticipants     int           `json:"num_participants"`
	MaxParticipants     int           `json:"max_participants"`
	EmptyTimeoutSeconds int           `json:"empty_timeout_seconds"`
	Participants        []Participant `json:"participants,omitempty"`
}

// Participant is a participant as the API returns it.
type Participant struct {
	Identity string          `json:"identity"`
	JoinedAt time.Time       `json:"joined_at"`
	Sources  []string        `json:"sources"`
	Muted    map[string]bool `json:"muted"`
	Grants   token.Grants    `json:"grants"`
}

// CreateRoom are the options of a new room.
type CreateRoom struct {
	Name                string `json:"name"`
	MaxParticipants     int    `json:"max_participants,omitempty"`
	EmptyTimeoutSeconds int    `json:"empty_timeout_seconds,omitempty"`
}

func (s *RoomService) CreateRoom(ctx context.Context, r CreateRoom) (Room, error) {
	var out Room
	return out, s.c.do(ctx, http.MethodPost, "/v1/rooms", r, &out)
}

func (s *RoomService) ListRooms(ctx context.Context) ([]Room, error) {
	var out struct{ Rooms []Room }
	return out.Rooms, s.c.do(ctx, http.MethodGet, "/v1/rooms", nil, &out)
}

// GetRoom returns the room with its participants.
func (s *RoomService) GetRoom(ctx context.Context, name string) (Room, error) {
	var out Room
	return out, s.c.do(ctx, http.MethodGet, "/v1/rooms/"+esc(name), nil, &out)
}

// DeleteRoom closes the room; everyone is disconnected.
func (s *RoomService) DeleteRoom(ctx context.Context, name string) error {
	return s.c.do(ctx, http.MethodDelete, "/v1/rooms/"+esc(name), nil, nil)
}

func (s *RoomService) ListParticipants(ctx context.Context, room string) ([]Participant, error) {
	var out struct{ Participants []Participant }
	return out.Participants, s.c.do(ctx, http.MethodGet, "/v1/rooms/"+esc(room)+"/participants", nil, &out)
}

// RemoveParticipant kicks a participant.
func (s *RoomService) RemoveParticipant(ctx context.Context, room, identity string) error {
	return s.c.do(ctx, http.MethodDelete, "/v1/rooms/"+esc(room)+"/participants/"+esc(identity), nil, nil)
}

// MutePublishedTrack mutes or unmutes a source from the server.
func (s *RoomService) MutePublishedTrack(ctx context.Context, room, identity, source string, muted bool) error {
	body := map[string]any{"source": source, "muted": muted}
	return s.c.do(ctx, http.MethodPost, "/v1/rooms/"+esc(room)+"/participants/"+esc(identity)+"/mute", body, nil)
}

// GrantsUpdate changes permissions; nil fields keep their value.
type GrantsUpdate struct {
	Publish   *bool     `json:"publish,omitempty"`
	Subscribe *bool     `json:"subscribe,omitempty"`
	Sources   *[]string `json:"sources,omitempty"`
}

// UpdateParticipant changes a participant's permissions live.
func (s *RoomService) UpdateParticipant(ctx context.Context, room, identity string, g GrantsUpdate) (Participant, error) {
	var out Participant
	err := s.c.do(ctx, http.MethodPatch, "/v1/rooms/"+esc(room)+"/participants/"+esc(identity), map[string]any{"grants": g}, &out)
	return out, err
}

// ---------- admin API ----------

// Admin calls the operator admin API. Never give the admin secret to tenants.
type Admin struct{ c client }

// NewAdmin returns an admin API client.
func NewAdmin(host, adminSecret string) *Admin {
	return &Admin{c: client{
		base: strings.TrimRight(host, "/"),
		http: &http.Client{Timeout: 15 * time.Second},
		auth: func() (string, error) { return adminSecret, nil },
	}}
}

// Limits are plan limits; 0 means no limit.
type Limits struct {
	MaxRooms        int `json:"max_rooms"`
	MaxParticipants int `json:"max_participants"`
	MaxRoomSize     int `json:"max_room_size"`
}

// Tenant is a customer.
type Tenant struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	AutoCreateRooms bool      `json:"auto_create_rooms"`
	WebhookURL      string    `json:"webhook_url"`
	WebhookSecret   string    `json:"webhook_secret,omitempty"` // only when created or rotated
	Limits          Limits    `json:"limits"`
	Disabled        bool      `json:"disabled"`
	CreatedAt       time.Time `json:"created_at"`
}

// NewTenant are the fields of a new tenant.
type NewTenant struct {
	ID              string `json:"id"`
	Name            string `json:"name,omitempty"`
	AutoCreateRooms bool   `json:"auto_create_rooms,omitempty"`
	WebhookURL      string `json:"webhook_url,omitempty"`
	Limits          Limits `json:"limits"`
}

// Created is a new tenant with its first credentials (shown only once).
type Created struct {
	Tenant    Tenant `json:"tenant"`
	APIKey    string `json:"api_key"`
	APISecret string `json:"api_secret"`
}

func (a *Admin) CreateTenant(ctx context.Context, t NewTenant) (Created, error) {
	var out Created
	return out, a.c.do(ctx, http.MethodPost, "/admin/v1/tenants", t, &out)
}

func (a *Admin) GetTenant(ctx context.Context, id string) (Tenant, error) {
	var out Tenant
	return out, a.c.do(ctx, http.MethodGet, "/admin/v1/tenants/"+esc(id), nil, &out)
}

// TenantUpdate changes a tenant; nil fields keep their value.
type TenantUpdate struct {
	Name                *string `json:"name,omitempty"`
	AutoCreateRooms     *bool   `json:"auto_create_rooms,omitempty"`
	WebhookURL          *string `json:"webhook_url,omitempty"`
	Limits              *Limits `json:"limits,omitempty"`
	Disabled            *bool   `json:"disabled,omitempty"`
	RotateWebhookSecret bool    `json:"rotate_webhook_secret,omitempty"`
}

func (a *Admin) UpdateTenant(ctx context.Context, id string, u TenantUpdate) (Tenant, error) {
	var out Tenant
	return out, a.c.do(ctx, http.MethodPatch, "/admin/v1/tenants/"+esc(id), u, &out)
}

// CreateKey adds an API key (for rotation); the secret is shown only here.
func (a *Admin) CreateKey(ctx context.Context, tenantID string) (key, secret string, err error) {
	var out struct {
		APIKey    string `json:"api_key"`
		APISecret string `json:"api_secret"`
	}
	err = a.c.do(ctx, http.MethodPost, "/admin/v1/tenants/"+esc(tenantID)+"/keys", nil, &out)
	return out.APIKey, out.APISecret, err
}

func (a *Admin) RevokeKey(ctx context.Context, tenantID, key string) error {
	return a.c.do(ctx, http.MethodDelete, "/admin/v1/tenants/"+esc(tenantID)+"/keys/"+esc(key), nil, nil)
}

// Usage is participant time for billing.
type Usage struct {
	TenantID string `json:"tenant_id"`
	From     string `json:"from"`
	To       string `json:"to"`
	Days     []struct {
		Day                string  `json:"day"`
		ParticipantMinutes float64 `json:"participant_minutes"`
	} `json:"days"`
	TotalParticipantMinutes float64 `json:"total_participant_minutes"`
}

// Usage returns participant-minutes per day; from and to are "2026-10-01"
// style dates ("" for this month).
func (a *Admin) Usage(ctx context.Context, tenantID, from, to string) (Usage, error) {
	q := url.Values{}
	if from != "" {
		q.Set("from", from)
	}
	if to != "" {
		q.Set("to", to)
	}
	path := "/admin/v1/tenants/" + esc(tenantID) + "/usage"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out Usage
	return out, a.c.do(ctx, http.MethodGet, path, nil, &out)
}
