// Package transport builds the Pion WebRTC API and ICE configuration used by all peers.
package transport

import (
	"fmt"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

// NewAPI returns a Pion API with the default codecs (VP8, VP9, H.264, Opus, ...)
// and the default interceptors (NACK, RTCP reports, TWCC).
//
// One API is shared by all PeerConnections; Pion copies the media engine
// for each new PeerConnection.
func NewAPI() (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("register codecs: %w", err)
	}

	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, fmt.Errorf("register interceptors: %w", err)
	}

	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir)), nil
}

// Configuration returns the PeerConnection configuration for the given STUN URLs.
// An empty list means only host candidates are gathered.
func Configuration(stunURLs []string) webrtc.Configuration {
	var cfg webrtc.Configuration
	if len(stunURLs) > 0 {
		cfg.ICEServers = []webrtc.ICEServer{{URLs: stunURLs}}
	}
	return cfg
}
