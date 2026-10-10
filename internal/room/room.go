package room

import (
	"sync"
)

// Room is a group of participants that can exchange media.
// Its membership is changed only through the Manager.
type Room struct {
	id string

	mu           sync.RWMutex
	participants map[string]*Participant
}

func newRoom(id string) *Room {
	return &Room{id: id, participants: make(map[string]*Participant)}
}

// ID returns the room identifier.
func (r *Room) ID() string { return r.id }

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
