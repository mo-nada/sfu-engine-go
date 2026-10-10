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

	"github.com/mo-nada/sfu-engine/internal/auth"
	"github.com/mo-nada/sfu-engine/internal/peer"
	"github.com/mo-nada/sfu-engine/internal/room"
	"github.com/mo-nada/sfu-engine/internal/sfu"
	"github.com/mo-nada/sfu-engine/pkg/token"
)

const (
	maxMessageSize = 64 << 10
	writeTimeout   = 10 * time.Second
	pongTimeout    = 60 * time.Second
	pingInterval   = 30 * time.Second
	// sendQueueSize bounds the messages waiting for one client. A client that
	// falls this far behind is too slow and is disconnected.
	sendQueueSize = 256
)

// Handler accepts WebSocket connections and runs one session per connection.
type Handler struct {
	api      *webrtc.API
	iceCfg   webrtc.Configuration
	rooms    *room.Manager
	tenants  *auth.Tenants
	logger   *slog.Logger
	upgrader websocket.Upgrader

	mu       sync.Mutex
	sessions map[*session]struct{}
	closed   bool
}

// NewHandler creates a Handler. Joins must carry a token signed by one of
// tenants. The upgrader keeps gorilla's default origin check, so browsers
// may only connect from pages on the same host.
func NewHandler(api *webrtc.API, iceCfg webrtc.Configuration, rooms *room.Manager, tenants *auth.Tenants, logger *slog.Logger) *Handler {
	return &Handler{
		api:      api,
		iceCfg:   iceCfg,
		rooms:    rooms,
		tenants:  tenants,
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
	s := &session{
		h:          h,
		conn:       conn,
		logger:     h.logger.With("remote", r.RemoteAddr),
		done:       make(chan struct{}),
		out:        make(chan Message, sendQueueSize),
		writerDone: make(chan struct{}),
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		_ = conn.Close()
		return
	}
	h.sessions[s] = struct{}{}
	h.mu.Unlock()

	s.logger.Debug("signaling connected")
	go s.writeLoop()
	go s.pingLoop()
	s.readLoop()
	s.close()
}

// Close closes every session and refuses new ones. Sessions close in
// parallel, so one stuck client does not delay the others.
func (h *Handler) Close() {
	h.mu.Lock()
	h.closed = true
	sessions := make([]*session, 0, len(h.sessions))
	for s := range h.sessions {
		sessions = append(sessions, s)
	}
	h.mu.Unlock()

	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Go(s.close)
	}
	wg.Wait()
}

// session is one client connection. It runs three goroutines: readLoop
// handles incoming messages one at a time, writeLoop is the only writer of
// messages (send just queues them, so callers never wait on the network), and
// pingLoop keeps the connection alive.
type session struct {
	h      *Handler
	conn   *websocket.Conn
	logger *slog.Logger
	done   chan struct{}

	out        chan Message
	writerDone chan struct{}
	closed     atomic.Bool

	mu            sync.Mutex // guards the fields below, set once on join
	joinLogger    *slog.Logger
	grants        token.Grants
	roomID        string // tenant-scoped room key: "<tenant>/<room>"
	participantID string
	publisher     *peer.Peer
	subscriber    *peer.Peer
	trackSources  map[string]string // publisher track ID -> source, from the latest offer
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
	pub, sub, logger, grants := s.publisher, s.subscriber, s.joinLogger, s.grants
	s.mu.Unlock()
	joined := pub != nil

	if msg.Type == TypeJoin {
		if joined {
			s.sendError("already joined")
			return true
		}
		s.join(msg.Token)
		return true
	}
	if !joined {
		s.sendError("join first")
		return true
	}

	switch msg.Type {
	case TypeOffer:
		if !grants.Publish {
			s.sendError("not allowed to publish")
			return true
		}
		if len(msg.Tracks) > maxTracks {
			s.sendError("too many tracks")
			return true
		}
		// Each offer describes all tracks being sent, so its map replaces the
		// previous one. Record it before applying the offer: OnTrack fires after.
		sources := make(map[string]string, len(msg.Tracks))
		for id, source := range msg.Tracks {
			sources[id] = source
		}
		s.mu.Lock()
		s.trackSources = sources
		s.mu.Unlock()
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
	case TypeMute:
		if !sfu.ValidSource(msg.Source) || msg.Muted == nil {
			s.sendError("mute needs a valid source and muted")
			return true
		}
		s.mu.Lock()
		roomID, participantID := s.roomID, s.participantID
		s.mu.Unlock()
		if err := s.h.rooms.SetMuted(roomID, participantID, msg.Source, *msg.Muted); err != nil {
			logger.Debug("mute failed", "err", err)
			s.sendError("mute failed")
		}
	case TypeLeave:
		return false
	default:
		s.sendError("unknown message type")
	}
	return true
}

// join checks the token, creates the participant's two peers, wires them to
// signaling and the room's router, and sends the first subscriber offer.
//
// The room, identity, tenant and permissions come only from the token. Rooms
// are keyed by tenant, so equal room names of different tenants never meet.
// The token is checked once, at join; a session outlives its token.
func (s *session) join(tok string) {
	tenant, claims, err := s.h.tenants.VerifyJoinToken(tok, time.Now())
	if err != nil {
		s.logger.Info("join rejected", "err", err) // the reason is logged, not told to the client
		s.sendError("invalid token")
		return
	}
	roomName, participantID, grants := claims.Room, claims.Subject, claims.Grants
	roomID := tenant.ID + "/" + roomName
	logger := s.logger.With("tenant_id", tenant.ID, "room_id", roomName, "participant_id", participantID)

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

	notify := func(e room.Event) { s.send(eventMessage(roomName, participantID, e)) }
	r, err := s.h.rooms.Join(roomID, room.NewParticipant(participantID, pub, sub, notify))
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
	s.grants = grants
	s.roomID, s.participantID = roomID, participantID
	s.publisher, s.subscriber = pub, sub
	s.mu.Unlock()

	pub.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		s.mu.Lock()
		source := sfu.NormalizeSource(s.trackSources[remote.ID()], remote.Kind())
		s.mu.Unlock()
		if !grants.AllowsSource(source) {
			// Not forwarded. Keep reading and drop the packets, so Pion's
			// buffers and RTCP keep working; the loop ends when the client
			// removes the track or the peer closes.
			logger.Info("track rejected by grants", "source", source)
			s.sendError("not allowed to publish " + source)
			go func() {
				for {
					if _, _, err := remote.ReadRTP(); err != nil {
						return
					}
				}
			}()
			return
		}
		if err := r.Router().Publish(participantID, source, pub, remote); err != nil {
			logger.Warn("publish failed", "err", err)
		}
	})
	// A peer that fails or closes ends the session. Run close in a new
	// goroutine: OnClose may be called from inside session.close itself.
	pub.OnClose(func() { go s.close() })
	sub.OnClose(func() { go s.close() })
	if grants.Subscribe {
		r.Router().AddSubscriber(participantID, sub)
	}

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

	// "joined" with the roster was already queued by the room during Join.
	if err := sub.Negotiate(); err != nil {
		logger.Warn("initial subscriber offer failed", "err", err)
	}
}

func (s *session) failJoin(logger *slog.Logger, err error) {
	logger.Error("join failed", "err", err)
	s.sendError("join failed")
}

// send queues msg for writeLoop. It never blocks: if the queue is full, the
// client is too slow and the session ends.
func (s *session) send(msg Message) {
	if s.closed.Load() {
		return
	}
	select {
	case s.out <- msg:
	default:
		s.logger.Warn("signaling client too slow, disconnecting")
		go s.close()
	}
}

// writeLoop writes queued messages in order. When the session closes, it
// flushes what is still queued (for example the error that ended the
// session) and exits.
func (s *session) writeLoop() {
	defer close(s.writerDone)
	for {
		select {
		case msg := <-s.out:
			if !s.write(msg) {
				go s.close()
				return
			}
		case <-s.done:
			for {
				select {
				case msg := <-s.out:
					if !s.write(msg) {
						return
					}
				default:
					return
				}
			}
		}
	}
}

func (s *session) write(msg Message) bool {
	_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := s.conn.WriteJSON(msg); err != nil {
		s.logger.Debug("signaling write failed", "err", err)
		return false
	}
	return true
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

	// Wait for writeLoop to flush. close never runs on the writeLoop goroutine
	// (it calls close with go), so this cannot wait for itself.
	<-s.writerDone
	_ = s.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	_ = s.conn.Close()
	s.logger.Debug("signaling closed")
}

// eventMessage turns a room event for participantID into a signaling message.
func eventMessage(roomID, participantID string, e room.Event) Message {
	switch e.Kind {
	case room.EventRoster:
		infos := make([]ParticipantInfo, 0, len(e.Roster))
		for _, p := range e.Roster {
			infos = append(infos, ParticipantInfo{ID: p.ID, Muted: p.Muted})
		}
		return Message{Type: TypeJoined, Room: roomID, Participant: participantID, Participants: infos}
	case room.EventJoined:
		return Message{Type: TypeParticipantJoined, Participant: e.Participant}
	case room.EventLeft:
		return Message{Type: TypeParticipantLeft, Participant: e.Participant}
	default: // room.EventMuted
		muted := e.Muted
		return Message{Type: TypeMute, Participant: e.Participant, Source: e.Source, Muted: &muted}
	}
}
