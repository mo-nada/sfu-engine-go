package sfu

import "github.com/pion/webrtc/v4"

// Track sources, as declared by the publishing client.
const (
	SourceCamera      = "camera"
	SourceMicrophone  = "microphone"
	SourceScreen      = "screen"
	SourceScreenAudio = "screen-audio"
)

// Sources lists every track source.
var Sources = []string{SourceCamera, SourceMicrophone, SourceScreen, SourceScreenAudio}

// ValidSource reports whether s is a known track source.
func ValidSource(s string) bool {
	switch s {
	case SourceCamera, SourceMicrophone, SourceScreen, SourceScreenAudio:
		return true
	}
	return false
}

// NormalizeSource returns source if it is known, otherwise the default for
// kind: camera for video, microphone for audio.
func NormalizeSource(source string, kind webrtc.RTPCodecType) string {
	if ValidSource(source) {
		return source
	}
	if kind == webrtc.RTPCodecTypeAudio {
		return SourceMicrophone
	}
	return SourceCamera
}

// StreamID returns the stream ID subscribers see for a forwarded track:
// "<participant>/media" for camera and microphone, so a player keeps them in
// sync, and "<participant>/screen" for screen and screen audio.
// Participant IDs never contain "/", so clients can split on it.
func StreamID(participantID, source string) string {
	if source == SourceScreen || source == SourceScreenAudio {
		return participantID + "/screen"
	}
	return participantID + "/media"
}
