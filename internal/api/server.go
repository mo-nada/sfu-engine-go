// Package api implements the server API (/v1) that tenants' backends and
// the server SDKs call: rooms, participants and join tokens.
//
// Every request needs "Authorization: Bearer <API token>" (see
// token.SignAPI). Everything is scoped to the token's tenant: another
// tenant's room answers 404, exactly like a room that does not exist.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mo-nada/sfu-engine/internal/auth"
	"github.com/mo-nada/sfu-engine/internal/room"
	"github.com/mo-nada/sfu-engine/internal/sfu"
	"github.com/mo-nada/sfu-engine/pkg/token"
)

const (
	defaultEmptyTimeout = room.DefaultEmptyTimeout
	maxEmptyTimeout     = 24 * time.Hour
	maxParticipantsCap  = 1000
	defaultTokenTTL     = time.Hour
	maxBodyBytes        = 64 << 10
)

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Rooms is what the API needs from the room layer. On one node it is the
// room.Manager; with several nodes it will route each call to the node that
// owns the room.
type Rooms interface {
	Create(roomID string, opts room.Options) error
	List(prefix string) []room.RoomSummary
	Summary(roomID string) (room.RoomSummary, error)
	Details(roomID string) ([]room.ParticipantDetail, error)
	Delete(roomID, reason string) error
	Remove(roomID, participantID, reason string) error
	ServerMute(roomID, participantID, source string, muted bool) error
	UpdateGrants(roomID, participantID string, change func(token.Grants) (token.Grants, error)) (token.Grants, error)
}

// LocalRooms adapts a room.Manager to Rooms.
type LocalRooms struct{ *room.Manager }

// Create creates an explicit room.
func (l LocalRooms) Create(roomID string, opts room.Options) error {
	_, err := l.Manager.Create(roomID, opts)
	return err
}

// Server serves /v1.
type Server struct {
	tenants *auth.Tenants
	rooms   Rooms
	logger  *slog.Logger
	cluster Cluster // nil: single node
	quota   Quota   // nil: no tenant limits
}

// SetPlatform enables multi-node routing and tenant limits. Call it before serving.
func (s *Server) SetPlatform(c Cluster, q Quota) { s.cluster, s.quota = c, q }

// NewServer creates the API server.
func NewServer(tenants *auth.Tenants, rooms Rooms, logger *slog.Logger) *Server {
	return &Server{tenants: tenants, rooms: rooms, logger: logger}
}

// Handler returns the /v1 routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/rooms", s.createRoom)
	mux.HandleFunc("GET /v1/rooms", s.listRooms)
	mux.HandleFunc("GET /v1/rooms/{room}", s.owned(s.getRoom))
	mux.HandleFunc("DELETE /v1/rooms/{room}", s.owned(s.deleteRoom))
	mux.HandleFunc("GET /v1/rooms/{room}/participants", s.owned(s.listParticipants))
	mux.HandleFunc("DELETE /v1/rooms/{room}/participants/{participant}", s.owned(s.removeParticipant))
	mux.HandleFunc("PATCH /v1/rooms/{room}/participants/{participant}", s.owned(s.updateParticipant))
	mux.HandleFunc("POST /v1/rooms/{room}/participants/{participant}/mute", s.owned(s.muteParticipant))
	mux.HandleFunc("POST /v1/tokens", s.createToken)
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	return s.authenticate(mux)
}

type tenantKey struct{}

// authenticate checks the bearer API token and puts its tenant in the context.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sfu-api"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer API token")
			return
		}
		tenant, err := s.tenants.VerifyAPIToken(strings.TrimSpace(tok), time.Now())
		if err != nil {
			s.logger.Info("api auth rejected", "err", err, "remote", r.RemoteAddr)
			w.Header().Set("WWW-Authenticate", `Bearer realm="sfu-api", error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid API token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tenantKey{}, tenant)))
	})
}

func tenantOf(r *http.Request) auth.Tenant { return r.Context().Value(tenantKey{}).(auth.Tenant) }

// roomKey returns the tenant-scoped room ID for the {room} path value.
func roomKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("room")
	if !validID.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid", "room name must be 1-64 letters, digits, '-' or '_'")
		return "", false
	}
	return tenantOf(r).ID + "/" + name, true
}

// ---------- rooms ----------

type roomJSON struct {
	Name                string            `json:"name"`
	CreatedAt           time.Time         `json:"created_at"`
	NumParticipants     int               `json:"num_participants"`
	MaxParticipants     int               `json:"max_participants"`
	EmptyTimeoutSeconds int               `json:"empty_timeout_seconds"`
	Participants        []participantJSON `json:"participants,omitempty"`
}

type participantJSON struct {
	Identity string          `json:"identity"`
	JoinedAt time.Time       `json:"joined_at"`
	Sources  []string        `json:"sources"`
	Muted    map[string]bool `json:"muted"`
	Grants   grantsJSON      `json:"grants"`
}

type grantsJSON struct {
	Publish   bool     `json:"publish"`
	Subscribe bool     `json:"subscribe"`
	Sources   []string `json:"sources"`
}

// grantsPatch is the request form of grants: missing fields keep their value.
type grantsPatch struct {
	Publish   *bool     `json:"publish"`
	Subscribe *bool     `json:"subscribe"`
	Sources   *[]string `json:"sources"`
}

// apply returns g with the patch applied, or an error message.
func (p *grantsPatch) apply(g token.Grants) (token.Grants, string) {
	if p == nil {
		return g, ""
	}
	if p.Publish != nil {
		g.Publish = *p.Publish
	}
	if p.Subscribe != nil {
		g.Subscribe = *p.Subscribe
	}
	if p.Sources != nil {
		for _, src := range *p.Sources {
			if !sfu.ValidSource(src) {
				return g, "unknown source " + src
			}
		}
		g.Sources = *p.Sources
	}
	return g, ""
}

func toRoomJSON(s room.RoomSummary) roomJSON {
	_, name, _ := strings.Cut(s.ID, "/")
	return roomJSON{
		Name:                name,
		CreatedAt:           s.CreatedAt.UTC(),
		NumParticipants:     s.NumParticipants,
		MaxParticipants:     s.Options.MaxParticipants,
		EmptyTimeoutSeconds: int(s.Options.EmptyTimeout / time.Second),
	}
}

func toParticipantsJSON(ds []room.ParticipantDetail) []participantJSON {
	out := make([]participantJSON, 0, len(ds))
	for _, d := range ds {
		p := participantJSON{Identity: d.ID, JoinedAt: d.JoinedAt.UTC(), Sources: d.Sources, Muted: d.Muted,
			Grants: grantsJSON{Publish: d.Grants.Publish, Subscribe: d.Grants.Subscribe, Sources: d.Grants.Sources}}
		if p.Grants.Sources == nil {
			p.Grants.Sources = []string{}
		}
		if p.Sources == nil {
			p.Sources = []string{}
		}
		if p.Muted == nil {
			p.Muted = map[string]bool{}
		}
		out = append(out, p)
	}
	return out
}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name                string `json:"name"`
		MaxParticipants     int    `json:"max_participants"`
		EmptyTimeoutSeconds *int   `json:"empty_timeout_seconds"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	opts := room.Options{MaxParticipants: req.MaxParticipants, EmptyTimeout: defaultEmptyTimeout}
	// Check the seconds before converting, so huge values cannot overflow.
	if sec := req.EmptyTimeoutSeconds; sec != nil {
		if *sec <= 0 || *sec > int(maxEmptyTimeout/time.Second) {
			writeError(w, http.StatusBadRequest, "invalid", "empty_timeout_seconds must be 1 to 86400")
			return
		}
		opts.EmptyTimeout = time.Duration(*sec) * time.Second
	}
	switch {
	case !validID.MatchString(req.Name):
		writeError(w, http.StatusBadRequest, "invalid", "name must be 1-64 letters, digits, '-' or '_'")
		return
	case opts.MaxParticipants < 0 || opts.MaxParticipants > maxParticipantsCap:
		writeError(w, http.StatusBadRequest, "invalid", "max_participants must be 0 (no limit) to 1000")
		return
	}

	tenant := tenantOf(r)
	if max := tenant.Limits.MaxRoomSize; max > 0 {
		if opts.MaxParticipants == 0 {
			opts.MaxParticipants = max
		} else if opts.MaxParticipants > max {
			writeError(w, http.StatusBadRequest, "invalid", fmt.Sprintf("max_participants is above your plan's room size (%d)", max))
			return
		}
	}
	if max := tenant.Limits.MaxRooms; max > 0 && s.quota != nil && s.quota.TenantRooms(tenant.ID) >= max {
		writeError(w, http.StatusForbidden, "limit_reached", fmt.Sprintf("your plan allows %d rooms at the same time", max))
		return
	}

	key := tenant.ID + "/" + req.Name
	if s.cluster != nil {
		// Place the room: on a less loaded node (forwarded there), or here.
		if !routed(r) {
			if id, _, err := s.cluster.Owner(r.Context(), key); err != nil {
				s.unavailable(w, err)
				return
			} else if id != "" && id != s.cluster.ID() {
				writeError(w, http.StatusConflict, "conflict", "room already exists")
				return
			}
			n, err := s.cluster.PickNode()
			if err != nil {
				s.unavailable(w, err)
				return
			}
			if n.ID != s.cluster.ID() {
				body, _ := json.Marshal(req)
				s.proxy(w, r, n.URL, body)
				return
			}
		}
		id, _, err := s.cluster.ClaimSelf(r.Context(), key)
		if err != nil {
			s.unavailable(w, err)
			return
		}
		if id != s.cluster.ID() {
			writeError(w, http.StatusConflict, "conflict", "room already exists")
			return
		}
	}
	if err := s.rooms.Create(key, opts); err != nil {
		if s.cluster != nil && !errors.Is(err, room.ErrRoomExists) {
			s.cluster.Release(key)
		}
		s.roomError(w, err)
		return
	}
	sum, err := s.rooms.Summary(key)
	if err != nil {
		s.roomError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toRoomJSON(sum))
}

func (s *Server) listRooms(w http.ResponseWriter, r *http.Request) {
	list := s.rooms.List(tenantOf(r).ID + "/")
	out := make([]roomJSON, 0, len(list))
	for _, sum := range list {
		out = append(out, toRoomJSON(sum))
	}
	if s.cluster != nil && !routed(r) {
		out = append(out, s.remoteRooms(r)...)
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": out})
}

func (s *Server) getRoom(w http.ResponseWriter, r *http.Request) {
	key, ok := roomKey(w, r)
	if !ok {
		return
	}
	sum, err := s.rooms.Summary(key)
	if err != nil {
		s.roomError(w, err)
		return
	}
	details, err := s.rooms.Details(key)
	if err != nil {
		s.roomError(w, err)
		return
	}
	out := toRoomJSON(sum)
	out.Participants = toParticipantsJSON(details)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteRoom(w http.ResponseWriter, r *http.Request) {
	key, ok := roomKey(w, r)
	if !ok {
		return
	}
	if err := s.rooms.Delete(key, "room closed"); err != nil {
		s.roomError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- participants ----------

func (s *Server) listParticipants(w http.ResponseWriter, r *http.Request) {
	key, ok := roomKey(w, r)
	if !ok {
		return
	}
	details, err := s.rooms.Details(key)
	if err != nil {
		s.roomError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"participants": toParticipantsJSON(details)})
}

func (s *Server) removeParticipant(w http.ResponseWriter, r *http.Request) {
	key, id, ok := participantKey(w, r)
	if !ok {
		return
	}
	if err := s.rooms.Remove(key, id, "removed by the server"); err != nil {
		s.roomError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) muteParticipant(w http.ResponseWriter, r *http.Request) {
	key, id, ok := participantKey(w, r)
	if !ok {
		return
	}
	var req struct {
		Source string `json:"source"`
		Muted  *bool  `json:"muted"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !sfu.ValidSource(req.Source) || req.Muted == nil {
		writeError(w, http.StatusBadRequest, "invalid", "need source (camera, microphone, screen, screen-audio) and muted")
		return
	}
	if err := s.rooms.ServerMute(key, id, req.Source, *req.Muted); err != nil {
		s.roomError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateParticipant(w http.ResponseWriter, r *http.Request) {
	key, id, ok := participantKey(w, r)
	if !ok {
		return
	}
	var req struct {
		Grants *grantsPatch `json:"grants"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	// The patch is applied to the current grants inside UpdateGrants, as one
	// step, so two concurrent PATCH requests cannot undo each other.
	_, err := s.rooms.UpdateGrants(key, id, func(cur token.Grants) (token.Grants, error) {
		g, msg := req.Grants.apply(cur)
		if msg != "" {
			return cur, errInvalidPatch(msg)
		}
		return g, nil
	})
	var invalid errInvalidPatch
	switch {
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, "invalid", string(invalid))
		return
	case err != nil:
		s.roomError(w, err)
		return
	}
	details, err := s.rooms.Details(key)
	if err != nil {
		s.roomError(w, err)
		return
	}
	for _, d := range details {
		if d.ID == id {
			writeJSON(w, http.StatusOK, toParticipantsJSON([]room.ParticipantDetail{d})[0])
			return
		}
	}
	writeError(w, http.StatusNotFound, "not_found", "participant not found") // left meanwhile
}

// errInvalidPatch is a validation error from a grants patch.
type errInvalidPatch string

func (e errInvalidPatch) Error() string { return string(e) }

// participantKey returns the room key and the {participant} path value.
func participantKey(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	key, ok := roomKey(w, r)
	if !ok {
		return "", "", false
	}
	id := r.PathValue("participant")
	if !validID.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid", "participant must be 1-64 letters, digits, '-' or '_'")
		return "", "", false
	}
	return key, id, true
}

// ---------- tokens ----------

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Room       string       `json:"room"`
		Identity   string       `json:"identity"`
		TTLSeconds int          `json:"ttl_seconds"`
		Grants     *grantsPatch `json:"grants"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ttl := defaultTokenTTL
	if req.TTLSeconds != 0 {
		// Check the seconds before converting, so huge values cannot overflow.
		if req.TTLSeconds < 0 || req.TTLSeconds > int(token.MaxTTL/time.Second) {
			writeError(w, http.StatusBadRequest, "invalid", "ttl_seconds must be 1 to 86400")
			return
		}
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	tenant := tenantOf(r)
	c := token.New(tenant.APIKey, req.Identity, req.Room, ttl)
	var msg string
	if c.Grants, msg = req.Grants.apply(c.Grants); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid", msg)
		return
	}
	tok, err := token.Sign(tenant.Secret, c)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", "room and identity must be 1-64 letters, digits, '-' or '_'")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "expires_at": time.Unix(c.ExpiresAt, 0).UTC()})
}

// ---------- helpers ----------

func (s *Server) roomError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, room.ErrRoomNotFound):
		writeError(w, http.StatusNotFound, "not_found", "room not found")
	case errors.Is(err, room.ErrNotInRoom):
		writeError(w, http.StatusNotFound, "not_found", "participant not found")
	case errors.Is(err, room.ErrRoomExists):
		writeError(w, http.StatusConflict, "conflict", "room already exists")
	default:
		s.logger.Error("api error", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
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
