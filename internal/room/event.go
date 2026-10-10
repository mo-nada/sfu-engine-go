package room

import "github.com/mo-nada/sfu-engine/pkg/token"

// Event kinds delivered to participants through their notifier.
const (
	// EventRoster is sent once to a participant when it joins. Roster lists
	// the other participants at that moment.
	EventRoster = "roster"
	// EventJoined is sent to the others when Participant joins.
	EventJoined = "joined"
	// EventLeft is sent to the others when Participant leaves.
	EventLeft = "left"
	// EventMuted is sent to the others when Participant mutes or unmutes Source.
	EventMuted = "muted"
	// EventGrants is sent to a participant whose permissions the server
	// changed. Grants holds the new permissions.
	EventGrants = "grants"
	// EventRemoved is sent to a participant that the server removes (kicked,
	// or its room closed). Reason says why. The participant is already out
	// of the room when it arrives.
	EventRemoved = "removed"
)

// Event is a room change delivered to a participant.
type Event struct {
	Kind        string
	Participant string
	Source      string
	Muted       bool
	ByServer    bool // for EventMuted: the server muted it, the client cannot unmute
	Grants      token.Grants
	Reason      string
	Roster      []ParticipantInfo
}

// ParticipantInfo describes another participant in a roster.
type ParticipantInfo struct {
	ID    string
	Muted map[string]bool // source -> muted; only sources that were reported
}

// Notifier receives room events for one participant. The Manager calls it
// while holding its lock, so that every participant sees events in the same
// order; it must not block and must not call back into the Manager.
type Notifier func(Event)
