package peer

import (
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/mo-nada/sfu-engine/internal/transport"
)

func newTestPeer(t *testing.T) (*Peer, *webrtc.API) {
	t.Helper()
	api, err := transport.NewAPI()
	if err != nil {
		t.Fatalf("NewAPI: %v", err)
	}
	// No STUN servers: tests use host candidates only and never leave the machine.
	p, err := New(api, transport.Configuration(nil), slog.New(slog.NewTextHandler(io.Discard, nil)), "test-peer")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, api
}

// newClient creates a client PeerConnection that offers to receive one video track.
func newClient(t *testing.T, api *webrtc.API) *webrtc.PeerConnection {
	t.Helper()
	client, err := api.NewPeerConnection(transport.Configuration(nil))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatalf("add transceiver: %v", err)
	}
	return client
}

// connect negotiates client and server peer. If candidatesFirst is true, the
// client's ICE candidates are sent to the server before the offer, and the
// server's candidates are withheld from the client, so the connection can only
// succeed if the server used the queued candidates.
func connect(t *testing.T, server *Peer, client *webrtc.PeerConnection, candidatesFirst bool) {
	t.Helper()

	serverCandidates := make(chan webrtc.ICECandidateInit, 64)
	server.OnICECandidate(func(c webrtc.ICECandidateInit) { serverCandidates <- c })

	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	gathered := webrtc.GatheringCompletePromise(client)
	if err := client.SetLocalDescription(offer); err != nil {
		t.Fatalf("client set local: %v", err)
	}
	<-gathered

	// Send the client candidates separately (trickle style), using the gathered SDP.
	clientCandidates := extractCandidates(t, client)

	if candidatesFirst {
		for _, c := range clientCandidates {
			if err := server.AddICECandidate(c); err != nil {
				t.Fatalf("early AddICECandidate: %v", err)
			}
		}
	}

	answer, err := server.HandleOffer(offer)
	if err != nil {
		t.Fatalf("HandleOffer: %v", err)
	}

	if !candidatesFirst {
		for _, c := range clientCandidates {
			if err := server.AddICECandidate(c); err != nil {
				t.Fatalf("AddICECandidate: %v", err)
			}
		}
	}

	if err := client.SetRemoteDescription(answer); err != nil {
		t.Fatalf("client set remote: %v", err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			select {
			case c := <-serverCandidates:
				if !candidatesFirst {
					_ = client.AddICECandidate(c)
				}
			case <-done:
				return
			}
		}
	}()

	waitState(t, server, webrtc.PeerConnectionStateConnected)
}

// extractCandidates reads the ICE candidates from the client's local SDP.
func extractCandidates(t *testing.T, client *webrtc.PeerConnection) []webrtc.ICECandidateInit {
	t.Helper()
	parsed, err := client.LocalDescription().Unmarshal()
	if err != nil {
		t.Fatalf("parse SDP: %v", err)
	}
	var out []webrtc.ICECandidateInit
	mid := "0"
	for _, md := range parsed.MediaDescriptions {
		for _, a := range md.Attributes {
			if a.Key == "candidate" {
				out = append(out, webrtc.ICECandidateInit{Candidate: "candidate:" + a.Value, SDPMid: &mid})
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("client gathered no ICE candidates")
	}
	return out
}

func waitState(t *testing.T, p *Peer, want webrtc.PeerConnectionState) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if p.ConnectionState() == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("peer state = %s, want %s", p.ConnectionState(), want)
}

func TestHandleOfferConnects(t *testing.T) {
	server, api := newTestPeer(t)
	connect(t, server, newClient(t, api), false)
}

func TestEarlyCandidatesAreQueued(t *testing.T) {
	server, api := newTestPeer(t)
	connect(t, server, newClient(t, api), true)
}

func TestHandleOfferRejectsBadInput(t *testing.T) {
	server, _ := newTestPeer(t)

	if _, err := server.HandleOffer(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: "v=0"}); err == nil {
		t.Error("want error for answer passed as offer")
	}
	if _, err := server.HandleOffer(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "not sdp"}); err == nil {
		t.Error("want error for invalid SDP")
	}
}

func TestPendingCandidateLimit(t *testing.T) {
	server, _ := newTestPeer(t)
	c := webrtc.ICECandidateInit{Candidate: "candidate:1 1 udp 1 127.0.0.1 9 typ host"}
	for range maxPendingCandidates {
		if err := server.AddICECandidate(c); err != nil {
			t.Fatalf("AddICECandidate: %v", err)
		}
	}
	if err := server.AddICECandidate(c); !errors.Is(err, ErrTooManyPendingCandidates) {
		t.Errorf("err = %v, want ErrTooManyPendingCandidates", err)
	}
}

func TestCloseIsIdempotentAndNotifiesOnce(t *testing.T) {
	server, api := newTestPeer(t)
	var calls atomic.Int32
	server.OnClose(func() { calls.Add(1) })

	connect(t, server, newClient(t, api), false)

	if err := server.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	// Give any late state-change handlers time to run.
	time.Sleep(100 * time.Millisecond)

	if got := calls.Load(); got != 1 {
		t.Errorf("OnClose called %d times, want 1", got)
	}
	if got := server.ConnectionState(); got != webrtc.PeerConnectionStateClosed {
		t.Errorf("state = %s, want closed", got)
	}
}

func TestRemoteFailureClosesPeer(t *testing.T) {
	server, api := newTestPeer(t)
	closed := make(chan struct{})
	server.OnClose(func() { close(closed) })

	client := newClient(t, api)
	connect(t, server, client, false)

	// Closing the client makes the server connection disconnect and then fail.
	_ = client.Close()
	select {
	case <-closed:
	case <-time.After(60 * time.Second):
		t.Fatal("server peer did not close after client went away")
	}
}

// connectServerOffer negotiates with the server as the offerer. If
// candidatesFirst is true, the client's candidates reach the server before the
// answer and the server's candidates are withheld from the client.
func connectServerOffer(t *testing.T, server *Peer, client *webrtc.PeerConnection, candidatesFirst bool) {
	t.Helper()

	// The server needs something to send, otherwise the offer has no media section.
	if _, err := server.pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}); err != nil {
		t.Fatalf("add transceiver: %v", err)
	}

	serverCandidates := make(chan webrtc.ICECandidateInit, 64)
	server.OnICECandidate(func(c webrtc.ICECandidateInit) { serverCandidates <- c })

	offer, err := server.CreateOffer()
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := client.SetRemoteDescription(offer); err != nil {
		t.Fatalf("client set remote: %v", err)
	}
	answer, err := client.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("client create answer: %v", err)
	}
	gathered := webrtc.GatheringCompletePromise(client)
	if err := client.SetLocalDescription(answer); err != nil {
		t.Fatalf("client set local: %v", err)
	}
	<-gathered
	clientCandidates := extractCandidates(t, client)

	addAll := func() {
		for _, c := range clientCandidates {
			if err := server.AddICECandidate(c); err != nil {
				t.Fatalf("AddICECandidate: %v", err)
			}
		}
	}
	if candidatesFirst {
		addAll()
	}
	if err := server.HandleAnswer(answer); err != nil {
		t.Fatalf("HandleAnswer: %v", err)
	}
	if !candidatesFirst {
		addAll()
		for len(serverCandidates) > 0 {
			_ = client.AddICECandidate(<-serverCandidates)
		}
	}

	waitState(t, server, webrtc.PeerConnectionStateConnected)
}

func TestCreateOfferConnects(t *testing.T) {
	server, api := newTestPeer(t)
	client, err := api.NewPeerConnection(transport.Configuration(nil))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	connectServerOffer(t, server, client, false)
}

func TestCreateOfferEarlyCandidatesAreQueued(t *testing.T) {
	server, api := newTestPeer(t)
	client, err := api.NewPeerConnection(transport.Configuration(nil))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	connectServerOffer(t, server, client, true)
}

func TestHandleAnswerRejectsBadInput(t *testing.T) {
	server, _ := newTestPeer(t)

	if err := server.HandleAnswer(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "v=0"}); err == nil {
		t.Error("want error for offer passed as answer")
	}
	// An answer without a pending local offer is invalid.
	if err := server.HandleAnswer(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: "v=0"}); err == nil {
		t.Error("want error for answer without offer")
	}
}
