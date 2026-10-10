package integration

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	sfuinternal "github.com/mo-nada/sfu-engine/internal/sfu"
	"github.com/mo-nada/sfu-engine/internal/signaling"
	"github.com/mo-nada/sfu-engine/pkg/sfu"
	"github.com/mo-nada/sfu-engine/pkg/token"
)

// client is a minimal SFU client, like a browser would be: one WebSocket and
// two PeerConnections (publisher: client offers; subscriber: server offers).
type client struct {
	t    *testing.T
	name string
	conn *websocket.Conn

	writeMu sync.Mutex
	closing atomic.Bool // set by close; errors after this are expected

	pub, sub *webrtc.PeerConnection
	pending  map[string][]webrtc.ICECandidateInit // per target, before remote description
	pendMu   sync.Mutex

	joined chan struct{}
	errors chan string
	closed chan struct{}

	mu      sync.Mutex
	offers  int            // subscriber offers received
	packets map[string]int // stream ID (publisher name) -> RTP packets received
	tracks  map[string]int // stream ID -> tracks currently attached

	published   map[string]publishedTrack // source -> our sending track
	kindPackets map[string]int            // "stream|kind" -> RTP packets received
}

type publishedTrack struct {
	sender *webrtc.RTPSender
	stop   chan struct{}
}

func newClient(t *testing.T, wsURL, name string) *client {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("%s: dial: %v", name, err)
	}
	c := &client{
		t: t, name: name, conn: conn,
		pending: make(map[string][]webrtc.ICECandidateInit),
		joined:  make(chan struct{}),
		errors:  make(chan string, 16),
		closed:  make(chan struct{}),
		packets: make(map[string]int),
		tracks:  make(map[string]int),

		published:   make(map[string]publishedTrack),
		kindPackets: make(map[string]int),
	}
	c.pub = c.newPC(signaling.TargetPublisher)
	c.sub = c.newPC(signaling.TargetSubscriber)
	c.sub.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		from := remote.StreamID()
		c.mu.Lock()
		c.tracks[from]++
		c.mu.Unlock()
		for {
			if _, _, err := remote.ReadRTP(); err != nil {
				c.mu.Lock()
				c.tracks[from]--
				c.mu.Unlock()
				return
			}
			c.mu.Lock()
			c.packets[from]++
			c.kindPackets[from+"|"+remote.Kind().String()]++
			c.mu.Unlock()
		}
	})
	t.Cleanup(c.close)
	go c.readLoop()
	return c
}

func (c *client) newPC(target string) *webrtc.PeerConnection {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		c.t.Fatalf("%s: new pc: %v", c.name, err)
	}
	pc.OnICECandidate(func(cand *webrtc.ICECandidate) {
		if cand != nil {
			ci := cand.ToJSON()
			c.send(signaling.Message{Type: signaling.TypeCandidate, Target: target, Candidate: &ci})
		}
	})
	return pc
}

func (c *client) send(msg signaling.Message) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.WriteJSON(msg)
}

func (c *client) readLoop() {
	defer close(c.closed)
	for {
		var msg signaling.Message
		if err := c.conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg.Type {
		case signaling.TypeJoined:
			close(c.joined)
		case signaling.TypeAnswer: // answer to our publisher offer
			c.setRemote(signaling.TargetPublisher, c.pub, webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: msg.SDP})
		case signaling.TypeOffer: // server offer on the subscriber connection
			c.mu.Lock()
			c.offers++
			c.mu.Unlock()
			c.setRemote(signaling.TargetSubscriber, c.sub, webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: msg.SDP})
			answer, err := c.sub.CreateAnswer(nil)
			if err == nil {
				err = c.sub.SetLocalDescription(answer)
			}
			if err != nil {
				c.fail("answer: %v", err)
				continue
			}
			c.send(signaling.Message{Type: signaling.TypeAnswer, SDP: answer.SDP})
		case signaling.TypeCandidate:
			c.addCandidate(msg.Target, *msg.Candidate)
		case signaling.TypeError:
			c.errors <- msg.Message
		}
	}
}

func (c *client) pcFor(target string) *webrtc.PeerConnection {
	if target == signaling.TargetPublisher {
		return c.pub
	}
	return c.sub
}

func (c *client) addCandidate(target string, ci webrtc.ICECandidateInit) {
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	pc := c.pcFor(target)
	if pc.RemoteDescription() == nil {
		c.pending[target] = append(c.pending[target], ci)
		return
	}
	_ = pc.AddICECandidate(ci)
}

func (c *client) setRemote(target string, pc *webrtc.PeerConnection, sd webrtc.SessionDescription) {
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	if err := pc.SetRemoteDescription(sd); err != nil {
		c.fail("set remote (%s): %v", target, err)
		return
	}
	for _, ci := range c.pending[target] {
		_ = pc.AddICECandidate(ci)
	}
	c.pending[target] = nil
}

// Test tenants. acme is used unless a test picks another one.
var (
	acme   = sfu.Tenant{ID: "acme", APIKey: "acme-key", Secret: "acme-secret-acme-secret-acme-sec"}
	globex = sfu.Tenant{ID: "globex", APIKey: "globex-key", Secret: "globex-secret-globex-secret-glob"}
)

// joinToken signs a token for name in room for tenant, with grants.
func joinToken(t *testing.T, tenant sfu.Tenant, room, name string, grants token.Grants) string {
	t.Helper()
	c := token.New(tenant.APIKey, name, room, time.Minute)
	c.Grants = grants
	tok, err := token.Sign(tenant.Secret, c)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return tok
}

var allGrants = token.Grants{Publish: true, Subscribe: true}

// join joins room as tenant acme with all permissions.
func (c *client) join(room string) { c.joinWith(acme, room, allGrants) }

func (c *client) joinWith(tenant sfu.Tenant, room string, grants token.Grants) {
	c.t.Helper()
	c.send(signaling.Message{Type: signaling.TypeJoin, Token: joinToken(c.t, tenant, room, c.name, grants)})
	select {
	case <-c.joined:
	case msg := <-c.errors:
		c.t.Fatalf("%s: join error: %s", c.name, msg)
	case <-time.After(5 * time.Second):
		c.t.Fatalf("%s: join timed out", c.name)
	}
}

// publishVideo publishes one camera track.
func (c *client) publishVideo() { c.publish(sfuinternal.SourceCamera) }

// publish adds one track per source (VP8 for video sources, Opus for audio
// sources), offers them with their sources, and sends fake media until the
// track is unpublished or the client closes.
func (c *client) publish(sources ...string) {
	c.t.Helper()
	for _, source := range sources {
		capability := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}
		sample := media.Sample{Data: []byte{0x10, 0x00, 0x00, 0x9d, 0x01, 0x2a}, Duration: 20 * time.Millisecond}
		if source == sfuinternal.SourceMicrophone || source == sfuinternal.SourceScreenAudio {
			capability = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
			sample = media.Sample{Data: []byte{0xfc, 0xff, 0xfe}, Duration: 20 * time.Millisecond}
		}
		track, err := webrtc.NewTrackLocalStaticSample(capability, source, "local")
		if err != nil {
			c.t.Fatalf("%s: track: %v", c.name, err)
		}
		sender, err := c.pub.AddTrack(track)
		if err != nil {
			c.t.Fatalf("%s: add track: %v", c.name, err)
		}
		stop := make(chan struct{})
		c.mu.Lock()
		c.published[source] = publishedTrack{sender: sender, stop: stop}
		c.mu.Unlock()

		go func() {
			tick := time.NewTicker(20 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-c.closed:
					return
				case <-stop:
					return
				case <-tick.C:
					_ = track.WriteSample(sample)
				}
			}
		}()
	}
	c.offer()
}

// unpublish removes the track for source and renegotiates, like a browser
// that stops a screen share.
func (c *client) unpublish(source string) {
	c.t.Helper()
	c.mu.Lock()
	pt, ok := c.published[source]
	delete(c.published, source)
	c.mu.Unlock()
	if !ok {
		c.t.Fatalf("%s: %s not published", c.name, source)
	}
	close(pt.stop)
	if err := c.pub.RemoveTrack(pt.sender); err != nil {
		c.t.Fatalf("%s: remove track: %v", c.name, err)
	}
	c.offer()
}

// offer sends a publisher offer that declares the source of every track.
// The track ID is the source name, so the map is source -> source.
func (c *client) offer() {
	c.t.Helper()
	tracks := make(map[string]string)
	c.mu.Lock()
	for source := range c.published {
		tracks[source] = source
	}
	c.mu.Unlock()

	offer, err := c.pub.CreateOffer(nil)
	if err != nil {
		c.t.Fatalf("%s: offer: %v", c.name, err)
	}
	if err := c.pub.SetLocalDescription(offer); err != nil {
		c.t.Fatalf("%s: set local: %v", c.name, err)
	}
	c.send(signaling.Message{Type: signaling.TypeOffer, SDP: offer.SDP, Tracks: tracks})
}

// receivedKind returns RTP packets received on stream for one track kind.
func (c *client) receivedKind(stream string, kind webrtc.RTPCodecType) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.kindPackets[stream+"|"+kind.String()]
}

// activeTracks returns how many tracks of stream are currently being read.
func (c *client) activeTracks(stream string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tracks[stream]
}

func (c *client) received(from string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.packets[from]
}

func (c *client) offerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offers
}

// fail reports an error from a background goroutine, unless the client is
// already closing (then the server may still be renegotiating with it).
func (c *client) fail(format string, args ...any) {
	if c.closing.Load() {
		return
	}
	c.t.Errorf(c.name+": "+format, args...)
}

// close disconnects the WebSocket and both PeerConnections, like closing a browser tab.
func (c *client) close() {
	c.closing.Store(true)
	_ = c.conn.Close()
	_ = c.pub.Close()
	_ = c.sub.Close()
}

func wsURL(addr string) string {
	return "ws://" + strings.TrimPrefix(addr, "http://") + "/ws"
}

func signalingJoin(t *testing.T, room, name string) signaling.Message {
	return signaling.Message{Type: signaling.TypeJoin, Token: joinToken(t, acme, room, name, allGrants)}
}
