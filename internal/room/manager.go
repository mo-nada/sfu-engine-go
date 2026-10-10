package room

import (
	"errors"
	"log/slog"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	// ErrDuplicateParticipant is returned when the participant ID is already in the room.
	ErrDuplicateParticipant = errors.New("participant already in room")
	// ErrManagerClosed is returned when joining after Close.
	ErrManagerClosed = errors.New("room manager closed")
	// ErrNotInRoom is returned when the participant is not in the room.
	ErrNotInRoom = errors.New("participant not in room")
	// ErrRoomExists is returned by Create when the room already exists.
	ErrRoomExists = errors.New("room already exists")
	// ErrRoomNotFound is returned when the room does not exist.
	ErrRoomNotFound = errors.New("room not found")
	// ErrRoomFull is returned by Join when the room has MaxParticipants.
	ErrRoomFull = errors.New("room is full")
)

// Manager owns all rooms. Join and Leave are serialized by one lock, so a
// participant can never be added to a room that is being removed.
type Manager struct {
	logger *slog.Logger

	mu     sync.Mutex
	rooms  map[string]*Room
	closed bool
}

// NewManager creates an empty Manager.
func NewManager(logger *slog.Logger) *Manager {
	return &Manager{logger: logger, rooms: make(map[string]*Room)}
}

// Join adds p to the room, creating the room if needed.
func (m *Manager) Join(roomID string, p *Participant) (*Room, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, ErrManagerClosed
	}

	r, ok := m.rooms[roomID]
	if !ok {
		r = newRoom(roomID, Options{}, false, m.logger)
		m.rooms[roomID] = r
		m.logger.Info("room created", "room_id", roomID)
	}
	// A duplicate ID is reported as such even when the room is full.
	if _, exists := r.Participant(p.ID()); exists {
		return nil, ErrDuplicateParticipant
	}
	if max := r.opts.MaxParticipants; max > 0 && r.count() >= max {
		return nil, ErrRoomFull
	}
	if !r.add(p) {
		return nil, ErrDuplicateParticipant
	}
	if r.emptyTimer != nil {
		r.emptyTimer.Stop()
		r.emptyTimer = nil
	}

	// Notify under the lock, so the roster and later events arrive in order.
	others := r.othersThan(p.ID())
	roster := make([]ParticipantInfo, 0, len(others))
	for _, o := range others {
		roster = append(roster, o.info())
		o.send(Event{Kind: EventJoined, Participant: p.ID()})
	}
	p.send(Event{Kind: EventRoster, Roster: roster})

	m.logger.Info("participant joined", "room_id", roomID, "participant_id", p.ID())
	return r, nil
}

// SetMuted records that participantID muted or unmuted source, and tells
// the others in the room.
func (m *Manager) SetMuted(roomID, participantID, source string, muted bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.rooms[roomID]
	if !ok {
		return ErrNotInRoom
	}
	p, ok := r.Participant(participantID)
	if !ok {
		return ErrNotInRoom
	}
	p.setMuted(source, muted)
	for _, o := range r.othersThan(participantID) {
		o.send(Event{Kind: EventMuted, Participant: participantID, Source: source, Muted: muted})
	}
	m.logger.Debug("participant mute changed", "room_id", roomID, "participant_id", participantID, "source", source, "muted", muted)
	return nil
}

// Leave removes the participant and closes its peers. The room is removed when
// it becomes empty. Leaving a room or participant that does not exist is a no-op.
func (m *Manager) Leave(roomID, participantID string) {
	_ = m.leave(roomID, participantID, nil, "")
}

// LeaveParticipant is like Leave, but only removes p itself: if p already
// left and a new participant now uses the same ID, it does nothing. A session
// that ends uses this, so it can never remove a newer session of the same user.
func (m *Manager) LeaveParticipant(roomID string, p *Participant) {
	_ = m.leave(roomID, p.ID(), p, "")
}

// leave removes participantID (only if it is want, when want is set). If
// reason is set, the participant is first sent EventRemoved. Membership is
// checked and changed under one lock hold.
func (m *Manager) leave(roomID, participantID string, want *Participant, reason string) error {
	m.mu.Lock()
	r, ok := m.rooms[roomID]
	if !ok {
		m.mu.Unlock()
		return ErrRoomNotFound
	}
	p, ok := r.Participant(participantID)
	if !ok || (want != nil && p != want) {
		m.mu.Unlock()
		return ErrNotInRoom
	}
	if reason != "" {
		p.send(Event{Kind: EventRemoved, Reason: reason})
	}
	_, remaining := r.remove(participantID)
	removeRoom := remaining == 0 && !r.explicit
	if removeRoom {
		delete(m.rooms, roomID)
	} else if remaining == 0 {
		m.startEmptyTimer(r) // explicit rooms wait for EmptyTimeout
	}
	for _, o := range r.Participants() {
		o.send(Event{Kind: EventLeft, Participant: participantID})
	}
	m.mu.Unlock()

	r.router.RemoveSubscriber(participantID, p.subscriber)
	if removeRoom {
		r.router.Close()
	}
	// Close outside the lock: closing peers can be slow, and peer callbacks may call Leave.
	if err := p.Close(); err != nil {
		m.logger.Warn("participant close error", "room_id", roomID, "participant_id", participantID, "err", err)
	}
	if reason != "" {
		m.logger.Info("participant removed", "room_id", roomID, "participant_id", participantID, "reason", reason)
	} else {
		m.logger.Info("participant left", "room_id", roomID, "participant_id", participantID)
	}
	if removeRoom {
		m.logger.Info("room closed", "room_id", roomID)
	}
	return nil
}

// DefaultEmptyTimeout is used by Create when opts.EmptyTimeout is not positive.
const DefaultEmptyTimeout = 5 * time.Minute

// Create creates an explicit room with opts. It stays open while empty until
// opts.EmptyTimeout (DefaultEmptyTimeout if not set), then it is removed.
func (m *Manager) Create(roomID string, opts Options) (*Room, error) {
	if opts.EmptyTimeout <= 0 {
		opts.EmptyTimeout = DefaultEmptyTimeout
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrManagerClosed
	}
	if _, ok := m.rooms[roomID]; ok {
		return nil, ErrRoomExists
	}
	r := newRoom(roomID, opts, true, m.logger)
	m.rooms[roomID] = r
	m.startEmptyTimer(r)
	m.logger.Info("room created", "room_id", roomID, "max_participants", opts.MaxParticipants, "empty_timeout", opts.EmptyTimeout)
	return r, nil
}

// startEmptyTimer removes r after its EmptyTimeout if it is still empty then.
// It must be called with m.mu held.
func (m *Manager) startEmptyTimer(r *Room) {
	if r.emptyTimer != nil {
		r.emptyTimer.Stop()
	}
	r.emptyTimer = time.AfterFunc(r.opts.EmptyTimeout, func() {
		m.mu.Lock()
		if m.rooms[r.id] != r || r.count() > 0 {
			m.mu.Unlock()
			return
		}
		delete(m.rooms, r.id)
		m.mu.Unlock()
		r.router.Close()
		m.logger.Info("room closed", "room_id", r.id, "reason", "empty timeout")
	})
}

// Remove takes participantID out of the room: it is told why (EventRemoved),
// leaves the room, and its peers are closed.
func (m *Manager) Remove(roomID, participantID, reason string) error {
	return m.leave(roomID, participantID, nil, reason)
}

// Delete closes the room: every participant is told why (EventRemoved) and
// closed, and the room is removed at once.
func (m *Manager) Delete(roomID, reason string) error {
	m.mu.Lock()
	r, ok := m.rooms[roomID]
	if !ok {
		m.mu.Unlock()
		return ErrRoomNotFound
	}
	delete(m.rooms, roomID)
	if r.emptyTimer != nil {
		r.emptyTimer.Stop()
	}
	removed := r.removeAll()
	for _, p := range removed {
		p.send(Event{Kind: EventRemoved, Reason: reason})
	}
	m.mu.Unlock()

	r.router.Close()
	for _, p := range removed {
		if err := p.Close(); err != nil {
			m.logger.Warn("participant close error", "room_id", roomID, "participant_id", p.ID(), "err", err)
		}
	}
	m.logger.Info("room closed", "room_id", roomID, "reason", reason, "participants", len(removed))
	return nil
}

// RoomSummary describes a room.
type RoomSummary struct {
	ID              string
	CreatedAt       time.Time
	NumParticipants int
	Options         Options
	Explicit        bool
}

// ParticipantDetail describes a participant in a room.
type ParticipantDetail struct {
	ID       string
	JoinedAt time.Time
	Muted    map[string]bool
	Sources  []string // sources being published, sorted
}

// List returns the rooms whose ID starts with prefix, sorted by ID.
func (m *Manager) List(prefix string) []RoomSummary {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []RoomSummary
	for id, r := range m.rooms {
		if strings.HasPrefix(id, prefix) {
			out = append(out, r.summary())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Summary returns one room's summary.
func (m *Manager) Summary(roomID string) (RoomSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[roomID]
	if !ok {
		return RoomSummary{}, ErrRoomNotFound
	}
	return r.summary(), nil
}

// Details returns the participants of a room, sorted by ID.
func (m *Manager) Details(roomID string) ([]ParticipantDetail, error) {
	m.mu.Lock()
	r, ok := m.rooms[roomID]
	m.mu.Unlock()
	if !ok {
		return nil, ErrRoomNotFound
	}
	parts := r.Participants()
	out := make([]ParticipantDetail, 0, len(parts))
	for _, p := range parts {
		p.mu.Lock()
		muted := maps.Clone(p.muted)
		p.mu.Unlock()
		out = append(out, ParticipantDetail{
			ID:       p.ID(),
			JoinedAt: p.joinedAt,
			Muted:    muted,
			Sources:  r.router.PublishedSources(p.ID()),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *Room) summary() RoomSummary {
	return RoomSummary{ID: r.id, CreatedAt: r.createdAt, NumParticipants: r.count(), Options: r.opts, Explicit: r.explicit}
}

// Room returns the room with the given ID.
func (m *Manager) Room(id string) (*Room, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rooms[id]
	return r, ok
}

// Close removes all rooms and closes every participant. Later Joins fail.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	rooms := m.rooms
	m.rooms = make(map[string]*Room)
	m.mu.Unlock()

	for id, r := range rooms {
		if r.emptyTimer != nil {
			r.emptyTimer.Stop()
		}
		r.router.Close()
		for _, p := range r.removeAll() {
			if err := p.Close(); err != nil {
				m.logger.Warn("participant close error", "room_id", id, "participant_id", p.ID(), "err", err)
			}
		}
		m.logger.Info("room closed", "room_id", id)
	}
}
