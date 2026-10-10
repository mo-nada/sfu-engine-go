// Package integration runs real WebRTC clients against a real SFU server
// through the public pkg/sfu API and the WebSocket signaling protocol.
package integration

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	sfuinternal "github.com/mo-nada/sfu-engine/internal/sfu"
	"github.com/mo-nada/sfu-engine/internal/signaling"
	"github.com/mo-nada/sfu-engine/pkg/sfu"
	"github.com/mo-nada/sfu-engine/pkg/token"
)

// startServer runs an Engine on a random local port without STUN and returns
// its WebSocket URL. The engine shuts down when the test ends.
func startServer(t *testing.T) string {
	t.Helper()
	eng, err := sfu.New(
		sfu.WithTenant(acme),
		sfu.WithTenant(globex),
		sfu.WithSTUN(),
		sfu.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		sfu.WithShutdownTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("sfu.New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.Serve(ctx, ln) }()
	t.Cleanup(func() {
		// Close the test's idle API connections first. A spare connection the
		// HTTP client opened but never used would otherwise hold up
		// http.Server.Shutdown for up to 5 seconds, longer than the timeout here.
		http.DefaultClient.CloseIdleConnections()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server did not shut down")
		}
	})
	return wsURL(ln.Addr().String())
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// The Phase 1 milestone: Alice publishes video and Bob receives it through the SFU.
func TestTwoPeersForwardVideo(t *testing.T) {
	url := startServer(t)

	alice := newClient(t, url, "alice")
	alice.join("room1")
	bob := newClient(t, url, "bob")
	bob.join("room1")

	alice.publishVideo()

	eventually(t, "bob receives alice's video", func() bool { return bob.received("alice/media") >= 20 })
	if n := alice.received("alice/media"); n != 0 {
		t.Errorf("alice received %d packets of her own video, want 0", n)
	}
}

// Several participants: a late joiner gets existing video, two publishers
// reach everyone else, and rooms are isolated from each other.
func TestMultipleParticipants(t *testing.T) {
	url := startServer(t)

	alice := newClient(t, url, "alice")
	alice.join("room1")
	bob := newClient(t, url, "bob")
	bob.join("room1")
	alice.publishVideo()
	bob.publishVideo()

	carol := newClient(t, url, "carol") // joins after both published
	carol.join("room1")
	dave := newClient(t, url, "dave") // different room
	dave.join("room2")

	eventually(t, "everyone in room1 receives the other publishers", func() bool {
		return alice.received("bob/media") >= 10 &&
			bob.received("alice/media") >= 10 &&
			carol.received("alice/media") >= 10 && carol.received("bob/media") >= 10
	})
	time.Sleep(200 * time.Millisecond)
	if n := dave.received("alice/media") + dave.received("bob/media"); n != 0 {
		t.Errorf("dave in room2 received %d packets from room1, want 0", n)
	}
}

// When a publisher disconnects, the others are renegotiated and stop
// receiving the track, and the same name can join again.
func TestPublisherDisconnectCleansUp(t *testing.T) {
	url := startServer(t)

	alice := newClient(t, url, "alice")
	alice.join("room1")
	bob := newClient(t, url, "bob")
	bob.join("room1")
	alice.publishVideo()
	eventually(t, "bob receives alice's video", func() bool { return bob.received("alice/media") >= 10 })

	offersBefore := bob.offerCount()
	alice.close()

	eventually(t, "bob gets a new offer without alice's track", func() bool { return bob.offerCount() > offersBefore })
	eventually(t, "bob stops receiving alice's video", func() bool {
		before := bob.received("alice/media")
		time.Sleep(300 * time.Millisecond)
		return bob.received("alice/media") == before
	})

	// The participant ID is free again.
	again := newClient(t, url, "alice")
	again.join("room1")
	again.publishVideo()
	eventually(t, "bob receives the new alice", func() bool { return bob.received("alice/media") >= 20 })
}

// A client that disconnects in the middle of negotiation must not break the
// room for the others.
func TestDisconnectDuringNegotiation(t *testing.T) {
	url := startServer(t)

	bob := newClient(t, url, "bob")
	bob.join("room1")

	quitter := newClient(t, url, "quitter")
	quitter.join("room1")
	quitter.publishVideo()
	quitter.close() // gone before ICE/DTLS can finish

	alice := newClient(t, url, "alice")
	alice.join("room1")
	alice.publishVideo()
	eventually(t, "bob still receives alice's video", func() bool { return bob.received("alice/media") >= 20 })
}

// A duplicate participant name in the same room is rejected over signaling.
func TestDuplicateNameRejected(t *testing.T) {
	url := startServer(t)

	first := newClient(t, url, "alice")
	first.join("room1")

	second := newClient(t, url, "alice")
	second.send(signalingJoin(t, "room1", "alice"))
	select {
	case msg := <-second.errors:
		if msg != "participant already in room" {
			t.Errorf("error = %q", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no error for duplicate name")
	}
}

// A participant publishes camera, microphone and screen. The others receive
// all three, grouped by stream ID, including audio. Stopping the screen share
// removes only the screen track.
func TestCameraMicrophoneAndScreenShare(t *testing.T) {
	url := startServer(t)

	alice := newClient(t, url, "alice")
	alice.join("room1")
	bob := newClient(t, url, "bob")
	bob.join("room1")

	alice.publish(sfuinternal.SourceCamera, sfuinternal.SourceMicrophone, sfuinternal.SourceScreen)

	eventually(t, "bob receives alice's camera, microphone and screen", func() bool {
		return bob.receivedKind("alice/media", webrtc.RTPCodecTypeVideo) >= 10 &&
			bob.receivedKind("alice/media", webrtc.RTPCodecTypeAudio) >= 10 &&
			bob.receivedKind("alice/screen", webrtc.RTPCodecTypeVideo) >= 10
	})
	if n := bob.activeTracks("alice/media"); n != 2 {
		t.Errorf("alice/media has %d tracks, want 2 (camera + microphone)", n)
	}

	alice.unpublish(sfuinternal.SourceScreen)

	eventually(t, "alice's screen track ends for bob", func() bool { return bob.activeTracks("alice/screen") == 0 })
	// Camera and microphone keep flowing.
	video := bob.receivedKind("alice/media", webrtc.RTPCodecTypeVideo)
	audio := bob.receivedKind("alice/media", webrtc.RTPCodecTypeAudio)
	eventually(t, "camera and microphone still flow", func() bool {
		return bob.receivedKind("alice/media", webrtc.RTPCodecTypeVideo) > video+10 &&
			bob.receivedKind("alice/media", webrtc.RTPCodecTypeAudio) > audio+10
	})
}

// Two tenants use the same room name: their participants never meet.
func TestTenantsDoNotSeeEachOther(t *testing.T) {
	url := startServer(t)

	alice := newClient(t, url, "alice")
	alice.join("room1") // tenant acme
	bob := newClient(t, url, "bob")
	bob.joinWith(globex, "room1", allGrants)
	carol := newClient(t, url, "carol")
	carol.join("room1") // acme

	alice.publishVideo()
	eventually(t, "carol (same tenant) receives alice", func() bool { return carol.received("alice/media") >= 10 })
	if n := bob.received("alice/media"); n != 0 {
		t.Errorf("bob (other tenant) received %d packets from alice, want 0", n)
	}
}

// A token that allows only camera and microphone: a screen share is not
// forwarded, and the publisher gets an error.
func TestGrantsLimitSources(t *testing.T) {
	url := startServer(t)

	alice := newClient(t, url, "alice")
	alice.joinWith(acme, "room1", token.Grants{Publish: true, Subscribe: true, Sources: []string{"camera", "microphone"}})
	bob := newClient(t, url, "bob")
	bob.join("room1")

	alice.publish(sfuinternal.SourceCamera, sfuinternal.SourceScreen)

	eventually(t, "bob receives alice's camera", func() bool { return bob.received("alice/media") >= 10 })
	select {
	case msg := <-alice.errors:
		if msg != "not allowed to publish screen" {
			t.Errorf("error = %q", msg)
		}
	case <-time.After(5 * time.Second):
		t.Error("no error for the screen track")
	}
	if n := bob.received("alice/screen"); n != 0 {
		t.Errorf("bob received %d screen packets, want 0", n)
	}
}

// apiCall sends a server API request as tenant acme and returns the status.
func apiCall(t *testing.T, wsURL, method, path, body string) int {
	t.Helper()
	base := "http://" + strings.TrimSuffix(strings.TrimPrefix(wsURL, "ws://"), "/ws")
	req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
	tok, err := token.SignAPI(acme.Secret, token.NewAPI(acme.APIKey, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func expectRemoved(t *testing.T, c *client) {
	t.Helper()
	select {
	case <-c.removed:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: no removed message", c.name)
	}
	select {
	case <-c.closed:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: connection not closed after removal", c.name)
	}
}

// The server API removes a participant and closes a room; the clients are
// told and disconnected, and media stops.
func TestAPIRemoveParticipantAndCloseRoom(t *testing.T) {
	url := startServer(t)
	if s := apiCall(t, url, "POST", "/v1/rooms", `{"name":"room1","max_participants":3}`); s != http.StatusCreated {
		t.Fatalf("create room: %d", s)
	}

	alice := newClient(t, url, "alice")
	alice.join("room1")
	bob := newClient(t, url, "bob")
	bob.join("room1")
	carol := newClient(t, url, "carol")
	carol.join("room1")

	// The room is full (max 3).
	dave := newClient(t, url, "dave")
	dave.send(signalingJoin(t, "room1", "dave"))
	select {
	case msg := <-dave.errors:
		if msg != "room is full" {
			t.Errorf("dave error = %q, want room is full", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no error for a full room")
	}

	alice.publishVideo()
	eventually(t, "bob receives alice", func() bool { return bob.received("alice/media") >= 10 })

	if s := apiCall(t, url, "DELETE", "/v1/rooms/room1/participants/alice", ""); s != http.StatusNoContent {
		t.Fatalf("remove alice: %d", s)
	}
	expectRemoved(t, alice)
	eventually(t, "bob stops receiving alice", func() bool {
		before := bob.received("alice/media")
		time.Sleep(300 * time.Millisecond)
		return bob.received("alice/media") == before
	})

	if s := apiCall(t, url, "DELETE", "/v1/rooms/room1", ""); s != http.StatusNoContent {
		t.Fatalf("close room: %d", s)
	}
	expectRemoved(t, bob)
	expectRemoved(t, carol)
	if s := apiCall(t, url, "GET", "/v1/rooms/room1", ""); s != http.StatusNotFound {
		t.Errorf("closed room: %d, want 404", s)
	}
}

// flowing reports whether c keeps receiving packets of kind on stream.
func flowing(c *client, stream string, kind webrtc.RTPCodecType) bool {
	before := c.receivedKind(stream, kind)
	time.Sleep(400 * time.Millisecond)
	return c.receivedKind(stream, kind) > before
}

// The server API mutes a participant's microphone and changes permissions
// live; media stops and resumes accordingly, and the clients are told.
func TestAPIServerMuteAndPermissions(t *testing.T) {
	url := startServer(t)
	alice := newClient(t, url, "alice")
	alice.join("room1")
	bob := newClient(t, url, "bob")
	bob.join("room1")

	alice.publish(sfuinternal.SourceCamera, sfuinternal.SourceMicrophone)
	eventually(t, "bob receives alice's camera and microphone", func() bool {
		return bob.receivedKind("alice/media", webrtc.RTPCodecTypeVideo) >= 10 &&
			bob.receivedKind("alice/media", webrtc.RTPCodecTypeAudio) >= 10
	})

	// Server mute of the microphone: audio stops, video continues.
	if s := apiCall(t, url, "POST", "/v1/rooms/room1/participants/alice/mute", `{"source":"microphone","muted":true}`); s != http.StatusNoContent {
		t.Fatalf("mute: %d", s)
	}
	select {
	case msg := <-alice.mutes:
		if msg.Participant != "alice" || msg.Source != "microphone" || !*msg.Muted || !msg.ByServer {
			t.Errorf("alice got %+v, want her own server mute", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("alice was not told about the server mute")
	}
	eventually(t, "alice's audio stops for bob", func() bool { return !flowing(bob, "alice/media", webrtc.RTPCodecTypeAudio) })
	if !flowing(bob, "alice/media", webrtc.RTPCodecTypeVideo) {
		t.Error("video stopped too")
	}

	// Alice cannot unmute herself.
	f := false
	alice.send(signaling.Message{Type: signaling.TypeMute, Source: "microphone", Muted: &f})
	select {
	case msg := <-alice.errors:
		if msg != "muted by the server" {
			t.Errorf("error = %q", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no error when unmuting a server mute")
	}

	// The server unmutes: audio resumes.
	apiCall(t, url, "POST", "/v1/rooms/room1/participants/alice/mute", `{"source":"microphone","muted":false}`)
	eventually(t, "alice's audio resumes", func() bool { return flowing(bob, "alice/media", webrtc.RTPCodecTypeAudio) })

	// Permissions: alice may no longer publish the camera; bob may no longer subscribe.
	if s := apiCall(t, url, "PATCH", "/v1/rooms/room1/participants/alice", `{"grants":{"sources":["microphone"]}}`); s != http.StatusOK {
		t.Fatalf("patch alice: %d", s)
	}
	select {
	case g := <-alice.perms:
		if len(g.Sources) != 1 || g.Sources[0] != "microphone" {
			t.Errorf("alice permissions = %+v", g)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("alice was not told about her new permissions")
	}
	eventually(t, "alice's camera stops for bob", func() bool { return !flowing(bob, "alice/media", webrtc.RTPCodecTypeVideo) })

	if s := apiCall(t, url, "PATCH", "/v1/rooms/room1/participants/bob", `{"grants":{"subscribe":false}}`); s != http.StatusOK {
		t.Fatalf("patch bob: %d", s)
	}
	eventually(t, "bob receives nothing", func() bool { return !flowing(bob, "alice/media", webrtc.RTPCodecTypeAudio) })

	// Everything back on.
	apiCall(t, url, "PATCH", "/v1/rooms/room1/participants/alice", `{"grants":{"sources":[]}}`)
	apiCall(t, url, "PATCH", "/v1/rooms/room1/participants/bob", `{"grants":{"subscribe":true}}`)
	eventually(t, "bob receives alice's camera and microphone again", func() bool {
		return flowing(bob, "alice/media", webrtc.RTPCodecTypeVideo) && flowing(bob, "alice/media", webrtc.RTPCodecTypeAudio)
	})
}

// A server mute cannot be undone by reconnecting with the same identity.
func TestServerMuteSurvivesReconnect(t *testing.T) {
	url := startServer(t)
	bob := newClient(t, url, "bob")
	bob.join("room1")
	alice := newClient(t, url, "alice")
	alice.join("room1")
	alice.publish(sfuinternal.SourceCamera, sfuinternal.SourceMicrophone)
	eventually(t, "bob receives alice's audio", func() bool { return bob.receivedKind("alice/media", webrtc.RTPCodecTypeAudio) >= 10 })

	apiCall(t, url, "POST", "/v1/rooms/room1/participants/alice/mute", `{"source":"microphone","muted":true}`)
	eventually(t, "alice's audio stops", func() bool { return !flowing(bob, "alice/media", webrtc.RTPCodecTypeAudio) })

	alice.close()
	eventually(t, "alice is gone", func() bool {
		return apiCall(t, url, "DELETE", "/v1/rooms/room1/participants/alice", "") == http.StatusNotFound
	})

	again := newClient(t, url, "alice")
	again.join("room1")
	again.publish(sfuinternal.SourceCamera, sfuinternal.SourceMicrophone)
	eventually(t, "bob receives the new alice's camera", func() bool { return flowing(bob, "alice/media", webrtc.RTPCodecTypeVideo) })
	if flowing(bob, "alice/media", webrtc.RTPCodecTypeAudio) {
		t.Error("reconnecting undid the server mute: audio flows again")
	}
}
