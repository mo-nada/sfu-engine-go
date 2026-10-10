// Package webhook sends room events to tenants' webhook URLs.
//
// Each event is a JSON POST signed with the tenant's webhook secret:
//
//	X-SFU-Event: participant_joined
//	X-SFU-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256(secret, "<t>.<body>")>
//
// The receiver recomputes the HMAC over "<t>.<raw body>" and rejects old
// timestamps, so replays are limited. Deliveries run in the background with
// retries; they never block media or signaling. If the queue is full, events
// are dropped and logged.
//
// Workers never wait between attempts: a retry is scheduled for later. Each
// tenant has at most perTenant deliveries in flight, so one tenant's slow or
// broken endpoint cannot hold up the others. Redirects are not followed.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/mo-nada/sfu-engine/internal/room"
	"github.com/mo-nada/sfu-engine/internal/store"
)

// Event names.
const (
	RoomStarted       = "room_started"
	RoomFinished      = "room_finished"
	ParticipantJoined = "participant_joined"
	ParticipantLeft   = "participant_left"
	TrackPublished    = "track_published"
	TrackUnpublished  = "track_unpublished"
	SignatureHeader   = "X-SFU-Signature"
	EventHeader       = "X-SFU-Event"
	queueSize         = 10000
	workers           = 8
	perTenant         = 2 // deliveries in flight per tenant
	closeTimeout      = 5 * time.Second
	maxAttempts       = 4
	requestTimeout    = 5 * time.Second
	tenantCacheTTL    = 10 * time.Second
)

// Event is the JSON body of a webhook.
type Event struct {
	ID          string       `json:"id"`
	Event       string       `json:"event"`
	CreatedAt   time.Time    `json:"created_at"`
	Room        RoomInfo     `json:"room"`
	Participant *Participant `json:"participant,omitempty"`
	Track       *Track       `json:"track,omitempty"`
}

// RoomInfo names the room.
type RoomInfo struct {
	Name string `json:"name"`
}

// Participant names the participant.
type Participant struct {
	Identity        string `json:"identity"`
	DurationSeconds int64  `json:"duration_seconds,omitempty"` // participant_left only
}

// Track describes a track.
type Track struct {
	Source string `json:"source"`
}

// Sign returns the X-SFU-Signature value for body at time t.
func Sign(secret string, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "."))
	m.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(m.Sum(nil))
}

// Dispatcher is a room.Observer that delivers webhooks.
type Dispatcher struct {
	store  store.Store
	client *http.Client
	logger *slog.Logger
	now    func() time.Time
	// backoff is the wait before attempt n (n >= 1); tests shorten it.
	backoff func(attempt int) time.Duration

	queue    chan delivery
	stop     chan struct{} // closed by Close
	deadline time.Time     // after Close: give up at this time
	wg       sync.WaitGroup

	mu       sync.Mutex
	cache    map[string]cachedTenant
	inflight map[string]int // tenant -> deliveries in progress
}

type delivery struct {
	tenantID string
	event    Event
	attempt  int // attempts made so far
}

type cachedTenant struct {
	url, secret string
	at          time.Time
}

// NewDispatcher starts the delivery workers. Tenants without a webhook URL
// get no deliveries.
func NewDispatcher(s store.Store, logger *slog.Logger) *Dispatcher {
	d := &Dispatcher{
		store: s,
		client: &http.Client{
			Timeout: requestTimeout,
			// A receiver could redirect the signed POST to an internal address.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		logger:   logger,
		now:      time.Now,
		backoff:  func(n int) time.Duration { return time.Duration(1<<(2*(n-1))) * time.Second }, // 1 s, 4 s, 16 s
		queue:    make(chan delivery, queueSize),
		stop:     make(chan struct{}),
		cache:    make(map[string]cachedTenant),
		inflight: make(map[string]int),
	}
	for range workers {
		d.wg.Go(d.work)
	}
	return d
}

// Close delivers what is queued (one attempt each, no retries) and stops,
// waiting at most closeTimeout. Events still queued then are dropped.
func (d *Dispatcher) Close() {
	d.mu.Lock()
	d.deadline = time.Now().Add(closeTimeout)
	d.mu.Unlock()
	close(d.stop)
	d.wg.Wait()
}

func (d *Dispatcher) closing() bool {
	select {
	case <-d.stop:
		return true
	default:
		return false
	}
}

func (d *Dispatcher) RoomStarted(roomID string)  { d.emit(roomID, RoomStarted, nil, nil) }
func (d *Dispatcher) RoomFinished(roomID string) { d.emit(roomID, RoomFinished, nil, nil) }

func (d *Dispatcher) ParticipantJoined(roomID, participantID string) {
	d.emit(roomID, ParticipantJoined, &Participant{Identity: participantID}, nil)
}

func (d *Dispatcher) ParticipantLeft(roomID, participantID string, joinedAt time.Time) {
	d.emit(roomID, ParticipantLeft, &Participant{Identity: participantID,
		DurationSeconds: int64(d.now().Sub(joinedAt) / time.Second)}, nil)
}

func (d *Dispatcher) TrackPublished(roomID, participantID, source string) {
	d.emit(roomID, TrackPublished, &Participant{Identity: participantID}, &Track{Source: source})
}

func (d *Dispatcher) TrackUnpublished(roomID, participantID, source string) {
	d.emit(roomID, TrackUnpublished, &Participant{Identity: participantID}, &Track{Source: source})
}

func (d *Dispatcher) emit(roomID, name string, p *Participant, t *Track) {
	tenant, roomName := room.SplitRoomID(roomID)
	ev := Event{ID: newID(), Event: name, CreatedAt: d.now().UTC(), Room: RoomInfo{Name: roomName}, Participant: p, Track: t}
	d.enqueue(delivery{tenantID: tenant, event: ev})
}

func (d *Dispatcher) enqueue(job delivery) {
	if d.closing() {
		return
	}
	select {
	case d.queue <- job:
	default:
		d.logger.Warn("webhook queue full, event dropped", "tenant_id", job.tenantID, "event", job.event.Event)
	}
}

// later puts job back in the queue after wait (unless closing).
func (d *Dispatcher) later(job delivery, wait time.Duration) {
	time.AfterFunc(wait, func() { d.enqueue(job) })
}

func (d *Dispatcher) work() {
	for {
		select {
		case job := <-d.queue:
			d.handle(job)
		case <-d.stop:
			// Deliver what is left, once each, until the deadline.
			for {
				d.mu.Lock()
				late := time.Now().After(d.deadline)
				d.mu.Unlock()
				if late {
					return
				}
				select {
				case job := <-d.queue:
					d.handle(job)
				default:
					return
				}
			}
		}
	}
}

func (d *Dispatcher) handle(job delivery) {
	d.mu.Lock()
	if d.inflight[job.tenantID] >= perTenant && !d.closing() {
		d.mu.Unlock()
		d.later(job, 200*time.Millisecond) // this tenant is busy: try again soon
		return
	}
	d.inflight[job.tenantID]++
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		if d.inflight[job.tenantID]--; d.inflight[job.tenantID] <= 0 {
			delete(d.inflight, job.tenantID)
		}
		d.mu.Unlock()
	}()
	d.deliver(job)
}

func (d *Dispatcher) deliver(job delivery) {
	url, secret, ok := d.tenant(job.tenantID)
	if !ok || url == "" {
		return
	}
	body, err := json.Marshal(job.event)
	if err != nil {
		return
	}
	job.attempt++
	status, err := d.post(url, secret, job.event.Event, body)
	switch {
	case err == nil && status >= 200 && status < 300:
		return
	case err == nil && status >= 400 && status < 500 && status != 408 && status != 429:
		// The receiver rejects it; retrying will not help.
		d.logger.Warn("webhook rejected", "tenant_id", job.tenantID, "event", job.event.Event, "status", status)
	case job.attempt >= maxAttempts || d.closing():
		d.logger.Warn("webhook failed", "tenant_id", job.tenantID, "event", job.event.Event, "status", status, "err", err)
	default:
		d.later(job, d.backoff(job.attempt))
	}
}

func (d *Dispatcher) post(url, secret, name string, body []byte) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "sfu-engine-webhook/1")
	req.Header.Set(EventHeader, name)
	req.Header.Set(SignatureHeader, Sign(secret, d.now(), body))
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// tenant returns the tenant's webhook URL and secret, cached for a few seconds.
func (d *Dispatcher) tenant(id string) (string, string, bool) {
	d.mu.Lock()
	c, ok := d.cache[id]
	d.mu.Unlock()
	if ok && time.Since(c.at) < tenantCacheTTL {
		return c.url, c.secret, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t, err := d.store.GetTenant(ctx, id)
	if err != nil {
		return "", "", false
	}
	d.mu.Lock()
	d.cache[id] = cachedTenant{url: t.WebhookURL, secret: t.WebhookSecret, at: time.Now()}
	d.mu.Unlock()
	return t.WebhookURL, t.WebhookSecret, true
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return fmt.Sprintf("evt_%x", b)
}
