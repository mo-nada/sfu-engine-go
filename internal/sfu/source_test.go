package sfu

import (
	"bytes"
	"testing"

	"github.com/pion/rtp"
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

func TestStripExtensions(t *testing.T) {
	pkt := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 7, Timestamp: 90, SSRC: 1}, Payload: []byte{1, 2, 3}}
	if err := pkt.Header.SetExtension(3, []byte{0xaa, 0xbb}); err != nil {
		t.Fatal(err)
	}

	stripExtensions(pkt)

	raw, err := pkt.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got rtp.Packet
	if err := got.Unmarshal(raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Header.Extension || len(got.Header.Extensions) != 0 {
		t.Errorf("extensions still present: %+v", got.Header)
	}
	if got.SequenceNumber != 7 || got.Timestamp != 90 || !bytes.Equal(got.Payload, []byte{1, 2, 3}) {
		t.Errorf("packet changed: seq=%d ts=%d payload=%v", got.SequenceNumber, got.Timestamp, got.Payload)
	}
}
