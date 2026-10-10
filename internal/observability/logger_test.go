package observability

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestNewLoggerJSON(t *testing.T) {
	var buf bytes.Buffer
	logger, err := NewLogger(&buf, "info", "json")
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	logger.Debug("hidden")
	logger.Info("room created", "room_id", "r1")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("output is not a single JSON line: %v\n%s", err, buf.String())
	}
	if entry["msg"] != "room created" || entry["room_id"] != "r1" {
		t.Errorf("unexpected entry: %v", entry)
	}
}

func TestNewLoggerText(t *testing.T) {
	var buf bytes.Buffer
	logger, err := NewLogger(&buf, "debug", "text")
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	logger.Debug("peer connected", "peer_id", "p1")
	out := buf.String()
	if !strings.Contains(out, "peer connected") || !strings.Contains(out, "peer_id=p1") {
		t.Errorf("unexpected output: %s", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("output to a non-terminal must not contain color codes: %q", out)
	}
}

func TestUseColor(t *testing.T) {
	if useColor(&bytes.Buffer{}) {
		t.Error("buffer: want no color")
	}

	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if useColor(f) {
		t.Error("regular file: want no color")
	}

	t.Setenv("NO_COLOR", "1")
	if useColor(os.Stderr) {
		t.Error("NO_COLOR set: want no color")
	}
}

func TestNewLoggerInvalid(t *testing.T) {
	if _, err := NewLogger(&bytes.Buffer{}, "loud", "text"); err == nil {
		t.Error("want error for bad level")
	}
	if _, err := NewLogger(&bytes.Buffer{}, "info", "xml"); err == nil {
		t.Error("want error for bad format")
	}
}
