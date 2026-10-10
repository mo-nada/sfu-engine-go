// Package sfu routes published media tracks to subscribers without decoding.
package sfu

import (
	"errors"
	"log/slog"
	"sync"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/mo-nada/sfu-engine/internal/peer"
)

var (
	// ErrRouterClosed is returned when publishing after Close.
	ErrRouterClosed = errors.New("router closed")
	// ErrTrackExists is returned when the publisher already published a track with this ID.
	ErrTrackExists = errors.New("track already published")
)

// Router forwards every published track in one room to every other
// participant's subscribe peer.
//
// Each published track has one shared TrackLocalStaticRTP. It is added to all
// subscribers, and one WriteRTP sends a packet to all of them; Pion rewrites
// the SSRC and payload type per subscriber.
//
// The Router does not own peers: it never closes them. A published track is
// removed when reading from the publisher fails, which happens when the
// publisher's peer closes.
type Router struct {
	logger *slog.Logger

	mu          sync.Mutex
	subscribers map[string]*peer.Peer      // participant ID -> subscribe peer
	tracks      map[string]*publishedTrack // track key -> track
	closed      bool
}

type publishedTrack struct {
	key         string
	publisherID string
	publisher   *peer.Peer
	remote      *webrtc.TrackRemote
	local       *webrtc.TrackLocalStaticRTP
	senders     map[string]*webrtc.RTPSender // subscriber ID -> sender
}

// NewRouter creates an empty Router.
func NewRouter(logger *slog.Logger) *Router {
	return &Router{
		logger:      logger,
		subscribers: make(map[string]*peer.Peer),
		tracks:      make(map[string]*publishedTrack),
	}
}

// Publish starts forwarding remote, received on publisherID's publish peer,
// to every other subscriber. source says what the track is (see Source*);
// an unknown or empty source is replaced by the default for the track kind.
// It returns once forwarding has started.
//
// Forwarding stops, and subscribers are renegotiated, when reading from
// remote fails: when the publisher's peer closes, or when the publisher
// removes the track and renegotiates.
func (r *Router) Publish(publisherID, source string, publisher *peer.Peer, remote *webrtc.TrackRemote) error {
	source = NormalizeSource(source, remote.Kind())
	key := publisherID + "/" + remote.ID()
	local, err := webrtc.NewTrackLocalStaticRTP(remote.Codec().RTPCodecCapability, key, StreamID(publisherID, source))
	if err != nil {
		return err
	}
	t := &publishedTrack{
		key:         key,
		publisherID: publisherID,
		publisher:   publisher,
		remote:      remote,
		local:       local,
		senders:     make(map[string]*webrtc.RTPSender),
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrRouterClosed
	}
	if _, exists := r.tracks[key]; exists {
		r.mu.Unlock()
		return ErrTrackExists
	}
	r.tracks[key] = t
	var changed []*peer.Peer
	for subID, sub := range r.subscribers {
		if subID != publisherID && r.attach(t, subID, sub) {
			changed = append(changed, sub)
		}
	}
	r.mu.Unlock()

	r.logger.Info("track published", "track", key, "source", source, "kind", remote.Kind().String(), "codec", remote.Codec().MimeType)
	go r.forward(t)
	r.negotiate(changed)
	if len(changed) > 0 {
		r.requestKeyframe(t)
	}
	return nil
}

// AddSubscriber registers a participant's subscribe peer and sends it every
// track already published by other participants.
func (r *Router) AddSubscriber(participantID string, sub *peer.Peer) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.subscribers[participantID] = sub
	var attached []*publishedTrack
	for _, t := range r.tracks {
		if t.publisherID != participantID && r.attach(t, participantID, sub) {
			attached = append(attached, t)
		}
	}
	r.mu.Unlock()

	r.logger.Info("subscriber added", "participant_id", participantID, "tracks", len(attached))
	if len(attached) > 0 {
		r.negotiate([]*peer.Peer{sub})
		for _, t := range attached {
			r.requestKeyframe(t)
		}
	}
}

// RemoveSubscriber forgets a participant's subscribe peer. The caller closes
// the peer; its senders stop with it.
func (r *Router) RemoveSubscriber(participantID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.subscribers, participantID)
	for _, t := range r.tracks {
		delete(t.senders, participantID)
	}
}

// Close stops routing. Forwarding goroutines end when their publisher peers close.
func (r *Router) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.subscribers = make(map[string]*peer.Peer)
	r.tracks = make(map[string]*publishedTrack)
}

// attach adds t to sub and starts reading RTCP from the new sender.
// It must be called with r.mu held.
func (r *Router) attach(t *publishedTrack, subID string, sub *peer.Peer) bool {
	sender, err := sub.AddTrack(t.local)
	if err != nil {
		r.logger.Warn("attach track failed", "track", t.key, "subscriber_id", subID, "err", err)
		return false
	}
	t.senders[subID] = sender
	go r.readRTCP(t, sender)
	return true
}

// forward copies RTP packets from the publisher to the shared local track
// until the publisher's track ends, then unpublishes it.
func (r *Router) forward(t *publishedTrack) {
	for {
		pkt, _, err := t.remote.ReadRTP()
		if err != nil {
			break
		}
		stripExtensions(pkt)
		// A write error only means one subscriber failed; the others still get the packet.
		_ = t.local.WriteRTP(pkt)
	}
	r.unpublish(t)
}

// stripExtensions removes the RTP header extensions. Their IDs were
// negotiated between the publisher and the SFU and can mean something else
// on a subscriber's connection, where the browser would misread them (for
// example as timing data). Nothing downstream needs them.
func stripExtensions(pkt *rtp.Packet) {
	pkt.Header.Extension = false
	pkt.Header.ExtensionProfile = 0
	pkt.Header.Extensions = nil
}

// unpublish removes t from every subscriber and renegotiates them.
func (r *Router) unpublish(t *publishedTrack) {
	r.mu.Lock()
	if r.tracks[t.key] != t {
		r.mu.Unlock()
		return
	}
	delete(r.tracks, t.key)
	var changed []*peer.Peer
	for subID, sender := range t.senders {
		sub, ok := r.subscribers[subID]
		if !ok {
			continue
		}
		if err := sub.RemoveTrack(sender); err != nil {
			r.logger.Warn("remove track failed", "track", t.key, "subscriber_id", subID, "err", err)
			continue
		}
		changed = append(changed, sub)
	}
	r.mu.Unlock()

	r.logger.Info("track unpublished", "track", t.key)
	r.negotiate(changed)
}

// readRTCP reads feedback from one subscriber. Reading is required for Pion's
// interceptors (NACK, reports) to work. Keyframe requests are passed on to the
// publisher. It returns when the sender is removed or its peer closes.
func (r *Router) readRTCP(t *publishedTrack, sender *webrtc.RTPSender) {
	for {
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		for _, pkt := range pkts {
			switch pkt.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				r.requestKeyframe(t)
			}
		}
	}
}

// requestKeyframe asks the publisher for a new keyframe so subscribers can
// start decoding. Audio tracks have no keyframes.
func (r *Router) requestKeyframe(t *publishedTrack) {
	if t.remote.Kind() != webrtc.RTPCodecTypeVideo {
		return
	}
	pli := &rtcp.PictureLossIndication{MediaSSRC: uint32(t.remote.SSRC())}
	if err := t.publisher.WriteRTCP([]rtcp.Packet{pli}); err != nil {
		r.logger.Debug("keyframe request failed", "track", t.key, "err", err)
	}
}

// negotiate sends a new offer to each peer. It must be called without r.mu
// held, because the OnOffer callback may block on signaling.
func (r *Router) negotiate(peers []*peer.Peer) {
	for _, p := range peers {
		if err := p.Negotiate(); err != nil {
			r.logger.Warn("renegotiation failed", "peer_id", p.ID(), "err", err)
		}
	}
}
