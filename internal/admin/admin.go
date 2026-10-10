// Package admin is the operator API (/admin/v1): customers (tenants), their
// API keys, plan limits, webhook settings and usage for billing.
//
// It is for the platform operator only, never for tenants. Every request
// needs "Authorization: Bearer <admin secret>". Keep it on a private network
// when possible. Secrets (API key secrets, webhook secrets) are returned only
// when they are created.
package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mo-nada/sfu-engine/internal/auth"
	"github.com/mo-nada/sfu-engine/internal/store"
)

const maxBodyBytes = 64 << 10

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Server serves /admin/v1.
type Server struct {
	secret  string
	store   store.Store
	tenants *auth.Tenants // to drop cached lookups after a change
	logger  *slog.Logger
}

// NewServer returns the admin API. secret must be at least 32 bytes.
func NewServer(secret string, s store.Store, tenants *auth.Tenants, logger *slog.Logger) *Server {
	return &Server{secret: secret, store: s, tenants: tenants, logger: logger}
}

// Handler returns the /admin/v1 routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/v1/tenants", s.createTenant)
	mux.HandleFunc("GET /admin/v1/tenants", s.listTenants)
	mux.HandleFunc("GET /admin/v1/tenants/{id}", s.getTenant)
	mux.HandleFunc("PATCH /admin/v1/tenants/{id}", s.updateTenant)
	mux.HandleFunc("POST /admin/v1/tenants/{id}/keys", s.createKey)
	mux.HandleFunc("GET /admin/v1/tenants/{id}/keys", s.listKeys)
	mux.HandleFunc("DELETE /admin/v1/tenants/{id}/keys/{key}", s.revokeKey)
	mux.HandleFunc("GET /admin/v1/tenants/{id}/usage", s.usage)
	mux.HandleFunc("/admin/v1/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	return s.authenticate(mux)
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.secret)) != 1 {
			s.logger.Warn("admin auth rejected", "remote", r.RemoteAddr)
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid admin secret")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- tenants ----------

type tenantJSON struct {
	store.Tenant
	WebhookSecret string `json:"webhook_secret,omitempty"` // only when created or rotated
}

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID              string       `json:"id"`
		Name            string       `json:"name"`
		AutoCreateRooms bool         `json:"auto_create_rooms"`
		WebhookURL      string       `json:"webhook_url"`
		Limits          store.Limits `json:"limits"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !validID.MatchString(req.ID) {
		writeError(w, http.StatusBadRequest, "invalid", "id must be 1-64 letters, digits, '-' or '_'")
		return
	}
	if msg := checkTenant(req.WebhookURL, req.Limits); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid", msg)
		return
	}
	t := store.Tenant{
		ID: req.ID, Name: req.Name, AutoCreateRooms: req.AutoCreateRooms, WebhookURL: req.WebhookURL,
		WebhookSecret: "whsec_" + randomHex(24), Limits: req.Limits, CreatedAt: time.Now().UTC(),
	}
	if err := s.store.CreateTenant(r.Context(), t); err != nil {
		s.storeError(w, err)
		return
	}
	key, err := s.newKey(r, t.ID)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.logger.Info("tenant created", "tenant_id", t.ID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"tenant":     tenantJSON{Tenant: t, WebhookSecret: t.WebhookSecret},
		"api_key":    key.Key,
		"api_secret": key.Secret,
	})
}

func (s *Server) listTenants(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListTenants(r.Context())
	if err != nil {
		s.storeError(w, err)
		return
	}
	if list == nil {
		list = []store.Tenant{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenants": list})
}

func (s *Server) getTenant(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTenant(r.Context(), r.PathValue("id"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) updateTenant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name                *string       `json:"name"`
		AutoCreateRooms     *bool         `json:"auto_create_rooms"`
		WebhookURL          *string       `json:"webhook_url"`
		Limits              *store.Limits `json:"limits"`
		Disabled            *bool         `json:"disabled"`
		RotateWebhookSecret bool          `json:"rotate_webhook_secret"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	t, err := s.store.GetTenant(r.Context(), r.PathValue("id"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	if req.Name != nil {
		t.Name = *req.Name
	}
	if req.AutoCreateRooms != nil {
		t.AutoCreateRooms = *req.AutoCreateRooms
	}
	if req.WebhookURL != nil {
		t.WebhookURL = *req.WebhookURL
	}
	if req.Limits != nil {
		t.Limits = *req.Limits
	}
	if req.Disabled != nil {
		t.Disabled = *req.Disabled
	}
	if msg := checkTenant(t.WebhookURL, t.Limits); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid", msg)
		return
	}
	out := tenantJSON{}
	t.WebhookSecret = "" // keep the stored one
	if req.RotateWebhookSecret {
		t.WebhookSecret = "whsec_" + randomHex(24)
		out.WebhookSecret = t.WebhookSecret
	}
	if err := s.store.UpdateTenant(r.Context(), t); err != nil {
		s.storeError(w, err)
		return
	}
	s.tenants.Forget()
	t, _ = s.store.GetTenant(r.Context(), t.ID)
	out.Tenant = t
	s.logger.Info("tenant updated", "tenant_id", t.ID, "disabled", t.Disabled)
	writeJSON(w, http.StatusOK, out)
}

func checkTenant(webhookURL string, l store.Limits) string {
	if webhookURL != "" {
		u, err := url.Parse(webhookURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return "webhook_url must be an http or https URL"
		}
	}
	if l.MaxRooms < 0 || l.MaxParticipants < 0 || l.MaxRoomSize < 0 {
		return "limits must not be negative (0 means no limit)"
	}
	return ""
}

// ---------- keys ----------

func (s *Server) newKey(r *http.Request, tenantID string) (store.APIKey, error) {
	k := store.APIKey{
		Key: "key_" + randomHex(10), Secret: randomHex(32), TenantID: tenantID, CreatedAt: time.Now().UTC(),
	}
	return k, s.store.CreateAPIKey(r.Context(), k)
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	k, err := s.newKey(r, r.PathValue("id"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.logger.Info("api key created", "tenant_id", k.TenantID, "api_key", k.Key)
	writeJSON(w, http.StatusCreated, map[string]any{"api_key": k.Key, "api_secret": k.Secret, "created_at": k.CreatedAt})
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetTenant(r.Context(), id); err != nil {
		s.storeError(w, err)
		return
	}
	keys, err := s.store.ListAPIKeys(r.Context(), id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if keys == nil {
		keys = []store.APIKey{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys}) // secrets are never listed
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	if err := s.store.RevokeAPIKey(r.Context(), r.PathValue("id"), r.PathValue("key")); err != nil {
		s.storeError(w, err)
		return
	}
	s.tenants.Forget()
	s.logger.Info("api key revoked", "tenant_id", r.PathValue("id"), "api_key", r.PathValue("key"))
	w.WriteHeader(http.StatusNoContent)
}

// ---------- usage ----------

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetTenant(r.Context(), id); err != nil {
		s.storeError(w, err)
		return
	}
	now := time.Now().UTC()
	from, to := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), now
	for name, dst := range map[string]*time.Time{"from": &from, "to": &to} {
		if v := r.URL.Query().Get(name); v != "" {
			d, err := time.Parse(time.DateOnly, v)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid", name+" must be a date like 2026-10-01")
				return
			}
			*dst = d
		}
	}
	if to.Before(from) || to.Sub(from) > 366*24*time.Hour {
		writeError(w, http.StatusBadRequest, "invalid", "need from <= to, at most one year apart")
		return
	}
	days, err := s.store.Usage(r.Context(), id, from, to)
	if err != nil {
		s.storeError(w, err)
		return
	}
	type dayJSON struct {
		Day                string  `json:"day"`
		ParticipantMinutes float64 `json:"participant_minutes"`
	}
	out := make([]dayJSON, 0, len(days))
	var total int64
	for _, d := range days {
		out = append(out, dayJSON{Day: d.Day.Format(time.DateOnly), ParticipantMinutes: float64(d.ParticipantSeconds) / 60})
		total += d.ParticipantSeconds
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant_id": id, "from": from.Format(time.DateOnly), "to": to.Format(time.DateOnly),
		"days": out, "total_participant_minutes": float64(total) / 60,
	})
}

// ---------- helpers ----------

func (s *Server) storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, store.ErrExists):
		writeError(w, http.StatusConflict, "conflict", "already exists")
	default:
		s.logger.Error("admin store error", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
