// Package peer manages the lifecycle of a single WebRTC PeerConnection.
package peer

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/pion/webrtc/v4"
)

// maxPendingCandidates bounds how many remote ICE candidates are queued
// while waiting for the remote description.
const maxPendingCandidates = 64

// ErrTooManyPendingCandidates is returned when the pending candidate queue is full.
var ErrTooManyPendingCandidates = errors.New("too many ICE candidates before remote description")

// Peer wraps one PeerConnection. It negotiates as either side (HandleOffer
// when the client offers, CreateOffer/HandleAnswer when the server offers),
// queues early ICE candidates, logs state changes, and reports closure once.
type Peer struct {
	id     string
	pc     *webrtc.PeerConnection
	logger *slog.Logger

	mu        sync.Mutex
	remoteSet bool
	pending   []webrtc.ICECandidateInit
	onClose   func()

	closeOnce sync.Once
	closeErr  error
}

// New creates a Peer with a new PeerConnection built from api and cfg.
func New(api *webrtc.API, cfg webrtc.Configuration, logger *slog.Logger, id string) (*Peer, error) {
	pc, err := api.NewPeerConnection(cfg)
	if err != nil {
		return nil, fmt.Errorf("new peer connection: %w", err)
	}

	p := &Peer{
		id:     id,
		pc:     pc,
		logger: logger.With("peer_id", id),
	}

	// Pion starts this handler in a new goroutine that pc.Close does not wait
	// for, so calling Close here cannot deadlock. Closed must be handled too:
	// when the remote side closes DTLS, the state goes straight to Closed
	// without passing through Failed.
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		p.logger.Info("peer connection state changed", "state", state.String())
		switch state {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			_ = p.Close()
		}
	})

	p.logger.Debug("peer created")
	return p, nil
}

// ID returns the peer identifier.
func (p *Peer) ID() string { return p.id }

// OnICECandidate registers f to receive local ICE candidates for trickle ICE.
// Register it before HandleOffer, because gathering starts there.
func (p *Peer) OnICECandidate(f func(webrtc.ICECandidateInit)) {
	p.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil { // gathering finished
			return
		}
		f(c.ToJSON())
	})
}

// OnClose registers f to be called once when the peer closes or fails.
func (p *Peer) OnClose(f func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onClose = f
}

// HandleOffer applies a remote offer and returns the local answer.
func (p *Peer) HandleOffer(offer webrtc.SessionDescription) (webrtc.SessionDescription, error) {
	if offer.Type != webrtc.SDPTypeOffer {
		return webrtc.SessionDescription{}, fmt.Errorf("expected offer, got %s", offer.Type)
	}
	if err := p.pc.SetRemoteDescription(offer); err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("set remote description: %w", err)
	}
	p.flushPendingCandidates()

	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("create answer: %w", err)
	}
	if err := p.pc.SetLocalDescription(answer); err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("set local description: %w", err)
	}

	p.logger.Debug("offer answered")
	return answer, nil
}

// CreateOffer creates a local offer, applies it, and returns it.
// It is used when the server starts negotiation (the subscribe direction).
func (p *Peer) CreateOffer() (webrtc.SessionDescription, error) {
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("create offer: %w", err)
	}
	if err := p.pc.SetLocalDescription(offer); err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("set local description: %w", err)
	}

	p.logger.Debug("offer created")
	return offer, nil
}

// HandleAnswer applies the remote answer to an offer from CreateOffer.
func (p *Peer) HandleAnswer(answer webrtc.SessionDescription) error {
	if answer.Type != webrtc.SDPTypeAnswer {
		return fmt.Errorf("expected answer, got %s", answer.Type)
	}
	if err := p.pc.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("set remote description: %w", err)
	}
	p.flushPendingCandidates()

	p.logger.Debug("answer applied")
	return nil
}

// AddICECandidate adds a remote ICE candidate. Candidates that arrive before
// the remote description are queued and added once it is set.
func (p *Peer) AddICECandidate(c webrtc.ICECandidateInit) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.remoteSet {
		if len(p.pending) >= maxPendingCandidates {
			return ErrTooManyPendingCandidates
		}
		p.pending = append(p.pending, c)
		return nil
	}
	if err := p.pc.AddICECandidate(c); err != nil {
		return fmt.Errorf("add ICE candidate: %w", err)
	}
	return nil
}

// flushPendingCandidates marks the remote description as set and adds all
// queued candidates. A bad queued candidate is logged and skipped, so one
// invalid candidate does not fail the whole negotiation.
func (p *Peer) flushPendingCandidates() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.remoteSet = true
	for _, c := range p.pending {
		if err := p.pc.AddICECandidate(c); err != nil {
			p.logger.Warn("dropping queued ICE candidate", "err", err)
		}
	}
	p.pending = nil
}

// ConnectionState returns the current PeerConnection state.
func (p *Peer) ConnectionState() webrtc.PeerConnectionState {
	return p.pc.ConnectionState()
}

// Close closes the PeerConnection and calls the OnClose callback.
// It is safe to call more than once; only the first call has effect.
//
// OnClose runs after the close has finished, outside the once-guard, so the
// callback may itself call Close (for example through room cleanup) without
// deadlocking.
func (p *Peer) Close() error {
	first := false
	p.closeOnce.Do(func() {
		p.closeErr = p.pc.Close()
		first = true
	})
	if !first {
		return p.closeErr
	}

	p.mu.Lock()
	onClose := p.onClose
	p.mu.Unlock()

	p.logger.Info("peer closed")
	if onClose != nil {
		onClose()
	}
	return p.closeErr
}
