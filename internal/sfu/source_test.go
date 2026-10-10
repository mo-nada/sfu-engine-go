package sfu

import (
	"testing"

	"github.com/pion/webrtc/v4"
)

func TestNormalizeSource(t *testing.T) {
	tests := []struct {
		source string
		kind   webrtc.RTPCodecType
		want   string
	}{
		{SourceScreen, webrtc.RTPCodecTypeVideo, SourceScreen},
		{SourceScreenAudio, webrtc.RTPCodecTypeAudio, SourceScreenAudio},
		{"", webrtc.RTPCodecTypeVideo, SourceCamera},
		{"", webrtc.RTPCodecTypeAudio, SourceMicrophone},
		{"hologram", webrtc.RTPCodecTypeVideo, SourceCamera},
	}
	for _, tt := range tests {
		if got := NormalizeSource(tt.source, tt.kind); got != tt.want {
			t.Errorf("NormalizeSource(%q, %s) = %q, want %q", tt.source, tt.kind, got, tt.want)
		}
	}
}

func TestStreamID(t *testing.T) {
	tests := map[string]string{
		SourceCamera:      "alice/media",
		SourceMicrophone:  "alice/media",
		SourceScreen:      "alice/screen",
		SourceScreenAudio: "alice/screen",
	}
	for source, want := range tests {
		if got := StreamID("alice", source); got != want {
			t.Errorf("StreamID(alice, %s) = %q, want %q", source, got, want)
		}
	}
}
