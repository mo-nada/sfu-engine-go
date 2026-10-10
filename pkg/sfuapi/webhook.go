package sfuapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// WebhookEvent is the body of a webhook.
type WebhookEvent struct {
	ID          string                `json:"id"`
	Event       string                `json:"event"` // room_started, room_finished, participant_joined, participant_left, track_published, track_unpublished
	CreatedAt   time.Time             `json:"created_at"`
	Room        struct{ Name string } `json:"room"`
	Participant *struct {
		Identity        string `json:"identity"`
		DurationSeconds int64  `json:"duration_seconds"`
	} `json:"participant,omitempty"`
	Track *struct {
		Source string `json:"source"`
	} `json:"track,omitempty"`
}

// ErrBadWebhook is returned for a missing, malformed, old or forged signature.
var ErrBadWebhook = errors.New("sfu: invalid webhook signature")

// VerifyWebhook checks the X-SFU-Signature of body and parses it. tolerance
// rejects signatures older or newer than that (use 5 minutes).
func VerifyWebhook(secret string, body []byte, signature string, tolerance time.Duration) (WebhookEvent, error) {
	ts, mac, ok := strings.Cut(strings.TrimPrefix(signature, "t="), ",v1=")
	if !ok || !strings.HasPrefix(signature, "t=") {
		return WebhookEvent{}, ErrBadWebhook
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return WebhookEvent{}, ErrBadWebhook
	}
	if d := time.Since(time.Unix(sec, 0)); d > tolerance || d < -tolerance {
		return WebhookEvent{}, fmt.Errorf("%w: too old", ErrBadWebhook)
	}
	got, err := hex.DecodeString(mac)
	if err != nil {
		return WebhookEvent{}, ErrBadWebhook
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "."))
	m.Write(body)
	if !hmac.Equal(got, m.Sum(nil)) {
		return WebhookEvent{}, ErrBadWebhook
	}
	var ev WebhookEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return WebhookEvent{}, err
	}
	return ev, nil
}

// WebhookHandler returns an http.Handler that verifies webhooks and passes
// each event to fn; it answers 400 for an invalid signature.
func WebhookHandler(secret string, fn func(WebhookEvent)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		ev, err := VerifyWebhook(secret, body, r.Header.Get("X-SFU-Signature"), 5*time.Minute)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fn(ev)
		w.WriteHeader(http.StatusNoContent)
	})
}
