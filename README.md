# sfu-engine-go

An open-source WebRTC **SFU** (Selective Forwarding Unit) written in Go on top of
[Pion WebRTC](https://github.com/pion/webrtc).

An SFU receives each participant's encoded media once and forwards the RTP packets
to the other participants in the same room, without decoding or re-encoding.

**Status:** a multi-tenant video platform service, like LiveKit:

- **Customers (tenants)** get API keys and use the **server API** to create
  rooms, issue join tokens and control participants.
- **Their apps** join rooms over WebSocket signaling and WebRTC: camera,
  microphone and screen share.
- **You, the operator,** manage customers, keys, plan limits and usage
  through the **admin API**.
- It **scales horizontally**: each room lives on one node, and any node can be
  called.

It has been tested with real browsers on one machine, and with Go test
clients on one or two nodes. It has not been load-tested yet. See
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

## Platform mode: database, cluster store and admin API

```bash
docker compose up -d                 # PostgreSQL :5432 and Valkey :6379
export SFU_DATABASE_URL=postgres://sfu:sfu@localhost:5432/sfu
export SFU_REDIS_URL=redis://localhost:6379/0
export SFU_ADMIN_SECRET=change-me-admin-secret-at-least-32-chars
go run ./cmd/sfu -dev -stun ""
```

- **PostgreSQL** keeps tenants, API keys and usage, so all nodes share them and
  they survive restarts.
- **Valkey** (or Redis) keeps the cluster state.
- The admin secret turns on the admin API.

**Two nodes on one machine** (ports 8081 and 8082): run `scripts/run-cluster.sh`.

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
| `-database-url` | env `SFU_DATABASE_URL` | PostgreSQL for tenants, keys and usage. Empty keeps them in memory |
| `-redis-url` | env `SFU_REDIS_URL` | Redis/Valkey shared by all nodes. Empty means one node |
| `-admin-secret` | env `SFU_ADMIN_SECRET` | Enables the admin API (at least 32 bytes). Prefer the env variable |
| `-node-id` | env `SFU_NODE_ID`, or host name + random | This node's name in the cluster |
| `-node-url` | env `SFU_NODE_URL`, or `http://127.0.0.1:<port>` | URL the other nodes use to reach this node |
| `-drain-timeout` | `0` | On shutdown, take no new rooms and wait this long for rooms to empty |

The server needs `-tenants`, `-database-url`, `-admin-secret` or `-dev` to start.

Set `NO_COLOR=1` to disable colored logs.

## HTTP endpoints

| Path | Purpose |
|---|---|
| `GET /` | Browser demo page for joining a room |
| `GET /ws` | WebSocket signaling |
| `GET /healthz` | Health check, returns `200 ok` |
| `GET /dev/token?room=&participant=` | Development join token (only with `-dev`) |
| `/v1/...` | Server API for tenants' backends |
| `/admin/v1/...` | Admin API for the operator (only with an admin secret) |
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
{"tenants": [{"id": "acme", "api_key": "acme-key", "secret": "…at least 32 random bytes…", "auto_create_rooms": false}]}
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

## Server API (`/v1`)

Tenants' backends manage rooms with a REST API. This is what the server SDKs
call. Each request carries `Authorization: Bearer <API token>`: a short-lived
HS256 JWT signed with the tenant secret, with claims `iss` (API key),
`aud: "sfu-api"`, `iat` and `exp` (at most 1 hour). The secret itself is never sent.

| Method | Path | Does |
|---|---|---|
| `POST` | `/v1/rooms` | Create a room `{name, max_participants, empty_timeout_seconds}` |
| `GET` | `/v1/rooms` | List rooms |
| `GET` | `/v1/rooms/{room}` | Room with its participants |
| `DELETE` | `/v1/rooms/{room}` | Close the room; everyone is disconnected |
| `GET` | `/v1/rooms/{room}/participants` | List participants (sources, mute state) |
| `DELETE` | `/v1/rooms/{room}/participants/{identity}` | Remove a participant |
| `POST` | `/v1/rooms/{room}/participants/{identity}/mute` | Server mute or unmute `{source, muted}`; the client cannot undo it |
| `PATCH` | `/v1/rooms/{room}/participants/{identity}` | Change permissions live `{grants: {publish, subscribe, sources}}` |
| `POST` | `/v1/tokens` | Create a join token `{room, identity, ttl_seconds, grants}` |

Full details are in `api/openapi.yaml`, rendered at `/docs`.

```go
apiTok, _ := token.SignAPI(secret, token.NewAPI("acme-key", 5*time.Minute))
// curl -X POST -H "Authorization: Bearer $apiTok" -d '{"name":"standup"}' http://localhost:8080/v1/rooms
```

A tenant never sees another tenant's rooms: they answer `404`.

By default, a join only works for a room created with `POST /v1/rooms`. That way
room limits always apply, and a closed room stays closed. A tenant with
`"auto_create_rooms": true` lets joins create rooms (the `-dev` tenant does).

## Admin API (`/admin/v1`), for the operator

Every request carries `Authorization: Bearer $SFU_ADMIN_SECRET`.

| Method | Path | Does |
|---|---|---|
| `POST` | `/admin/v1/tenants` | Create a customer `{id, name, auto_create_rooms, webhook_url, limits}`. Returns its first `api_key` and `api_secret`, and its `webhook_secret`; these are shown only once |
| `GET` | `/admin/v1/tenants` | List customers |
| `GET` / `PATCH` | `/admin/v1/tenants/{id}` | Read or change a customer: name, limits, webhook, `disabled`, `rotate_webhook_secret` |
| `POST` / `GET` | `/admin/v1/tenants/{id}/keys` | Create another key (for rotation), or list keys (without secrets) |
| `DELETE` | `/admin/v1/tenants/{id}/keys/{key}` | Revoke a key |
| `GET` | `/admin/v1/tenants/{id}/usage?from=2026-10-01&to=2026-10-31` | Participant-minutes per day, for billing |

**Limits** (`limits` object; 0 means no limit):

- `max_rooms`: concurrent rooms.
- `max_participants`: concurrent participants in all rooms together.
- `max_room_size`: participants in one room.

Limits are counted across all nodes. Changes to keys and customers reach every
node within 10 seconds.

**Usage** counts every participant's connected time per customer and UTC day. It
is written every minute.

## Webhooks

If a customer has a `webhook_url`, every node sends it signed JSON events:
`room_started`, `room_finished`, `participant_joined`, `participant_left` (with
`duration_seconds`), `track_published` and `track_unpublished`.

The headers are:

- `X-SFU-Event`: the event name.
- `X-SFU-Signature: t=<unix>,v1=<hex HMAC-SHA256(webhook_secret, "<t>.<raw body>")>`.

Check the signature and reject old timestamps. A non-2xx answer is retried up to
3 times. The full format is in the `webhooks` section of `api/openapi.yaml`.

## Running several nodes

```
clients ──► any node ──(forwards the WebSocket and API calls)──► node that owns the room
                         │                                          ▲
                         └─ PostgreSQL (tenants, keys, usage)       │ WebRTC media goes
                            Valkey (nodes, room placement)          │ straight to this node
```

- Each node sends a heartbeat (with its load) to Valkey every 2 seconds. A node
  without a heartbeat for 6 seconds is gone.
- **A room lives on exactly one node.** The first node to claim it in Valkey owns
  it. A claim of a node that is gone can be taken over.
- **New rooms** go to the node with the fewest participants. Draining nodes are
  skipped.
- **A join or API call reaching another node** is forwarded to the owner. Media
  never passes through the forwarding node: ICE candidates come from the owner,
  so clients connect to it directly.
- **Upgrades:** start the node with `-drain-timeout 5m`. On SIGTERM it takes no
  new rooms and waits for its rooms to empty.
- Every node must reach the others at its `-node-url`. Clients must reach each
  node's media ports.

## SDKs

Customers build on the SDKs, not on the raw protocol.

| Package | For | Location |
|---|---|---|
| `@sfu-engine/client` | Browsers: join a room, camera, microphone, screen share, devices, events, auto-reconnect | `sdks/js/packages/client` |
| `@sfu-engine/react` | React and Next.js: `<LiveRoom>`, hooks, `<VideoConference>` and other components | `sdks/js/packages/react` |
| `@sfu-engine/server` | Node, Next.js, edge runtimes: join tokens, room control, webhooks, admin API | `sdks/js/packages/server` |
| `pkg/sfuapi` + `pkg/token` | Go backends: the same server-side features | this module |

**Backend** (for example a Next.js route): create the room and a join token.

```ts
import { AccessToken, RoomServiceClient } from "@sfu-engine/server";

const rooms = new RoomServiceClient(SFU_URL, API_KEY, API_SECRET);
await rooms.createRoom({ name: "standup", maxParticipants: 10 }).catch(() => {}); // exists: fine
const token = await new AccessToken(API_KEY, API_SECRET, { identity: "alice" })
  .addGrant({ room: "standup", publish: true, subscribe: true })
  .toJwt();
```

**Frontend:** a complete call in React.

```tsx
import { LiveRoom, VideoConference } from "@sfu-engine/react";

<LiveRoom serverUrl={SFU_URL} token={token}>
  <VideoConference />
</LiveRoom>
```

**Frontend without React:**

```ts
import { Room } from "@sfu-engine/client";

const room = new Room();
room.on("trackSubscribed", (track, participant) => attachToVideo(track.stream, participant.identity));
await room.connect(SFU_URL, token);
await room.localParticipant.setCameraEnabled(true);
await room.localParticipant.setMicrophoneEnabled(true);
await room.localParticipant.setScreenShareEnabled(true);
```

**Webhooks** (Node): `await new WebhookReceiver(webhookSecret).receive(rawBody, req.headers["x-sfu-signature"])`.
In Go: `sfuapi.WebhookHandler(secret, func(ev sfuapi.WebhookEvent) { ... })`.

**Building and testing the JS SDKs:**

```bash
cd sdks/js
npm install
npm run build
npm test
```

A full Next.js example is in `sdks/js/examples/nextjs`. It is not installed by
`npm install`: see its README.

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
| server → client | `removed` | `message` | The server API removed you or closed the room; the connection closes |
| server → client | `permissions` | `grants` | The server API changed your permissions |
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
internal/auth        join and API token checks against tenants/keys (cached 10 s)
internal/api         server API /v1 (rooms, participants, tokens), cluster routing
internal/admin       operator API /admin/v1 (tenants, keys, limits, usage)
internal/store       PostgreSQL / memory: tenants, keys, usage
internal/cluster     Valkey / memory: node heartbeats, room placement, quotas
internal/usage       participant-time metering for billing
internal/webhook     signed webhook delivery with retries
pkg/token            public: create and verify join and API tokens
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
go test -race ./...                       # all unit and integration tests (in-memory stores)
# also against real PostgreSQL and Valkey (use a separate Redis database):
SFU_TEST_DATABASE_URL=postgres://sfu:sfu@localhost:5432/sfu \
SFU_TEST_REDIS_URL=redis://localhost:6379/15 go test -race ./...
go test -race -v ./tests/integration/     # end-to-end: real clients, real server
go vet ./...
```

The integration tests start the real server on a random local port without STUN.
Pion clients connect over WebSocket, publish VP8 video, and check that the other
participants receive the RTP packets. They also cover late joiners, room
isolation, disconnect cleanup, and duplicate names.

## Known limitations

These are planned for later phases:

- **SDKs are not published to npm yet.** Use them from `sdks/js` (build first).
- **The Next.js example has not been run here:** `next` is not installed.
- **Not load-tested.** A room is limited by one node's capacity.
- **Limits are approximate under bursts.** Counts are a few seconds old, so a
  burst of joins can pass a limit by a few.
- **When a node dies, its rooms are lost.** Participants must reconnect; the room
  is then placed on a live node.
- **Secrets are stored in plain text** in PostgreSQL (API key secrets, webhook
  secrets). Protect the database.
- **Single node.** Horizontal scaling comes in the next Platform Track stages.
- **Same-origin browsers only.** A web page served from another host cannot
  connect to `/ws`. Non-browser clients are not affected.
- **No TURN server.** Clients behind strict NATs or firewalls may not connect.
- **No timeout for an unanswered subscriber offer.** A client that never answers
  gets no further offers until it leaves.
- **Slow cleanup after a network drop.** When a client's network drops without
  closing, cleanup waits for Pion's ICE timeouts (about 30 seconds).
- No simulcast, congestion control or bandwidth adaptation yet (Phase 2).
