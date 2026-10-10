package room

import "time"

// Observer is told about room, participant and track lifecycle events, for
// usage metering and webhooks. Methods are called while the Manager holds
// its lock (so events arrive in order) and must not block or call back into
// the Manager: queue the work instead.
type Observer interface {
	RoomStarted(roomID string)
	RoomFinished(roomID string)
	ParticipantJoined(roomID, participantID string)
	ParticipantLeft(roomID, participantID string, joinedAt time.Time)
	TrackPublished(roomID, participantID, source string)
	TrackUnpublished(roomID, participantID, source string)
}

// NopObserver ignores every event.
type NopObserver struct{}

func (NopObserver) RoomStarted(string)                        {}
func (NopObserver) RoomFinished(string)                       {}
func (NopObserver) ParticipantJoined(string, string)          {}
func (NopObserver) ParticipantLeft(string, string, time.Time) {}
func (NopObserver) TrackPublished(string, string, string)     {}
func (NopObserver) TrackUnpublished(string, string, string)   {}

// Observers sends every event to each observer in order.
type Observers []Observer

func (os Observers) RoomStarted(r string) {
	for _, o := range os {
		o.RoomStarted(r)
	}
}

func (os Observers) RoomFinished(r string) {
	for _, o := range os {
		o.RoomFinished(r)
	}
}

func (os Observers) ParticipantJoined(r, p string) {
	for _, o := range os {
		o.ParticipantJoined(r, p)
	}
}

func (os Observers) ParticipantLeft(r, p string, joined time.Time) {
	for _, o := range os {
		o.ParticipantLeft(r, p, joined)
	}
}

func (os Observers) TrackPublished(r, p, s string) {
	for _, o := range os {
		o.TrackPublished(r, p, s)
	}
}

func (os Observers) TrackUnpublished(r, p, s string) {
	for _, o := range os {
		o.TrackUnpublished(r, p, s)
	}
}

// SplitRoomID splits a room key "<tenant>/<room>" into its tenant and room name.
func SplitRoomID(roomID string) (tenantID, name string) {
	for i := 0; i < len(roomID); i++ {
		if roomID[i] == '/' {
			return roomID[:i], roomID[i+1:]
		}
	}
	return "", roomID
}
