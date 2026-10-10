// Package signaling implements the WebSocket signaling protocol between
// clients and the SFU.
//
// Every message is a JSON object with a "type" field:
//
//	client → server  join       {room, participant}  must be the first message
//	server → client  joined     {room, participant}
//	client → server  offer      {sdp, tracks}        publish connection offer; tracks maps
//	                                                 track ID → "camera", "microphone",
//	                                                 "screen" or "screen-audio" for every
//	                                                 track in the offer (optional)
//	server → client  answer     {sdp}                answer to that offer
//	server → client  offer      {sdp}                subscribe connection offer
//	client → server  answer     {sdp}                answer to that offer
//	both ways        candidate  {target, candidate}  target is "publisher" or "subscriber"
//	client → server  leave      {}
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
	"regexp"

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
)

// Candidate targets.
const (
	TargetPublisher  = "publisher"
	TargetSubscriber = "subscriber"
)

// Message is one signaling message. Only the fields used by its Type are set.
type Message struct {
	Type        string                   `json:"type"`
	Room        string                   `json:"room,omitempty"`
	Participant string                   `json:"participant,omitempty"`
	SDP         string                   `json:"sdp,omitempty"`
	Tracks      map[string]string        `json:"tracks,omitempty"`
	Target      string                   `json:"target,omitempty"`
	Candidate   *webrtc.ICECandidateInit `json:"candidate,omitempty"`
	Message     string                   `json:"message,omitempty"`
}

// maxTracks bounds the size of an offer's tracks map.
const maxTracks = 16

// validID matches room and participant IDs: 1–64 letters, digits, '-' or '_'.
var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
