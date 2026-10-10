# Platform context

A short, current summary of how the SFU is built and why, so work can start
without re-reading every source file. **Keep it up to date:** update it in the
same change as any change to architecture, ownership, protocol, public API,
endpoints, or key decisions. The protocol and endpoints are documented in
detail in `api/asyncapi.yaml` and `api/openapi.yaml`.

Last updated: 2026-10-10. Phase 1 is done, plus the Phase 3 controls
(microphone, camera, screen share, roster, mute) and the quality settings. The
current phase is the **Platform Track** (see CLAUDE.md section 6): multi-tenant
control API and horizontal scaling. Stage 1 (tenants and join tokens) is done.

## What it is

A Go WebRTC SFU on Pion WebRTC v4. Publishers send RTP once, and the SFU
forwards it unchanged to the other participants in the same room. There is no
transcoding. It is a multi-tenant service: tenants' backends issue join tokens.
It runs on a single node for now; horizontal scaling is being built (each room
on one node, shared store in Valkey or Redis).

- Module: `github.com/mo-nada/sfu-engine`, Go 1.27.
- Direct dependencies: `pion/webrtc/v4`, `pion/rtcp`, `pion/interceptor`,
  `gorilla/websocket`, `lmittmann/tint`. `testify` and `uuid` are listed in
  `go.mod` but not used. **Do not run `go mod tidy`**: it removes them, and the
  user decides on dependencies.
- Run: `go run ./cmd/sfu -dev` → <http://localhost:8080>. Test: `go test -race ./...`.
- Local Valkey (a Redis-compatible store) runs in Docker on `localhost:6379`
  (container `valkey`, image `valkey/valkey:9`). It is meant for the shared-store
  stages.
- Dev environment: WSL2. The browser runs on Windows and reaches the server via
  `localhost` forwarding.

## Packages and ownership

| Package | Owns / does |
|---|---|
| `cmd/sfu` | flags → `config.Load` → `observability.NewLogger` → `app.New` → `Run`; Ctrl+C or SIGTERM cancels ctx |
| `internal/config` | `Config` (Addr, LogLevel, LogFormat, STUNURLs, ShutdownTimeout, TenantsFile, Tenants, Dev), flags, `Validate` (needs tenants or dev) |
| `internal/auth` | `Tenant{ID, APIKey, Secret}`, `NewTenants` (validates: unique id and key, secret ≥ 32 bytes), `LoadTenants(json)`, `DevTenant` (random secret), `VerifyJoinToken` |
| `pkg/token` | Public, stdlib only: JWT HS256 `Claims{iss=api key, sub, room, iat, nbf, exp, grants{publish, subscribe, sources}}`, `New`, `Sign`, `Verify` (rejects other algorithms, 30 s leeway) |
| `internal/observability` | `NewLogger`: tint colored text in a terminal (respects `NO_COLOR`), JSON otherwise |
| `internal/app` | Wires the Pion API, tenants (file + programmatic + dev), `room.Manager`, `signaling.Handler`, routes (`/dev/token` only with `-dev`). `Serve` shutdown order: HTTP Shutdown → `signaling.Close` (parallel) → `rooms.Close` |
| `internal/transport` | `NewAPI()` (default codecs and interceptors; one API is shared, Pion copies the MediaEngine per PC), `Configuration(stunURLs)` |
| `internal/peer` | `Peer` = one PeerConnection. `HandleOffer` (client offers), `Negotiate` / `OnOffer` / `HandleAnswer` (server offers, **one offer in flight**, extra requests merged into one follow-up), early ICE candidate queue (max 64), `OnTrack`, `AddTrack` / `RemoveTrack`, `WriteRTCP`, `CreateDataChannel`, `Close` (idempotent; `OnClose` runs once, **outside** `sync.Once`) |
| `internal/room` | `Manager` → `Room` → `Participant`. Manager lock serializes Join, Leave and SetMuted **and the event notifications**, so all participants see events in one order. A Room owns one `sfu.Router`. A Participant owns two peers (publisher, subscriber), mute state, and a non-blocking `Notifier` |
| `internal/sfu` | `Router` per room. `Publish(publisherID, source, peer, remoteTrack)`: one shared `TrackLocalStaticRTP` per published track, attached to every other subscriber. Read loop → `WriteRTP` (Pion copies to each subscriber). PLI when someone subscribes; subscriber PLI and FIR passed to the publisher. Track removed when `ReadRTP` fails. Sources and `StreamID` in `source.go` |
| `internal/signaling` | WebSocket `Handler`; one `session` per connection with **3 goroutines**: readLoop (handles messages in order), writeLoop (the only writer; `send` queues up to 256 and never blocks, and a full queue disconnects the slow client), pingLoop (30 s ping, 60 s pong timeout) |
| `internal/web` | Embedded pages: `/` demo client, `/docs` (Swagger UI from CDN), `/docs/signaling` (AsyncAPI viewer from CDN) |
| `api` | `openapi.yaml`, `asyncapi.yaml` (embedded, served at `/api/...`) |
| `pkg/sfu` | Public API: `New(WithAddr, WithSTUN, WithLogger, WithShutdownTimeout, WithTenant, WithTenantsFile, WithDevMode)`, `Run`, `Serve`. A thin layer over `internal/app` |
| `tests/integration` | Real Pion clients over WebSocket against `pkg/sfu`. The test client is in `client_test.go` |

## Session lifecycle (signaling)

1. `join {token}`: `tenants.VerifyJoinToken`. The room key is
   `"<tenant>/<room>"`, and the identity and grants come **only** from the
   token. The token is checked once, at join. The server then creates the pub
   and sub peers, plus a data channel on sub so the first offer has an m-line,
   and wires the ICE and offer callbacks.
2. `rooms.Join(...)`: under the manager lock, the joiner's notifier gets the
   roster, which becomes the `joined` message, and the others get
   `participant-joined`.
3. The pub `OnTrack` looks up the source from the latest offer's `tracks` map
   (each offer's map **replaces** the previous one) and checks
   `grants.AllowsSource`. A rejected track is not forwarded and the client gets
   an error. Otherwise it calls `Router.Publish`. An `offer` without
   `grants.Publish` is rejected.
4. `Router.AddSubscriber` (only with `grants.Subscribe`), then `sub.Negotiate()`
   (the first subscriber offer).
5. `session.close()` runs once (atomic CAS) and is triggered by any of:
   - the WebSocket closing,
   - a `leave` message,
   - a bad subscriber answer,
   - a peer failing or closing (`OnClose` → `go s.close()`),
   - a write error,
   - a full send queue,
   - `Handler.Close`.

   It calls `rooms.Leave` (removes the participant from the router, closes its
   peers, notifies `participant-left`, closes the router when the room is
   empty), waits for writeLoop to flush, then closes the socket.
6. Join/close race: `join` checks `closed` **after** all wiring. If close
   already ran, `join` calls Leave and `Router.RemoveSubscriber` itself.

## Key decisions (with reasons)

- **Two PeerConnections per participant**, so offers can never collide (glare).
- **One shared local track per published track**: Pion handles the fan-out and
  rewrites SSRC and payload type per subscriber. Sequence numbers and timestamps
  pass through, which is fine until simulcast.
- **PLI on subscribe**, so late joiners see video quickly.
- **Bad subscriber answer → disconnect.** Pion v4.2.23 cannot roll back a local
  offer (`SetLocalDescription(rollback)` fails), so a rejected answer would
  leave negotiation stuck.
- **Stopping a track = remove it and re-offer.** Pion ends `ReadRTP` with EOF,
  and the router cleans up. There is no `unpublish` message.
- **Stream IDs:** `<name>/media` (camera and microphone, kept in sync) and
  `<name>/screen`.
- **Duplicate participant ID in a room is rejected** (no kick-old behavior).
- **Tenants and auth:**
  - Join tokens are JWT HS256, built with the standard library only (no JWT
    dependency), so a tenant's backend in any language can sign them.
  - The join error is always just "invalid token"; the real reason is only
    logged.
  - Tenants come from a JSON file that every node loads. A later stage moves
    them to the shared store.
  - `-dev` is required for local runs without a tenants file.
- **Horizontal scaling plan** (Platform Track stages 4–6):
  - Each room lives on one node, and rooms never span nodes.
  - The shared store (Valkey or Redis, through go-redis, which needs the
    user's approval to download) holds the node registry, load, room
    placement and tenants.
  - Joins and API calls are routed to the room's node, and nodes drain for
    upgrades.
- **Same-origin WebSocket only** (gorilla default). The page is served by the
  SFU for this reason.
- **API docs are hand-written** (OpenAPI for HTTP, AsyncAPI for WebSocket). The
  viewers load from the unpkg CDN. No swaggo dependency.
- **RTP header extensions are stripped before forwarding** (`stripExtensions` in
  `router.go`). Their IDs are negotiated per connection, so the publisher's IDs
  can mean something else on a subscriber's connection.
- **Quality settings live in the page** (`QUALITY`, `CAMERA_VIDEO`, `SCREEN_VIDEO`):
  - camera: 1280×720 at 30 fps, at most 2.5 Mbps, `balanced`;
  - screen: up to 1920×1080 at 30 fps, at most 4 Mbps, `contentHint "detail"`,
    `maintain-resolution`;
  - screen audio: unprocessed;
  - codec: VP8 (VP9 or AV1 and simulcast are planned for Phase 2).

  Each tile shows resolution, fps and bitrate from `getStats`.
- **Page controls:** mic mute = `track.enabled = false`. Camera off =
  `replaceTrack(null)` and stop the device (no renegotiation); camera on =
  `replaceTrack(newTrack)`. Screen = its own transceiver plus a re-offer.

## Known limitations

- No control API or webhooks yet (Platform Track stages 2–3).
- Single node (Platform Track stages 4–6).
- Token revocation is not possible; keep tokens short-lived.
- No TURN server.
- No timeout for an unanswered subscriber offer.
- About 30 s cleanup delay after a network drop (Pion ICE timeouts).
- No simulcast, congestion control or bandwidth adaptation.
- The `/docs` viewers need internet access (CDN).
- The browser page is checked by hand only, with no automated browser tests.

## Stage history

| Commit | Stage |
|---|---|
| `deefa6a` | Stage 1: config, logger, HTTP server, graceful shutdown |
| `25e570a` | Colored logging (tint) |
| `87e69d7` | Stages 2–3: transport, peer negotiation, rooms |
| `5c22e53` | Stage 4: peer renegotiation, router with RTP forwarding and PLI |
| `f403263` | Stage 5a: WebSocket signaling, room-owned routers |
| `458ad6c` | Stage 5b: wiring in the app, ordered shutdown |
| `52f8bdc` | Stage 6: `pkg/sfu`, integration tests, demo page, README |
| `19ef4b2` | Controls stage A: track sources and stream IDs |
| `a569b42` | Controls stage B (roster, mute, writer goroutine), API docs, stage C (page controls), this file |
| `b4d2775` | Quality: strip RTP header extensions, camera and screen constraints and bitrates, quality overlay |
| (uncommitted) | CLAUDE.md Platform Track rules; Platform stage 1: tenants, join tokens, grants, dev mode |

## Working agreements (from the user)

- Follow the global rules in `~/.claude/CLAUDE.md`: plan, approval, small
  stages, review guide, and no git writes, downloads or new dependencies without
  a clear yes.
- The user may ask to skip the stop-and-wait ("make your recommendation and do
  it all"). Then build everything in order (backend before frontend) and give
  one review at the end.
- Use goroutines for per-user work where it helps (for example, the per-session
  writer).
