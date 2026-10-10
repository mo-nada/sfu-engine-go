package signaling

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mo-nada/sfu-engine/internal/auth"
	"github.com/mo-nada/sfu-engine/internal/room"
	"github.com/mo-nada/sfu-engine/internal/transport"
	"github.com/mo-nada/sfu-engine/pkg/token"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestServer(t *testing.T) (*httptest.Server, *room.Manager) {
	t.Helper()
	api, err := transport.NewAPI()
	if err != nil {
		t.Fatalf("NewAPI: %v", err)
	}
	rooms := room.NewManager(discardLogger())
	tenants, err := auth.NewTenants([]auth.Tenant{
		{ID: "acme", APIKey: "acme-key", Secret: acmeSecret},
		{ID: "globex", APIKey: "globex-key", Secret: globexSecret},
	})
	if err != nil {
		t.Fatalf("tenants: %v", err)
	}
	h := NewHandler(api, transport.Configuration(nil), rooms, tenants, discardLogger())
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
	for msg.Type == TypeOffer { // the subscriber offer may arrive in between
		msg = next(t, conn)
	}
	if msg.Type != TypeError || !strings.Contains(msg.Message, contains) {
		t.Fatalf("got %+v, want error containing %q", msg, contains)
	}
}

const (
	acmeSecret   = "acme-secret-acme-secret-acme-sec"
	globexSecret = "globex-secret-globex-secret-glob"
)

// acmeToken returns a join token of tenant "acme" with all permissions.
func acmeToken(t *testing.T, roomID, participant string) string {
	t.Helper()
	tok, err := token.Sign(acmeSecret, token.New("acme-key", participant, roomID, time.Minute))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return tok
}

func joinMsg(t *testing.T, roomID, participant string) Message {
	return Message{Type: TypeJoin, Token: acmeToken(t, roomID, participant)}
}

func join(t *testing.T, conn *websocket.Conn, roomID, participant string) {
	t.Helper()
	send(t, conn, joinMsg(t, roomID, participant))
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
	r, ok := rooms.Room("acme/r1")
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

	send(t, conn, Message{Type: TypeJoin, Token: "not-a-token"})
	expectError(t, conn, "invalid token")
	forged, _ := token.Sign(globexSecret, token.New("acme-key", "alice", "r1", time.Minute))
	send(t, conn, Message{Type: TypeJoin, Token: forged})
	expectError(t, conn, "invalid token")

	join(t, conn, "r1", "alice")
	if msg := next(t, conn); msg.Type != TypeOffer {
		t.Fatalf("got %+v, want subscriber offer", msg)
	}
	send(t, conn, joinMsg(t, "r1", "alice"))
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
	send(t, second, joinMsg(t, "r1", "alice"))
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
		_, ok := rooms.Room("acme/r1")
		return !ok
	})
}

func TestLeaveMessageClosesSession(t *testing.T) {
	srv, rooms := newTestServer(t)
	conn := dial(t, srv)
	join(t, conn, "r1", "alice")

	send(t, conn, Message{Type: TypeLeave})
	eventually(t, "room removed after leave", func() bool {
		_, ok := rooms.Room("acme/r1")
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
		_, ok := rooms.Room("acme/r1")
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

func TestJoinedListsOthersAndEventsFollow(t *testing.T) {
	srv, _ := newTestServer(t)
	alice := dial(t, srv)
	send(t, alice, joinMsg(t, "r1", "alice"))
	first := next(t, alice)
	if first.Type != TypeJoined || first.Participants == nil || len(first.Participants) != 0 {
		t.Fatalf("alice joined = %+v, want empty participants list", first)
	}
	muted := true
	send(t, alice, Message{Type: TypeMute, Source: "microphone", Muted: &muted})

	bob := dial(t, srv)
	send(t, bob, joinMsg(t, "r1", "bob"))
	joined := next(t, bob)
	if joined.Type != TypeJoined || len(joined.Participants) != 1 ||
		joined.Participants[0].ID != "alice" || !joined.Participants[0].Muted["microphone"] {
		t.Fatalf("bob joined = %+v, want alice with muted microphone", joined)
	}

	// Alice hears about bob, then about bob's mute, then bob leaving.
	if msg := nextOfType(t, alice, TypeParticipantJoined); msg.Participant != "bob" {
		t.Errorf("participant-joined = %+v", msg)
	}
	off := true
	send(t, bob, Message{Type: TypeMute, Source: "camera", Muted: &off})
	if msg := nextOfType(t, alice, TypeMute); msg.Participant != "bob" || msg.Source != "camera" || msg.Muted == nil || !*msg.Muted {
		t.Errorf("mute = %+v", msg)
	}
	send(t, bob, Message{Type: TypeLeave})
	if msg := nextOfType(t, alice, TypeParticipantLeft); msg.Participant != "bob" {
		t.Errorf("participant-left = %+v", msg)
	}
}

func TestInvalidMuteRejected(t *testing.T) {
	srv, _ := newTestServer(t)
	conn := dial(t, srv)
	join(t, conn, "r1", "alice")
	muted := true
	send(t, conn, Message{Type: TypeMute, Source: "hologram", Muted: &muted})
	expectError(t, conn, "valid source")
	send(t, conn, Message{Type: TypeMute, Source: "camera"})
	expectError(t, conn, "valid source")
}

// nextOfType skips messages until one of type typ arrives.
func nextOfType(t *testing.T, conn *websocket.Conn, typ string) Message {
	t.Helper()
	for {
		if msg := next(t, conn); msg.Type == typ {
			return msg
		}
	}
}

// The same room name in two tenants gives two separate rooms.
func TestTenantsAreIsolated(t *testing.T) {
	srv, rooms := newTestServer(t)
	join(t, dial(t, srv), "r1", "alice")

	bob := dial(t, srv)
	tok, _ := token.Sign(globexSecret, token.New("globex-key", "bob", "r1", time.Minute))
	send(t, bob, Message{Type: TypeJoin, Token: tok})
	joined := next(t, bob)
	if joined.Type != TypeJoined || len(joined.Participants) != 0 {
		t.Fatalf("globex bob joined = %+v, want an empty room", joined)
	}
	if _, ok := rooms.Room("acme/r1"); !ok {
		t.Error("acme room missing")
	}
	if _, ok := rooms.Room("globex/r1"); !ok {
		t.Error("globex room missing")
	}
}

func TestPublishNotGranted(t *testing.T) {
	srv, _ := newTestServer(t)
	conn := dial(t, srv)
	c := token.New("acme-key", "viewer", "r1", time.Minute)
	c.Grants.Publish = false
	tok, _ := token.Sign(acmeSecret, c)
	send(t, conn, Message{Type: TypeJoin, Token: tok})
	if msg := next(t, conn); msg.Type != TypeJoined {
		t.Fatalf("got %+v, want joined", msg)
	}
	send(t, conn, Message{Type: TypeOffer, SDP: "v=0"})
	expectError(t, conn, "not allowed to publish")
}
