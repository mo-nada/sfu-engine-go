package room

import (
	"errors"
	"log/slog"
	"sync"
)

var (
	// ErrDuplicateParticipant is returned when the participant ID is already in the room.
	ErrDuplicateParticipant = errors.New("participant already in room")
	// ErrManagerClosed is returned when joining after Close.
	ErrManagerClosed = errors.New("room manager closed")
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
		r = newRoom(roomID)
		m.rooms[roomID] = r
		m.logger.Info("room created", "room_id", roomID)
	}
	if !r.add(p) {
		return nil, ErrDuplicateParticipant
	}

	m.logger.Info("participant joined", "room_id", roomID, "participant_id", p.ID())
	return r, nil
}

// Leave removes the participant and closes its peers. The room is removed when
// it becomes empty. Leaving a room or participant that does not exist is a no-op.
func (m *Manager) Leave(roomID, participantID string) {
	m.mu.Lock()
	r, ok := m.rooms[roomID]
	if !ok {
		m.mu.Unlock()
		return
	}
	p, remaining := r.remove(participantID)
	if remaining == 0 {
		delete(m.rooms, roomID)
	}
	m.mu.Unlock()

	if p == nil {
		return
	}
	// Close outside the lock: closing peers can be slow, and peer callbacks may call Leave.
	if err := p.Close(); err != nil {
		m.logger.Warn("participant close error", "room_id", roomID, "participant_id", participantID, "err", err)
	}
	m.logger.Info("participant left", "room_id", roomID, "participant_id", participantID)
	if remaining == 0 {
		m.logger.Info("room closed", "room_id", roomID)
	}
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
		for _, p := range r.removeAll() {
			if err := p.Close(); err != nil {
				m.logger.Warn("participant close error", "room_id", id, "participant_id", p.ID(), "err", err)
			}
		}
		m.logger.Info("room closed", "room_id", id)
	}
}
