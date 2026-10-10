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
go run ./cmd/sfu
```

Then open <http://localhost:8080> in two browser tabs (or two browsers). In each tab,
enter the same room and a different name, then click **Join**. Each tab sends its
camera, and you see the other tab's video.

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

Set `NO_COLOR=1` to disable colored logs.

## HTTP endpoints

| Path | Purpose |
|---|---|
| `GET /` | Browser demo page for joining a room |
| `GET /ws` | WebSocket signaling |
| `GET /healthz` | Health check, returns `200 ok` |

## Embedding in a Go application

```go
import "github.com/mo-nada/sfu-engine/pkg/sfu"

eng, err := sfu.New(
	sfu.WithAddr(":8080"),
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

Clients connect to `ws://<host>/ws` and exchange JSON messages. Every message has a
`type` field.

Each participant uses **two RTCPeerConnections**:

- **publisher**: the client offers and sends its media to the SFU.
- **subscriber**: the SFU offers and sends the other participants' media to the client.

Two connections mean only one side ever starts negotiation on each one, so
offers can never collide.

| Direction | `type` | Fields | Meaning |
|---|---|---|---|
| client → server | `join` | `room`, `participant` | Must be the first message. IDs are 1–64 letters, digits, `-` or `_` |
| server → client | `joined` | `room`, `participant` | Join accepted. A subscriber `offer` follows |
| client → server | `offer` | `sdp` | Offer on the **publisher** connection |
| server → client | `answer` | `sdp` | Answer to that offer |
| server → client | `offer` | `sdp` | Offer on the **subscriber** connection (initial, and when tracks are added or removed) |
| client → server | `answer` | `sdp` | Answer to that offer |
| both ways | `candidate` | `target`, `candidate` | Trickle ICE. `target` is `publisher` or `subscriber` |
| client → server | `leave` | none | Leave the room and close the connection |
| server → client | `error` | `message` | The request failed |

Example session:

```json
→ {"type":"join","room":"room1","participant":"alice"}
← {"type":"joined","room":"room1","participant":"alice"}
← {"type":"offer","sdp":"v=0..."}
→ {"type":"answer","sdp":"v=0..."}
→ {"type":"offer","sdp":"v=0..."}
← {"type":"answer","sdp":"v=0..."}
→ {"type":"candidate","target":"publisher","candidate":{"candidate":"candidate:...","sdpMid":"0","sdpMLineIndex":0}}
```

Each forwarded track's **stream ID is the publisher's participant name**, so
clients can label the video.

**Errors:**

- These close the connection:
  - an invalid subscriber `answer`, because the subscriber connection could not renegotiate any more;
  - `leave`;
  - a failed or closed PeerConnection;
  - no pong for 60 seconds;
  - a message larger than 64 KB.
- Other mistakes (bad JSON, unknown type, invalid IDs, a duplicate name in the
  room, a message before `join`, a second `join`) get an `error` reply, and the
  connection stays open.

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
internal/web         embedded browser demo page
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

- **No authentication.** Anyone who can reach `/ws` can join any room (Phase 6).
- **Same-origin browsers only.** A web page served from another host cannot
  connect to `/ws`. Non-browser clients are not affected.
- **No TURN server.** Clients behind strict NATs or firewalls may not connect.
- **Video only** in the demo page. Audio is forwarded by the router, but it has
  not been tested yet (Phase 3).
- **No timeout for an unanswered subscriber offer.** A client that never answers
  gets no further offers until it leaves.
- **Slow cleanup after a network drop.** When a client's network drops without
  closing, cleanup waits for Pion's ICE timeouts (about 30 seconds).
- No simulcast, congestion control or bandwidth adaptation yet (Phase 2).
