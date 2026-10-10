// Package room manages rooms and the participants inside them.
//
// Ownership: the Manager owns all rooms, a Room owns its participants, and a
// Participant owns its two peers (publish and subscribe). Removing a
// participant closes its peers; removing the last participant removes the room.
package room

import (
	"errors"

	"github.com/mo-nada/sfu-engine/internal/peer"
)

// Participant is one client in a room. It uses two PeerConnections:
// Publisher carries media from the client to the SFU (the client offers),
// Subscriber carries media from the SFU to the client (the SFU offers).
type Participant struct {
	id         string
	publisher  *peer.Peer
	subscriber *peer.Peer
}

// NewParticipant creates a participant. Either peer may be nil if it has not
// been created yet.
func NewParticipant(id string, publisher, subscriber *peer.Peer) *Participant {
	return &Participant{id: id, publisher: publisher, subscriber: subscriber}
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
