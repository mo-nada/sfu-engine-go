package sfu

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/mo-nada/sfu-engine/internal/peer"
	"github.com/mo-nada/sfu-engine/internal/transport"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testClient is a client-side PeerConnection that queues remote ICE
// candidates until its remote description is set.
type testClient struct {
	pc      *webrtc.PeerConnection
	mu      sync.Mutex
	pending []webrtc.ICECandidateInit
}

func newTestClient(t *testing.T, api *webrtc.API) *testClient {
	t.Helper()
	pc, err := api.NewPeerConnection(transport.Configuration(nil))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return &testClient{pc: pc}
}

func (c *testClient) addCandidate(ci webrtc.ICECandidateInit) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pc.RemoteDescription() == nil {
		c.pending = append(c.pending, ci)
		return
	}
	_ = c.pc.AddICECandidate(ci)
}

func (c *testClient) setRemote(t *testing.T, sd webrtc.SessionDescription) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.pc.SetRemoteDescription(sd); err != nil {
		t.Errorf("client set remote: %v", err)
		return
	}
	for _, ci := range c.pending {
		_ = c.pc.AddICECandidate(ci)
	}
	c.pending = nil
}

// newServerPeer creates a server-side peer wired to client for trickle ICE.
func newServerPeer(t *testing.T, api *webrtc.API, client *testClient, id string) *peer.Peer {
	t.Helper()
	p, err := peer.New(api, transport.Configuration(nil), discardLogger(), id)
	if err != nil {
		t.Fatalf("peer.New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	p.OnICECandidate(client.addCandidate)
	client.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = p.AddICECandidate(c.ToJSON())
		}
	})
	return p
}

// publisher connects a client that sends one VP8 track and passes the
// server-side track to the router.
type publisher struct {
	client *testClient
	peer   *peer.Peer
	track  *webrtc.TrackLocalStaticSample
	sender *webrtc.RTPSender
	remote chan *webrtc.TrackRemote // server-side track, sent once published
}

func newPublisher(t *testing.T, api *webrtc.API, r *Router, id string) *publisher {
	t.Helper()
	client := newTestClient(t, api)
	srv := newServerPeer(t, api, client, id+"-pub")
	remotes := make(chan *webrtc.TrackRemote, 1)
	srv.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if err := r.Publish(id, srv, remote); err != nil {
			t.Errorf("Publish: %v", err)
		}
		remotes <- remote
	})

	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "cam", id)
	if err != nil {
		t.Fatalf("track: %v", err)
	}
	sender, err := client.pc.AddTrack(track)
	if err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	offer, err := client.pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	if err := client.pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local: %v", err)
	}
	answer, err := srv.HandleOffer(offer)
	if err != nil {
		t.Fatalf("HandleOffer: %v", err)
	}
	client.setRemote(t, answer)
	return &publisher{client: client, peer: srv, track: track, sender: sender, remote: remotes}
}

// sendVideo writes fake VP8 samples until stop is closed.
func (p *publisher) sendVideo(stop <-chan struct{}) {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			_ = p.track.WriteSample(media.Sample{Data: []byte{0x10, 0x00, 0x00, 0x9d, 0x01, 0x2a}, Duration: 20 * time.Millisecond})
		}
	}
}

// subscriber connects a client that receives tracks from the router and
// counts the RTP packets it gets from each publisher.
type subscriber struct {
	client  *testClient
	peer    *peer.Peer
	offers  atomic.Int32
	mu      sync.Mutex
	packets map[string]int // publisher (stream ID) -> packets received
}

func newSubscriber(t *testing.T, api *webrtc.API, r *Router, id string) *subscriber {
	t.Helper()
	client := newTestClient(t, api)
	s := &subscriber{client: client, packets: make(map[string]int)}
	s.peer = newServerPeer(t, api, client, id+"-sub")

	s.peer.OnOffer(func(offer webrtc.SessionDescription) {
		s.offers.Add(1)
		go func() {
			client.setRemote(t, offer)
			answer, err := client.pc.CreateAnswer(nil)
			if err != nil {
				t.Errorf("answer: %v", err)
				return
			}
			if err := client.pc.SetLocalDescription(answer); err != nil {
				t.Errorf("set local: %v", err)
				return
			}
			if err := s.peer.HandleAnswer(answer); err != nil {
				t.Errorf("HandleAnswer: %v", err)
			}
		}()
	})
	client.pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			if _, _, err := remote.ReadRTP(); err != nil {
				return
			}
			s.mu.Lock()
			s.packets[remote.StreamID()]++
			s.mu.Unlock()
		}
	})
	r.AddSubscriber(id, s.peer)
	return s
}

func (s *subscriber) received(from string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.packets[from]
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func (r *Router) trackCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.tracks)
}

// Alice publishes video. Bob subscribes before she publishes, Carol after.
// Both must receive her packets; Alice must not receive her own track. When
// Alice leaves, her track is removed and the others are renegotiated.
func TestForwardsVideoToAllOtherSubscribers(t *testing.T) {
	api, err := transport.NewAPI()
	if err != nil {
		t.Fatalf("NewAPI: %v", err)
	}
	r := NewRouter(discardLogger())
	t.Cleanup(r.Close)

	aliceSub := newSubscriber(t, api, r, "alice")
	bob := newSubscriber(t, api, r, "bob")
	alice := newPublisher(t, api, r, "alice")

	stop := make(chan struct{})
	defer close(stop)
	go alice.sendVideo(stop)

	eventually(t, "bob receives alice's video", func() bool { return bob.received("alice") >= 10 })

	carol := newSubscriber(t, api, r, "carol")
	eventually(t, "carol (late joiner) receives alice's video", func() bool { return carol.received("alice") >= 10 })

	if n := aliceSub.received("alice"); n != 0 {
		t.Errorf("alice received %d packets of her own track, want 0", n)
	}
	if n := aliceSub.offers.Load(); n != 0 {
		t.Errorf("alice's subscriber got %d offers, want 0", n)
	}

	bobOffers, carolOffers := bob.offers.Load(), carol.offers.Load()
	_ = alice.peer.Close()

	eventually(t, "alice's track is unpublished", func() bool { return r.trackCount() == 0 })
	eventually(t, "bob and carol are renegotiated", func() bool {
		return bob.offers.Load() > bobOffers && carol.offers.Load() > carolOffers
	})
}

// A new subscriber must trigger a keyframe request (PLI) to the publisher.
func TestNewSubscriberRequestsKeyframe(t *testing.T) {
	api, err := transport.NewAPI()
	if err != nil {
		t.Fatalf("NewAPI: %v", err)
	}
	r := NewRouter(discardLogger())
	t.Cleanup(r.Close)

	alice := newPublisher(t, api, r, "alice")
	var plis atomic.Int32
	go func() {
		for {
			pkts, _, err := alice.sender.ReadRTCP()
			if err != nil {
				return
			}
			for _, p := range pkts {
				if _, ok := p.(*rtcp.PictureLossIndication); ok {
					plis.Add(1)
				}
			}
		}
	}()

	stop := make(chan struct{})
	defer close(stop)
	go alice.sendVideo(stop)
	eventually(t, "track published", func() bool { return r.trackCount() == 1 })

	before := plis.Load()
	bob := newSubscriber(t, api, r, "bob")
	eventually(t, "bob receives video", func() bool { return bob.received("alice") > 0 })
	eventually(t, "publisher receives a PLI", func() bool { return plis.Load() > before })
}

func TestAddSubscriberAfterCloseIsIgnored(t *testing.T) {
	r := NewRouter(discardLogger())
	r.Close()
	r.AddSubscriber("bob", nil) // ignored after Close
	if len(r.subscribers) != 0 {
		t.Error("subscriber added after Close")
	}
}

func TestPublishSameTrackTwiceFails(t *testing.T) {
	api, err := transport.NewAPI()
	if err != nil {
		t.Fatalf("NewAPI: %v", err)
	}
	r := NewRouter(discardLogger())
	t.Cleanup(r.Close)

	alice := newPublisher(t, api, r, "alice")
	stop := make(chan struct{})
	defer close(stop)
	go alice.sendVideo(stop)

	var remote *webrtc.TrackRemote
	select {
	case remote = <-alice.remote:
	case <-time.After(10 * time.Second):
		t.Fatal("track not published")
	}
	if err := r.Publish("alice", alice.peer, remote); !errors.Is(err, ErrTrackExists) {
		t.Errorf("err = %v, want ErrTrackExists", err)
	}
	if n := r.trackCount(); n != 1 {
		t.Errorf("tracks = %d, want 1", n)
	}
}
