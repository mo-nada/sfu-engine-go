// Package integration runs real WebRTC clients against a real SFU server
// through the public pkg/sfu API and the WebSocket signaling protocol.
package integration

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/mo-nada/sfu-engine/pkg/sfu"
)

// startServer runs an Engine on a random local port without STUN and returns
// its WebSocket URL. The engine shuts down when the test ends.
func startServer(t *testing.T) string {
	t.Helper()
	eng, err := sfu.New(
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

	eventually(t, "bob receives alice's video", func() bool { return bob.received("alice") >= 20 })
	if n := alice.received("alice"); n != 0 {
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
		return alice.received("bob") >= 10 &&
			bob.received("alice") >= 10 &&
			carol.received("alice") >= 10 && carol.received("bob") >= 10
	})
	time.Sleep(200 * time.Millisecond)
	if n := dave.received("alice") + dave.received("bob"); n != 0 {
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
	eventually(t, "bob receives alice's video", func() bool { return bob.received("alice") >= 10 })

	offersBefore := bob.offerCount()
	alice.close()

	eventually(t, "bob gets a new offer without alice's track", func() bool { return bob.offerCount() > offersBefore })
	eventually(t, "bob stops receiving alice's video", func() bool {
		before := bob.received("alice")
		time.Sleep(300 * time.Millisecond)
		return bob.received("alice") == before
	})

	// The participant ID is free again.
	again := newClient(t, url, "alice")
	again.join("room1")
	again.publishVideo()
	eventually(t, "bob receives the new alice", func() bool { return bob.received("alice") >= 20 })
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
	eventually(t, "bob still receives alice's video", func() bool { return bob.received("alice") >= 20 })
}

// A duplicate participant name in the same room is rejected over signaling.
func TestDuplicateNameRejected(t *testing.T) {
	url := startServer(t)

	first := newClient(t, url, "alice")
	first.join("room1")

	second := newClient(t, url, "alice")
	second.send(signalingJoin("room1", "alice"))
	select {
	case msg := <-second.errors:
		if msg != "participant already in room" {
			t.Errorf("error = %q", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no error for duplicate name")
	}
}
