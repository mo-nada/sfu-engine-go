// Package signaling implements the WebSocket signaling protocol between
// clients and the SFU.
//
// Every message is a JSON object with a "type" field:
//
//	client → server  join       {token}              must be the first message; the token
//	                                                 (see pkg/token) names the room, the
//	                                                 participant and its permissions
//	server → client  joined     {room, participant, participants}
//	                                                 participants: the others already in the
//	                                                 room, each {id, muted: {source: bool}}
//	server → client  participant-joined {participant}
//	server → client  participant-left   {participant}
//	client → server  mute       {source, muted}      the client muted or unmuted a source
//	server → client  mute       {participant, source, muted}  another participant did
//	client → server  offer      {sdp, tracks}        publish connection offer; tracks maps
//	                                                 track ID → "camera", "microphone",
//	                                                 "screen" or "screen-audio" for every
//	                                                 track in the offer (optional)
//	server → client  answer     {sdp}                answer to that offer
//	server → client  offer      {sdp}                subscribe connection offer
//	client → server  answer     {sdp}                answer to that offer
//	both ways        candidate  {target, candidate}  target is "publisher" or "subscriber"
//	client → server  leave      {}
//	server → client  removed    {message}            the server removed you (kicked, or the
//	                                                 room was closed); the connection closes
//	server → client  error      {message}
//
// Each participant uses two PeerConnections: on "publisher" the client
// offers and sends media; on "subscriber" the server offers and sends media.
//
// Forwarded tracks use the stream ID "<participant>/media" (camera and
// microphone) or "<participant>/screen" (screen and screen audio). To stop
// sending a track, the client removes it and sends a new offer.
package signaling

import (
	"github.com/pion/webrtc/v4"
)

// Message types.
const (
	TypeJoin      = "join"
	TypeJoined    = "joined"
	TypeOffer     = "offer"
	TypeAnswer    = "answer"
	TypeCandidate = "candidate"
	TypeLeave     = "leave"
	TypeError     = "error"

	TypeParticipantJoined = "participant-joined"
	TypeParticipantLeft   = "participant-left"
	TypeMute              = "mute"
	TypeRemoved           = "removed"
)

// Candidate targets.
const (
	TargetPublisher  = "publisher"
	TargetSubscriber = "subscriber"
)

// Message is one signaling message. Only the fields used by its Type are set.
type Message struct {
	Type        string                   `json:"type"`
	Token       string                   `json:"token,omitempty"`
	Room        string                   `json:"room,omitempty"`
	Participant string                   `json:"participant,omitempty"`
	SDP         string                   `json:"sdp,omitempty"`
	Tracks      map[string]string        `json:"tracks,omitempty"`
	Target      string                   `json:"target,omitempty"`
	Candidate   *webrtc.ICECandidateInit `json:"candidate,omitempty"`
	Message     string                   `json:"message,omitempty"`
	Source      string                   `json:"source,omitempty"`
	Muted       *bool                    `json:"muted,omitempty"`
	// Participants is set on "joined"; an empty room gives an empty list.
	Participants []ParticipantInfo `json:"participants,omitzero"`
}

// ParticipantInfo describes another participant in a "joined" message.
type ParticipantInfo struct {
	ID    string          `json:"id"`
	Muted map[string]bool `json:"muted,omitempty"`
}

// maxTracks bounds the size of an offer's tracks map.
const maxTracks = 16
