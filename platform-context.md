# Platform context

A short, current summary of how the SFU is built and why, so work can start
without re-reading every source file. **Keep it up to date:** update it in the
same change as any change to architecture, ownership, protocol, public API,
endpoints, or key decisions. The protocol and endpoints are documented in
detail in `api/asyncapi.yaml` and `api/openapi.yaml`.

Last updated: 2026-10-10. Phase 1 is done, plus the Phase 3 controls
(microphone, camera, screen share, roster, mute) and the quality settings. The
current phase is the **Platform Track** (see CLAUDE.md section 6).

**Product goal:** a LiveKit-like paid video service. Customers (tenants) get
an API key and secret. They use a server SDK (create rooms, issue tokens,
control rooms) and a client SDK with React components (join, camera,
microphone, screen). The REST API and signaling are what the SDKs call.

Order of work:

1. tenants and tokens — done;
2. server API v1 — done (2a: rooms, participants, tokens; 2b: server mute,
   live permissions, `auto_create_rooms`);
3. client SDK (TypeScript) — done (`sdks/js/packages/client`, 14 tests);
4. server SDKs — done: TypeScript (`sdks/js/packages/server`, 6 tests against
   the real Go server) and Go (`pkg/sfuapi`: `RoomService`, `Admin`,
   `VerifyWebhook`, `WebhookHandler`; tested against a real engine);
5. React SDK — done (`sdks/js/packages/react`, 4 tests, builds). The Next.js
   example source is written (`sdks/js/examples/nextjs`) but has not been run
   (`next` is not installed);
6. admin API, PostgreSQL store and usage metering — done;
7. webhooks — done;
8. horizontal scaling and multi-node tests — done.

SDKs live under `sdks/js/` (npm workspaces, scope `@sfu-engine`, a
placeholder until the user picks a name). Commands, run in `sdks/js`:

- `npm test`: vitest, all packages. The server SDK test builds and starts
  the Go binary itself.
- `npx tsc -p packages/<name>`: type-check one package.
- `npm run build -w @sfu-engine/<name>`: tsup bundles, then `tsc` writes the
  declarations. tsup's `--dts` breaks with TypeScript 7, so it is not used.

Installed dev dependencies (approved by the user): typescript 7, tsup,
vitest, react, react-dom, @types/react, @types/react-dom. `@types/node` is
**not** installed: the server SDK uses Web Crypto and `fetch`, so it also runs
on edge runtimes. Node APIs appear only in tests, which are not type-checked.
npm 11 blocked esbuild's install script, but builds work anyway.

**SDK design:**

- **`@sfu-engine/client`** — `Room` (events, `connect(url, token)`,
  `disconnect`, auto-reconnect that publishes again), `LocalParticipant`
  (`setMicrophoneEnabled`, `setCameraEnabled`, `setScreenShareEnabled`,
  `switchDevice`), `RemoteParticipant` (`tracks` by source, `mediaStream`,
  `screenStream`) and `RemoteTrack`.
  - It uses the same quality defaults as the demo page.
  - Tests inject fake browser APIs through `options.env`.
- **`@sfu-engine/server`** — `AccessToken` (join token), `createApiToken`,
  `RoomServiceClient` (every `/v1` call), `WebhookReceiver` (checks
  `X-SFU-Signature`, 300 s tolerance), `AdminClient` (every `/admin/v1`
  call) and `SFUError`.
- **`@sfu-engine/react`** — `<LiveRoom serverUrl token>` (context; connects,
  then turns the mic and camera on), hooks (`useRoom`, `useConnectionState`,
  `useParticipants`, `useRemoteParticipants`, `useLocalParticipant`,
  `useParticipantMedia`; `useSyncExternalStore` on room events) and
  components (`VideoTrack`, `RoomAudioRenderer`, `ParticipantTile`,
  `ControlBar`, `VideoConference`).

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
- Local Docker containers:
  - Valkey (Redis-compatible) on `localhost:6379`: container `valkey`, image
    `valkey/valkey:9`.
  - PostgreSQL on `localhost:5432`: container `sfu-postgres`, image
    `postgres:17-alpine`, user, password and database all `sfu`.

  `docker-compose.yml` describes both.
- Platform mode: set `SFU_DATABASE_URL`, `SFU_REDIS_URL` and `SFU_ADMIN_SECRET`.
  `scripts/run-cluster.sh` starts 2 nodes on ports 8081 and 8082, using Redis db 1.
- Tests against the real stores: set
  `SFU_TEST_DATABASE_URL=postgres://sfu:sfu@localhost:5432/sfu` and
  `SFU_TEST_REDIS_URL=redis://localhost:6379/15`. Use a separate Redis db,
  otherwise running dev nodes join the test cluster.
- Go dependencies added for the platform: `jackc/pgx/v5`, `redis/go-redis/v9`.
- Dev environment: WSL2. The browser runs on Windows and reaches the server via
  `localhost` forwarding.

## Packages and ownership

| Package | Owns / does |
|---|---|
| `cmd/sfu` | flags → `config.Load` → `observability.NewLogger` → `app.New` → `Run`; Ctrl+C or SIGTERM cancels ctx |
| `internal/config` | `Config` (Addr, LogLevel, LogFormat, STUNURLs, ShutdownTimeout, TenantsFile, Tenants, Dev), flags, `Validate` (needs tenants or dev) |
| `internal/store` | Durable data. Interface `Store`: tenants (with `Limits{MaxRooms, MaxParticipants, MaxRoomSize}`, webhook URL and secret, `Disabled`), API keys (several per tenant, revocable), usage per tenant per UTC day. Implementations: `Memory` (dev, tests) and `Postgres` (schema created on start, tables `tenants`, `api_keys`, `usage_days`). The same conformance test runs against both |
| `internal/auth` | Checks tokens against the store, with a 10 s cache per key; unknown keys go in a separate cache bounded to 10k entries (`Forget` after admin changes). A tenants file and `-dev` **seed** the store. `Tenant{ID, APIKey, Secret, AutoCreateRooms, Limits, WebhookURL, WebhookSecret}` (auto-create default off: joins need an API-created room; dev tenant on). A disabled tenant or revoked key is rejected. Also: `NewTenants` (validates: unique id, key and secret; secret ≥ 32 bytes), `LoadTenants(json)`, `DevTenant` (random secret), `VerifyJoinToken`, `VerifyAPIToken` |
| `pkg/token` | Public, stdlib only, JWT HS256. Join `Claims{iss=api key, sub, room, iat, nbf, exp ≤ 24 h, grants{publish, subscribe, sources}}` with `New`, `Sign`, `Verify`. API `APIClaims{iss, aud="sfu-api", iat, exp ≤ 1 h}` with `NewAPI`, `SignAPI`, `VerifyAPI`. The two kinds are not interchangeable. Strict base64, 30 s leeway |
| `internal/api` | Server API `/v1`: Bearer API token → tenant in context. Routes: rooms (POST, GET list, GET one with participants, DELETE = close), participants (GET, DELETE = remove, PATCH = permissions, POST …/mute = server mute), POST tokens. Talks to rooms through the `Rooms` interface (`LocalRooms` wraps `room.Manager`; it will route to the owner node when scaling). Errors are `{"error": {code, message}}`; another tenant's room is `404` |
| `internal/observability` | `NewLogger`: tint colored text in a terminal (respects `NO_COLOR`), JSON otherwise |
| `internal/app` | Wires everything:
  - stores: Postgres or memory, and Redis or a memory cluster store; the dev key's real secret is read back from the store;
  - room observers: the usage meter, webhooks, and releasing the claim when a room finishes;
  - `joinRouting` for signaling (owner → forward; new auto room → least loaded node → claim);
  - the API platform (cluster plus quota);
  - routes: `/admin/v1` (with an admin secret) and `/dev/token` (with `-dev`).

  Shutdown: optional drain (`SetDraining`, wait for participants to reach 0), then HTTP, signaling, rooms, meter flush, webhooks, leaving the cluster, closing the stores. `Serve` shutdown order: HTTP Shutdown → `signaling.Close` (parallel) → `rooms.Close` |
| `internal/transport` | `NewAPI()` (default codecs and interceptors; one API is shared, Pion copies the MediaEngine per PC), `Configuration(stunURLs)` |
| `internal/peer` | `Peer` = one PeerConnection. `HandleOffer` (client offers), `Negotiate` / `OnOffer` / `HandleAnswer` (server offers, **one offer in flight**, extra requests merged into one follow-up), early ICE candidate queue (max 64), `OnTrack`, `AddTrack` / `RemoveTrack`, `WriteRTCP`, `CreateDataChannel`, `Close` (idempotent; `OnClose` runs once, **outside** `sync.Once`) |
| `internal/room` | `Manager` → `Room` → `Participant`. Explicit rooms (`Create` with `Options{MaxParticipants, EmptyTimeout}`) stay open while empty until the timeout; implicit rooms (created by a join) are removed when empty. `Remove` (kick) and `Delete` (close room) send `EventRemoved` first. `List(prefix)`, `Summary` and `Details` serve the API.
  - `JoinExisting` is for tenants without auto-create.
  - Each participant has its own `Grants` (from the token, then live through `UpdateGrants` → `EventGrants`).
  - `ServerMute` → `EventMuted{ByServer}` goes to everyone, including the participant. A client unmute of a server-muted source gets `ErrServerMuted`.
  - Leaving checks the **exact** `*Participant` under one lock hold (`LeaveParticipant`), so a late leave never removes a newer session with the same ID.
  - Join adds the subscriber to the router **under the manager lock**, so it never races with permission changes.
  - `ServerMute` and `UpdateGrants` change the participant, notify, and update the router all **under the manager lock**, so they never hit a newer session with the same identity, and concurrent PATCHes never lose changes.
  - **Server changes persist per identity** for the room's lifetime (`Room.overrides`: grants that replace the token's, and server mutes). They are applied again on rejoin, so reconnecting cannot undo a mute or a revoked permission. The override is cleared only when the room is gone.
  - On leave, the old session's tracks are `Retire`d (always silent until their read loop ends). Manager lock serializes Join, Leave and SetMuted **and the event notifications**, so all participants see events in one order. A Room owns one `sfu.Router`. A Participant owns two peers (publisher, subscriber), mute state, and a non-blocking `Notifier` |
| `internal/sfu` | `Router` per room. `Publish(publisherID, source, peer, remoteTrack)`: one shared `TrackLocalStaticRTP` per published track, attached to every other subscriber. Read loop → `WriteRTP` (Pion copies to each subscriber). PLI when someone subscribes; subscriber PLI and FIR passed to the publisher. Track removed when `ReadRTP` fails. Sources and `StreamID` in `source.go`.
  - `SetBlocked(pid, source, reason, on)` pauses forwarding while any reason (`server-mute`, `not-allowed`) is on. Packets are still read; on resume, a PLI is sent. It also applies to tracks published later.
  - `Unsubscribe` removes the tracks from a peer that stays open, and renegotiates.
  - `RemoveSubscriber` only removes the exact peer. |
| `internal/signaling` | WebSocket `Handler`; one `session` per connection with **3 goroutines**: readLoop (handles messages in order), writeLoop (the only writer; `send` queues up to 256 and never blocks, and a full queue disconnects the slow client), pingLoop (30 s ping, 60 s pong timeout) |
| `internal/admin` | Operator API `/admin/v1` (Bearer admin secret, constant-time compare): tenants (create returns key, secret and webhook secret once; PATCH limits, webhook, disabled, `rotate_webhook_secret`), keys (create, list without secrets, revoke), usage report |
| `internal/usage` | `Meter` (`room.Observer`): participant-seconds per tenant per UTC day.
  - Written every minute for connected participants, and on leave.
  - Time is split exactly at midnight.
  - A finished day is written in full (rounded); today's sub-second remainder carries to the next flush.
  - Failed writes are retried. |
| `internal/webhook` | `Dispatcher` (`room.Observer`): events room_started/finished, participant_joined/left (with duration), track_published/unpublished. JSON POST signed with `X-SFU-Signature: t=,v1=HMAC-SHA256(secret, "t.body")`.
  - 8 workers, a queue of 10k (dropped when full).
  - At most 2 deliveries in flight per tenant, so a slow tenant cannot block others.
  - Workers never sleep: a retry is re-queued later (up to 4 attempts, backoff 1/4/16 s); 4xx (except 408 and 429) is not retried.
  - Redirects are not followed (SSRF).
  - `Close` makes one attempt each, for at most 5 s.
  - Tenant webhook settings are cached 10 s. |
| `internal/cluster` | `Store` (`MemoryStore`, `RedisStore`): node heartbeats `sfu:node:<id>` with TTL, set `sfu:nodes`, room claims `sfu:room:<tenant/room>` = `"<node>|<generation>"`. The claim TTL is 10× the heartbeat TTL and the owner renews it every heartbeat. Lua claim takes over a dead owner, and the same node re-claims with a new generation. Renew and release are compare-and-set on the exact claim, so a late release of an old copy never removes a newer copy. Every heartbeat checks local rooms: a claim now held by another live node → `Hooks.Lost` closes the local copy; a vanished claim → re-claimed. `Cluster`: heartbeat every 2 s with load (rooms and participants per tenant), TTL 6 s, `Owner`, `ClaimSelf` (refuses while draining), `PickNode` (fewest participants, not draining, self on a tie), `RemoteTenantCounts`, `Release` (async; `Stop` waits up to 5 s for releases). `RoutedHeader = X-SFU-Routed` marks a forwarded request (never forwarded again) |
| `internal/web` | Embedded pages: `/` demo client, `/docs` (Swagger UI from CDN), `/docs/signaling` (AsyncAPI viewer from CDN) |
| `api` | `openapi.yaml`, `asyncapi.yaml` (embedded, served at `/api/...`) |
| `pkg/sfu` | Public API: `New(WithAddr, WithSTUN, WithLogger, WithShutdownTimeout, WithTenant, WithTenantsFile, WithDevMode, WithDatabase, WithCluster, WithAdminSecret, WithDrainTimeout)`, `Run`, `Serve`. A thin layer over `internal/app` |
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

## Multi-node flow

- **Join** on any node: the session verifies the token, then asks
  `RouteJoin`:
  - the owner is this node → handle the join here;
  - the owner is another node → forward the session there. The WebSocket is
    relayed byte for byte; media goes straight to the owner;
  - no owner and the tenant has auto-create → pick the least loaded node:
    forward to it, or claim the room here;
  - no owner and no auto-create → local `JoinExisting` → "room not found".

  A forwarded join that arrives at a non-owner gets the error "room
  unavailable", so the client retries.
- **API:**
  - `owned()` forwards room-specific calls to the owner;
  - `POST /v1/rooms` picks a node (forwarding if needed), and the creating node
    claims the room (409 if someone else owns it);
  - `GET /v1/rooms` merges every node.
- **Limits** are enforced inside `room.Manager` under its lock. This node's
  counts are exact, so no burst on one node can pass a limit; other nodes'
  counts come from heartbeats (`SetRemoteCounts`) and are a few seconds old.
  - `Participant.SetLimits` covers joins.
  - `Options.TenantMaxRooms` covers API-created rooms.
- **Failed joins** always release a claim that `RouteJoin` made.
- **Known rare races** (the next heartbeat's `checkClaims` repairs both):
  - Two concurrent re-claims of the same room on one node can leave the local
    generation out of step with the store.
  - A failed join can release a claim that a concurrent join on the same node
    is about to use.

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
- **Storage split:**
  - PostgreSQL holds the durable data a business needs (tenants, keys,
    billing usage);
  - Valkey holds the fast, temporary cluster state (heartbeats, room
    claims).

  Both are behind interfaces with in-memory versions for development and
  tests.
- **Scaling by rooms:** each room lives on one node, and rooms never span nodes
  (no media relay between nodes). Joins and API calls are forwarded to the
  owner; media goes straight to the owner.
- **Admin API** auth is a single operator secret. Tenant API secrets are stored
  in plain text, because HMAC needs the secret itself.
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

- No SDKs yet (stages 3–5).
- Not load-tested. A room is limited by one node.
- Limits can be passed by a few during bursts (counts are a few seconds old).
- A node crash loses its rooms; clients must reconnect.
- One unexplained test failure was seen once in about 100 repeated signaling
  test runs (2026-10-10). It could not be reproduced and no output was kept. If
  it shows up again, capture the output with
  `go test -count=20 ./internal/signaling/ 2>&1 | tee /tmp/fail.txt`.
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
| `22c04e6` | CLAUDE.md Platform Track rules; Platform stage 1: tenants, join tokens, grants, dev mode |
| `0fa590f` | CLAUDE.md product goal (LiveKit-like, SDKs); Platform stage 2a: API tokens, server API `/v1` (rooms, participants, tokens), `removed` message, room options |
| `188b58c` | Platform stage 2b: server mute, live permissions (`permissions` message), `auto_create_rooms` per tenant |
| `850d239` | Platform stages 6–8 (first version): PostgreSQL store, admin API, usage metering, webhooks, cluster (Valkey), routing, limits |
| (uncommitted) | Review fixes:<br>- claim generations and TTL, lost-room check;<br>- limits enforced inside the room manager;<br>- claim released on failed joins;<br>- webhook rework (no sleeping workers, per-tenant cap, no redirects, bounded close);<br>- bounded unknown-key cache;<br>- billing split at midnight;<br>- pings to the owner node.<br><br>Also: multi-node integration tests, docker-compose, `scripts/run-cluster.sh`, docs, and the SDKs (`sdks/js`) |

## Working agreements (from the user)

- Follow the global rules in `~/.claude/CLAUDE.md`: plan, approval, small
  stages, review guide, and no git writes, downloads or new dependencies without
  a clear yes.
- The user may ask to skip the stop-and-wait ("make your recommendation and do
  it all"). Then build everything in order (backend before frontend) and give
  one review at the end.
- Use goroutines for per-user work where it helps (for example, the per-session
  writer).

## Where to continue (resume here)

State on 2026-10-10:

- **Go:** all packages pass `go test -race ./...`, also with the real stores
  (`SFU_TEST_DATABASE_URL=postgres://sfu:sfu@localhost:5432/sfu`,
  `SFU_TEST_REDIS_URL=redis://localhost:6379/15`).
- **SDKs:** 24 JS tests pass, all 3 packages build, and the Go SDK tests pass.
- Everything since `850d239` is uncommitted (see the stage history).

Open items:

1. Run the Next.js example. This needs the user's approval to `npm install`
   `next` in `sdks/js/examples/nextjs`.
2. Publish the npm packages under a real product name (the user picks the name
   and scope).
3. Load tests, TURN, Docker images for the SFU itself, and Phase 2 media
   quality (simulcast, congestion control).

Watch out for:

- The user often starts a dev server on Redis db 0. Tests must use another
  db (15).
- PostgreSQL runs in Docker as the container `sfu-postgres`. After a reboot:
  `docker start sfu-postgres valkey`.
