package room

import (
	"sync"
	"time"

	"github.com/mo-nada/sfu-engine/internal/sfu"
	"github.com/mo-nada/sfu-engine/pkg/token"
)

// Room is a group of participants that can exchange media.
// Its membership is changed only through the Manager. Each room owns one
// Router that forwards media between its participants.
type Room struct {
	id        string
	router    *sfu.Router
	createdAt time.Time
	opts      Options
	// explicit rooms were created through the API: they stay open while
	// empty until opts.EmptyTimeout. Implicit rooms (created by a join) are
	// removed as soon as the last participant leaves.
	explicit bool
	// emptyTimer removes an empty explicit room. Guarded by the Manager lock.
	emptyTimer *time.Timer
	// overrides keeps server changes per identity (permissions, server mutes)
	// for the room's lifetime, so they survive a reconnect. Guarded by the
	// Manager lock.
	overrides map[string]*override

	mu           sync.RWMutex
	participants map[string]*Participant
}

// Options are set when a room is created through the API.
type Options struct {
	// MaxParticipants limits the room size; 0 means no limit.
	MaxParticipants int
	// EmptyTimeout is how long an explicit room stays open with nobody in it.
	EmptyTimeout time.Duration
}

// newRoom creates a room whose router reports track events to m's observer.
// It must be called with m.mu held.
func (m *Manager) newRoom(id string, opts Options, explicit bool) *Room {
	router := sfu.NewRouter(m.logger.With("room_id", id))
	router.OnTrack(func(published bool, publisherID, source string) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if published {
			m.obs.TrackPublished(id, publisherID, source)
		} else {
			m.obs.TrackUnpublished(id, publisherID, source)
		}
	})
	return &Room{
		id:           id,
		router:       router,
		createdAt:    time.Now(),
		opts:         opts,
		explicit:     explicit,
		participants: make(map[string]*Participant),
	}
}

// override is what the server API changed for one identity.
type override struct {
	grants      *token.Grants   // replaces the token's grants on join
	serverMuted map[string]bool // source -> muted by the server
}

// override returns the identity's override, creating it. It must be called
// with the Manager lock held.
func (r *Room) override(id string) *override {
	if r.overrides == nil {
		r.overrides = make(map[string]*override)
	}
	ov := r.overrides[id]
	if ov == nil {
		ov = &override{serverMuted: make(map[string]bool)}
		r.overrides[id] = ov
	}
	return ov
}

func (r *Room) count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.participants)
}

// ID returns the room identifier.
func (r *Room) ID() string { return r.id }

// Router returns the room's media router.
func (r *Room) Router() *sfu.Router { return r.router }

// Participant returns the participant with the given ID.
func (r *Room) Participant(id string) (*Participant, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.participants[id]
	return p, ok
}

// Participants returns a snapshot of the current participants.
func (r *Room) Participants() []*Participant {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Participant, 0, len(r.participants))
	for _, p := range r.participants {
		out = append(out, p)
	}
	return out
}

// othersThan returns every participant except id.
func (r *Room) othersThan(id string) []*Participant {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Participant, 0, len(r.participants))
	for pid, p := range r.participants {
		if pid != id {
			out = append(out, p)
		}
	}
	return out
}

// add inserts p. It reports false if the ID is already taken.
func (r *Room) add(p *Participant) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.participants[p.ID()]; exists {
		return false
	}
	r.participants[p.ID()] = p
	return true
}

// remove deletes the participant with the given ID and returns it with the
// number of participants left.
func (r *Room) remove(id string) (removed *Participant, remaining int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	removed = r.participants[id]
	delete(r.participants, id)
	return removed, len(r.participants)
}

// removeAll deletes and returns every participant.
func (r *Room) removeAll() []*Participant {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Participant, 0, len(r.participants))
	for id, p := range r.participants {
		out = append(out, p)
		delete(r.participants, id)
	}
	return out
}
