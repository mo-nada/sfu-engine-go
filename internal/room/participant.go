// Package room manages rooms and the participants inside them.
//
// Ownership: the Manager owns all rooms, a Room owns its participants, and a
// Participant owns its two peers (publish and subscribe). Removing a
// participant closes its peers; removing the last participant removes the room.
package room

import (
	"errors"
	"maps"
	"sync"
	"time"

	"github.com/mo-nada/sfu-engine/internal/peer"
	"github.com/mo-nada/sfu-engine/pkg/token"
)

// Participant is one client in a room. It uses two PeerConnections:
// Publisher carries media from the client to the SFU (the client offers),
// Subscriber carries media from the SFU to the client (the SFU offers).
type Participant struct {
	id         string
	publisher  *peer.Peer
	subscriber *peer.Peer
	notify     Notifier
	joinedAt   time.Time

	// applyMu serializes permission and server-mute changes, which update
	// both the participant (under the Manager lock) and the router (after it).
	applyMu sync.Mutex

	mu          sync.Mutex
	muted       map[string]bool // source -> muted (as everyone sees it)
	serverMuted map[string]bool // source -> muted by the server
	grants      token.Grants
}

// NewParticipant creates a participant. Either peer may be nil if it has not
// been created yet. notify receives room events and may be nil.
func NewParticipant(id string, publisher, subscriber *peer.Peer, notify Notifier) *Participant {
	return &Participant{id: id, publisher: publisher, subscriber: subscriber, notify: notify,
		joinedAt: time.Now(), muted: make(map[string]bool), serverMuted: make(map[string]bool),
		grants: token.Grants{Publish: true, Subscribe: true}}
}

// SetGrants sets the permissions before the participant joins. After that,
// use Manager.UpdateGrants.
func (p *Participant) SetGrants(g token.Grants) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.grants = g
}

// Grants returns the current permissions.
func (p *Participant) Grants() token.Grants {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.grants
}

func (p *Participant) send(e Event) {
	if p.notify != nil {
		p.notify(e)
	}
}

func (p *Participant) info() ParticipantInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return ParticipantInfo{ID: p.id, Muted: maps.Clone(p.muted)}
}

func (p *Participant) setMuted(source string, muted bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.muted[source] = muted
}

// ID returns the participant identifier, unique within a room.
func (p *Participant) ID() string { return p.id }

// Publisher returns the publish peer, or nil.
func (p *Participant) Publisher() *peer.Peer { return p.publisher }

// Subscriber returns the subscribe peer, or nil.
func (p *Participant) Subscriber() *peer.Peer { return p.subscriber }

// Close closes both peers. It is safe to call more than once.
func (p *Participant) Close() error {
	var errs []error
	for _, pr := range []*peer.Peer{p.publisher, p.subscriber} {
		if pr != nil {
			errs = append(errs, pr.Close())
		}
	}
	return errors.Join(errs...)
}
