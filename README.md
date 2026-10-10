# sfu-engine-go

An open-source WebRTC **SFU** (Selective Forwarding Unit) written in Go on top of
[Pion WebRTC](https://github.com/pion/webrtc).

An SFU receives each participant's encoded media once and forwards the RTP packets
to the other participants in the same room, without decoding or re-encoding.

**Status: Phase 1 (core media engine).** Two or more participants can join a room
over WebSocket signaling, publish video, and receive everyone else's video through
the SFU. It is a working minimal SFU, not a production-ready platform yet. See
[Known limitations](#known-limitations).

## Quick start

Requirements: Go 1.27 or newer.

```bash
go run ./cmd/sfu -dev
```

`-dev` adds a development tenant and the `/dev/token` endpoint, which the demo
page uses to get join tokens. Never use `-dev` in production. For real use, pass
a tenants file instead (see [Tenants and join tokens](#tenants-and-join-tokens)).

Then open <http://localhost:8080> in two browser tabs (or two browsers). In each tab,
enter the same room and a different name, then click **Join**. Each tab sends its
camera and microphone, and you see and hear the other tab. The control bar turns
the microphone and camera on and off, shares your screen, and switches devices.

The browser only allows camera access on `localhost` or over HTTPS. To test from
another machine, put the server behind HTTPS (for example, a reverse proxy).

## Command-line flags

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `:8080` | HTTP listen address |
| `-log-level` | `info` | `debug`, `info`, `warn` or `error` |
| `-log-format` | `text` | `text` (colored in a terminal) or `json` |
| `-stun` | `stun:stun.l.google.com:19302` | Comma-separated STUN URLs. Pass `-stun ""` to disable |
| `-shutdown-timeout` | `10s` | Maximum time for graceful shutdown |
| `-tenants` | none | JSON file with tenants (see `tenants.example.json`) |
| `-dev` | off | Development tenant and open `/dev/token` endpoint. Never in production |

The server needs `-tenants` or `-dev` to start.

Set `NO_COLOR=1` to disable colored logs.

## HTTP endpoints

| Path | Purpose |
|---|---|
| `GET /` | Browser demo page for joining a room |
| `GET /ws` | WebSocket signaling |
| `GET /healthz` | Health check, returns `200 ok` |
| `GET /dev/token?room=&participant=` | Development join token (only with `-dev`) |
| `GET /docs` | HTTP API documentation (Swagger UI) |
| `GET /docs/signaling` | Signaling protocol documentation (AsyncAPI viewer) |
| `GET /api/openapi.yaml` | OpenAPI 3.1 document for the HTTP endpoints |
| `GET /api/asyncapi.yaml` | AsyncAPI 3.0 document for the WebSocket protocol |

The two `/docs` pages load their viewers from the public unpkg CDN, so the
browser needs internet access to show them. The YAML documents are served by the
SFU itself, and other systems can use them to generate clients.

## Tenants and join tokens

The SFU is a shared service. Each **tenant** (a customer system) has an ID, an
API key and a secret of at least 32 bytes:

```json
{"tenants": [{"id": "acme", "api_key": "acme-key", "secret": "…at least 32 random bytes…"}]}
```

The tenant's backend gives each user a short-lived **join token**: a JWT signed
with HS256 using the tenant secret. Its claims are:

| Claim | Meaning |
|---|---|
| `iss` | the tenant API key |
| `sub` | the participant identity |
| `room` | the room name |
| `exp` | expiry time (required) |
| `nbf`, `iat` | optional validity start and issue time |
| `grants` | `{publish, subscribe, sources}`; empty `sources` means all sources |

The browser sends the token in `join`. Rooms are separate per tenant: the same
room name in two tenants is two different rooms.

Go backends can create tokens with `pkg/token`:

```go
c := token.New("acme-key", "alice", "room1", time.Hour)
c.Grants.Sources = []string{"camera", "microphone"} // no screen share
tok, err := token.Sign(secret, c)
```

## Embedding in a Go application

```go
import "github.com/mo-nada/sfu-engine/pkg/sfu"

eng, err := sfu.New(
	sfu.WithAddr(":8080"),
	sfu.WithTenantsFile("tenants.json"), // or sfu.WithTenant(sfu.Tenant{...})
	sfu.WithSTUN("stun:stun.l.google.com:19302"), // sfu.WithSTUN() disables STUN
	sfu.WithLogger(slog.Default()),
	sfu.WithShutdownTimeout(10*time.Second),
)
if err != nil {
	log.Fatal(err)
}
// Blocks until ctx is cancelled, then closes all connections and rooms.
if err := eng.Run(ctx); err != nil {
	log.Fatal(err)
}
```

`eng.Serve(ctx, listener)` does the same with your own `net.Listener`.

## Signaling protocol

The full, machine-readable description is in [`api/asyncapi.yaml`](api/asyncapi.yaml)
(rendered at `/docs/signaling`). In short:

Clients connect to `ws://<host>/ws` and exchange JSON messages. Every message has a
`type` field. Each participant uses **two RTCPeerConnections**:

- **publisher**: the client offers and sends its media to the SFU.
- **subscriber**: the SFU offers and sends the other participants' media to the client.

Two connections mean only one side ever starts negotiation on each one, so offers
can never collide.

| Direction | `type` | Fields | Meaning |
|---|---|---|---|
| client → server | `join` | `token` | Must be the first message. The join token names the room, the participant and the permissions |
| server → client | `joined` | `room`, `participant`, `participants` | Join accepted. `participants` lists the others: `[{id, muted: {source: bool}}]`. A subscriber `offer` follows |
| server → client | `participant-joined` / `participant-left` | `participant` | Someone else joined or left |
| client → server | `offer` | `sdp`, `tracks` | Offer on the **publisher** connection. `tracks` maps each track ID to `camera`, `microphone`, `screen` or `screen-audio` |
| server → client | `answer` | `sdp` | Answer to that offer |
| server → client | `offer` | `sdp` | Offer on the **subscriber** connection (initial, and when tracks are added or removed) |
| client → server | `answer` | `sdp` | Answer to that offer |
| both ways | `candidate` | `target`, `candidate` | Trickle ICE. `target` is `publisher` or `subscriber` |
| client → server | `mute` | `source`, `muted` | Report that you muted or unmuted a source |
| server → client | `mute` | `participant`, `source`, `muted` | Another participant did |
| client → server | `leave` | none | Leave the room and close the connection |
| server → client | `error` | `message` | The request failed |

**Stream IDs.** A forwarded track's stream ID is `<participant>/media` for camera
and microphone (one stream, so players keep them in sync), or
`<participant>/screen` for screen and screen audio.

**Stopping a track** (for example, a screen share): remove it from the publisher
connection and send a new `offer`. The SFU then removes it from everyone.

**Errors.** These close the connection:

- an invalid subscriber `answer`;
- `leave`;
- a failed PeerConnection;
- no pong for 60 seconds;
- a message over 64 KB;
- a client too slow to read its messages.

Other mistakes get an `error` reply, and the connection stays open.

## Architecture

```
cmd/sfu              executable: flags → config → logger → app
pkg/sfu              public Go API (Engine, options)
internal/app         wiring, HTTP server, ordered shutdown
internal/config      flags and validation
internal/observability  slog logger (tint in terminals, JSON otherwise)
internal/signaling   WebSocket protocol, one session per client
internal/room        Manager → Room → Participant lifecycle
internal/peer        one PeerConnection: negotiation, ICE queueing, close
internal/sfu         Router: forwards published tracks to subscribers
internal/transport   Pion API (codecs, interceptors) and ICE config
internal/web         embedded browser pages: demo client and API doc viewers
api                  OpenAPI and AsyncAPI documents (embedded and served)
tests/integration    real clients against the real server
```

**Ownership and cleanup:**

- The **Manager** owns rooms. A **Room** owns its participants and one **Router**.
  A **Participant** owns its two peers.
- When a participant leaves (WebSocket closed, `leave`, or a peer fails), their
  peers are closed and they are removed from the router.
- When a publisher's peer closes, the router's read loop ends. Its tracks are then
  removed from every subscriber, and each subscriber gets a new offer.
- When the last participant leaves, the room and its router are removed.
- On shutdown, the server stops accepting HTTP connections, then closes all
  WebSocket sessions, then closes all rooms.

**Forwarding:** each published track has one shared `TrackLocalStaticRTP` that is
added to every other participant's subscriber connection. Each RTP packet is
written once, and Pion sends it to every subscriber. When someone subscribes, the
router asks the publisher for a keyframe (PLI), so new viewers see video quickly.
Keyframe requests from subscribers are passed on to the publisher.

## Testing

```bash
go test -race ./...                       # all unit and integration tests
go test -race -v ./tests/integration/     # end-to-end: real clients, real server
go vet ./...
```

The integration tests start the real server on a random local port without STUN.
Pion clients connect over WebSocket, publish VP8 video, and check that the other
participants receive the RTP packets. They also cover late joiners, room
isolation, disconnect cleanup, and duplicate names.

## Known limitations

These are planned for later phases:

- **No control API or webhooks yet.** These come in the next Platform Track stages.
- **Single node.** Horizontal scaling comes in the next Platform Track stages.
- **Same-origin browsers only.** A web page served from another host cannot
  connect to `/ws`. Non-browser clients are not affected.
- **No TURN server.** Clients behind strict NATs or firewalls may not connect.
- **No timeout for an unanswered subscriber offer.** A client that never answers
  gets no further offers until it leaves.
- **Slow cleanup after a network drop.** When a client's network drops without
  closing, cleanup waits for Pion's ICE timeouts (about 30 seconds).
- No simulcast, congestion control or bandwidth adaptation yet (Phase 2).
