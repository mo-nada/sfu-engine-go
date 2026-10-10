package signaling

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mo-nada/sfu-engine/internal/room"
	"github.com/mo-nada/sfu-engine/internal/transport"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestServer(t *testing.T) (*httptest.Server, *room.Manager) {
	t.Helper()
	api, err := transport.NewAPI()
	if err != nil {
		t.Fatalf("NewAPI: %v", err)
	}
	rooms := room.NewManager(discardLogger())
	h := NewHandler(api, transport.Configuration(nil), rooms, discardLogger())
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		h.Close()
		srv.Close()
		rooms.Close()
	})
	return srv, rooms
}

func dial(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func send(t *testing.T, conn *websocket.Conn, msg any) {
	t.Helper()
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// next returns the next message that is not an ICE candidate.
func next(t *testing.T, conn *websocket.Conn) Message {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var msg Message
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read: %v", err)
		}
		if msg.Type != TypeCandidate {
			return msg
		}
	}
}

func expectError(t *testing.T, conn *websocket.Conn, contains string) {
	t.Helper()
	msg := next(t, conn)
	if msg.Type != TypeError || !strings.Contains(msg.Message, contains) {
		t.Fatalf("got %+v, want error containing %q", msg, contains)
	}
}

func join(t *testing.T, conn *websocket.Conn, roomID, participant string) {
	t.Helper()
	send(t, conn, Message{Type: TypeJoin, Room: roomID, Participant: participant})
	if msg := next(t, conn); msg.Type != TypeJoined || msg.Room != roomID || msg.Participant != participant {
		t.Fatalf("got %+v, want joined", msg)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func TestJoinSendsJoinedThenSubscriberOffer(t *testing.T) {
	srv, rooms := newTestServer(t)
	conn := dial(t, srv)
	join(t, conn, "r1", "alice")

	offer := next(t, conn)
	if offer.Type != TypeOffer || !strings.Contains(offer.SDP, "m=application") {
		t.Fatalf("got %+v, want subscriber offer with a data channel section", offer)
	}
	r, ok := rooms.Room("r1")
	if !ok {
		t.Fatal("room not created")
	}
	if _, ok := r.Participant("alice"); !ok {
		t.Error("alice not in room")
	}
}

// Bad messages get an error reply and the connection stays usable.
func TestBadMessagesGetErrors(t *testing.T) {
	srv, _ := newTestServer(t)
	conn := dial(t, srv)

	send(t, conn, Message{Type: TypeOffer, SDP: "v=0"})
	expectError(t, conn, "join first")

	if err := conn.WriteMessage(websocket.TextMessage, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	expectError(t, conn, "invalid JSON")

	send(t, conn, Message{Type: TypeJoin, Room: "bad room!", Participant: "alice"})
	expectError(t, conn, "letters, digits")
	send(t, conn, Message{Type: TypeJoin, Room: "r1", Participant: ""})
	expectError(t, conn, "letters, digits")

	join(t, conn, "r1", "alice")
	if msg := next(t, conn); msg.Type != TypeOffer {
		t.Fatalf("got %+v, want subscriber offer", msg)
	}
	send(t, conn, Message{Type: TypeJoin, Room: "r1", Participant: "alice"})
	expectError(t, conn, "already joined")

	send(t, conn, Message{Type: "dance"})
	expectError(t, conn, "unknown message type")

	send(t, conn, Message{Type: TypeCandidate, Target: "nowhere"})
	expectError(t, conn, "target")

	send(t, conn, Message{Type: TypeOffer, SDP: "not sdp"})
	expectError(t, conn, "invalid offer")
}

func TestDuplicateParticipantRejected(t *testing.T) {
	srv, _ := newTestServer(t)
	join(t, dial(t, srv), "r1", "alice")

	second := dial(t, srv)
	send(t, second, Message{Type: TypeJoin, Room: "r1", Participant: "alice"})
	expectError(t, second, "already in room")

	// The rejected connection can still join with another ID.
	join(t, second, "r1", "bob")
}

func TestDisconnectLeavesRoom(t *testing.T) {
	srv, rooms := newTestServer(t)
	conn := dial(t, srv)
	join(t, conn, "r1", "alice")

	_ = conn.Close()
	eventually(t, "room removed after disconnect", func() bool {
		_, ok := rooms.Room("r1")
		return !ok
	})
}

func TestLeaveMessageClosesSession(t *testing.T) {
	srv, rooms := newTestServer(t)
	conn := dial(t, srv)
	join(t, conn, "r1", "alice")

	send(t, conn, Message{Type: TypeLeave})
	eventually(t, "room removed after leave", func() bool {
		_, ok := rooms.Room("r1")
		return !ok
	})
	expectClosed(t, conn)
}

func TestBadAnswerDisconnects(t *testing.T) {
	srv, rooms := newTestServer(t)
	conn := dial(t, srv)
	join(t, conn, "r1", "alice")
	if msg := next(t, conn); msg.Type != TypeOffer {
		t.Fatalf("got %+v, want offer", msg)
	}

	send(t, conn, Message{Type: TypeAnswer, SDP: "garbage"})
	expectError(t, conn, "invalid answer")
	expectClosed(t, conn)
	eventually(t, "room removed", func() bool {
		_, ok := rooms.Room("r1")
		return !ok
	})
}

// expectClosed reads until the server closes the connection.
func expectClosed(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			if strings.Contains(err.Error(), "timeout") {
				t.Fatal("connection was not closed by the server")
			}
			return
		}
	}
}
