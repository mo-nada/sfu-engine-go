package signaling

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"

	"github.com/mo-nada/sfu-engine/internal/peer"
	"github.com/mo-nada/sfu-engine/internal/room"
)

const (
	maxMessageSize = 64 << 10
	writeTimeout   = 10 * time.Second
	pongTimeout    = 60 * time.Second
	pingInterval   = 30 * time.Second
)

// Handler accepts WebSocket connections and runs one session per connection.
type Handler struct {
	api      *webrtc.API
	iceCfg   webrtc.Configuration
	rooms    *room.Manager
	logger   *slog.Logger
	upgrader websocket.Upgrader

	mu       sync.Mutex
	sessions map[*session]struct{}
	closed   bool
}

// NewHandler creates a Handler. The upgrader keeps gorilla's default origin
// check, so browsers may only connect from pages on the same host.
func NewHandler(api *webrtc.API, iceCfg webrtc.Configuration, rooms *room.Manager, logger *slog.Logger) *Handler {
	return &Handler{
		api:      api,
		iceCfg:   iceCfg,
		rooms:    rooms,
		logger:   logger,
		sessions: make(map[*session]struct{}),
	}
}

// ServeHTTP upgrades the request to a WebSocket and serves it until it closes.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade has already written the HTTP error.
	}
	s := &session{h: h, conn: conn, logger: h.logger.With("remote", r.RemoteAddr), done: make(chan struct{})}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		_ = conn.Close()
		return
	}
	h.sessions[s] = struct{}{}
	h.mu.Unlock()

	s.logger.Debug("signaling connected")
	go s.pingLoop()
	s.readLoop()
	s.close()
}

// Close closes every session and refuses new ones.
func (h *Handler) Close() {
	h.mu.Lock()
	h.closed = true
	sessions := make([]*session, 0, len(h.sessions))
	for s := range h.sessions {
		sessions = append(sessions, s)
	}
	h.mu.Unlock()

	for _, s := range sessions {
		s.close()
	}
}

// session is one client connection. Messages are handled one at a time by
// readLoop; writes come from many goroutines and are serialized by writeMu.
type session struct {
	h      *Handler
	conn   *websocket.Conn
	logger *slog.Logger
	done   chan struct{}

	writeMu sync.Mutex
	closed  atomic.Bool

	mu            sync.Mutex // guards the fields below, set once on join
	joinLogger    *slog.Logger
	roomID        string
	participantID string
	publisher     *peer.Peer
	subscriber    *peer.Peer
}

func (s *session) readLoop() {
	s.conn.SetReadLimit(maxMessageSize)
	_ = s.conn.SetReadDeadline(time.Now().Add(pongTimeout))
	s.conn.SetPongHandler(func(string) error {
		return s.conn.SetReadDeadline(time.Now().Add(pongTimeout))
	})

	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			return
		}
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			s.sendError("invalid JSON")
			continue
		}
		if !s.handle(msg) {
			return
		}
	}
}

// handle processes one message. It returns false when the session must end.
func (s *session) handle(msg Message) bool {
	s.mu.Lock()
	pub, sub, logger := s.publisher, s.subscriber, s.joinLogger
	s.mu.Unlock()
	joined := pub != nil

	if msg.Type == TypeJoin {
		if joined {
			s.sendError("already joined")
			return true
		}
		s.join(msg.Room, msg.Participant)
		return true
	}
	if !joined {
		s.sendError("join first")
		return true
	}

	switch msg.Type {
	case TypeOffer:
		answer, err := pub.HandleOffer(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: msg.SDP})
		if err != nil {
			logger.Warn("bad publisher offer", "err", err)
			s.sendError("invalid offer")
			return true
		}
		s.send(Message{Type: TypeAnswer, SDP: answer.SDP})
	case TypeAnswer:
		// A rejected answer leaves the subscribe connection unable to renegotiate,
		// so it ends the session.
		if err := sub.HandleAnswer(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: msg.SDP}); err != nil {
			logger.Warn("bad subscriber answer, disconnecting", "err", err)
			s.sendError("invalid answer")
			return false
		}
	case TypeCandidate:
		target := map[string]*peer.Peer{TargetPublisher: pub, TargetSubscriber: sub}[msg.Target]
		if target == nil || msg.Candidate == nil {
			s.sendError("candidate needs a target and a candidate")
			return true
		}
		if err := target.AddICECandidate(*msg.Candidate); err != nil {
			logger.Debug("bad ICE candidate", "target", msg.Target, "err", err)
			s.sendError("invalid candidate")
		}
	case TypeLeave:
		return false
	default:
		s.sendError("unknown message type")
	}
	return true
}

// join creates the participant's two peers, wires them to signaling and the
// room's router, and sends the first subscriber offer.
func (s *session) join(roomID, participantID string) {
	if !validID.MatchString(roomID) || !validID.MatchString(participantID) {
		s.sendError("room and participant must be 1-64 letters, digits, '-' or '_'")
		return
	}
	logger := s.logger.With("room_id", roomID, "participant_id", participantID)

	pub, err := peer.New(s.h.api, s.h.iceCfg, logger, participantID+"/pub")
	if err != nil {
		s.failJoin(logger, err)
		return
	}
	sub, err := peer.New(s.h.api, s.h.iceCfg, logger, participantID+"/sub")
	if err != nil {
		_ = pub.Close()
		s.failJoin(logger, err)
		return
	}
	// Gives the first subscriber offer a media section, so ICE connects before any track exists.
	if _, err := sub.CreateDataChannel("sfu"); err != nil {
		_ = pub.Close()
		_ = sub.Close()
		s.failJoin(logger, err)
		return
	}

	pub.OnICECandidate(func(c webrtc.ICECandidateInit) {
		s.send(Message{Type: TypeCandidate, Target: TargetPublisher, Candidate: &c})
	})
	sub.OnICECandidate(func(c webrtc.ICECandidateInit) {
		s.send(Message{Type: TypeCandidate, Target: TargetSubscriber, Candidate: &c})
	})
	sub.OnOffer(func(o webrtc.SessionDescription) {
		s.send(Message{Type: TypeOffer, SDP: o.SDP})
	})

	r, err := s.h.rooms.Join(roomID, room.NewParticipant(participantID, pub, sub))
	if err != nil {
		_ = pub.Close()
		_ = sub.Close()
		if errors.Is(err, room.ErrDuplicateParticipant) {
			s.sendError("participant already in room")
		} else {
			s.failJoin(logger, err)
		}
		return
	}

	s.mu.Lock()
	s.joinLogger = logger
	s.roomID, s.participantID = roomID, participantID
	s.publisher, s.subscriber = pub, sub
	s.mu.Unlock()

	pub.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if err := r.Router().Publish(participantID, pub, remote); err != nil {
			logger.Warn("publish failed", "err", err)
		}
	})
	// A peer that fails or closes ends the session. Run close in a new
	// goroutine: OnClose may be called from inside session.close itself.
	pub.OnClose(func() { go s.close() })
	sub.OnClose(func() { go s.close() })
	r.Router().AddSubscriber(participantID, sub)

	// close may have run during the steps above. It reads the participant
	// fields after marking the session closed, and we check the mark after all
	// wiring is done, so at least one of us calls Leave and the participant
	// never stays in the room or the router. A second Leave is harmless.
	if s.closed.Load() {
		s.h.rooms.Leave(roomID, participantID)
		// If close's Leave ran first, our Leave finds no participant and does
		// not touch the router, so undo AddSubscriber directly.
		r.Router().RemoveSubscriber(participantID)
		return
	}

	s.send(Message{Type: TypeJoined, Room: roomID, Participant: participantID})
	if err := sub.Negotiate(); err != nil {
		logger.Warn("initial subscriber offer failed", "err", err)
	}
}

func (s *session) failJoin(logger *slog.Logger, err error) {
	logger.Error("join failed", "err", err)
	s.sendError("join failed")
}

// send writes msg. A failed write ends the session.
func (s *session) send(msg Message) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed.Load() {
		return
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := s.conn.WriteJSON(msg); err != nil {
		s.logger.Debug("signaling write failed", "err", err)
		// send may run inside a Pion callback, so do not close peers here.
		go s.close()
	}
}

func (s *session) sendError(text string) {
	s.send(Message{Type: TypeError, Message: text})
}

func (s *session) pingLoop() {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			// WriteControl may run concurrently with other writes.
			if err := s.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout)); err != nil {
				go s.close()
				return
			}
		}
	}
}

// close leaves the room, closes the peers and the WebSocket. Only the first
// call has effect; later calls return at once, so it can be reached again
// from peer OnClose callbacks without deadlocking.
func (s *session) close() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	close(s.done)

	s.h.mu.Lock()
	delete(s.h.sessions, s)
	s.h.mu.Unlock()

	s.mu.Lock()
	roomID, participantID := s.roomID, s.participantID
	s.mu.Unlock()
	if participantID != "" {
		s.h.rooms.Leave(roomID, participantID)
	}

	s.writeMu.Lock()
	_ = s.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	_ = s.conn.Close()
	s.writeMu.Unlock()
	s.logger.Debug("signaling closed")
}
