# Go-Based Open Source WebRTC SFU Engine

## 1. Project Overview

Build an open-source, production-oriented real-time media engine written in Go, inspired by projects such as LiveKit.

The goal is to create a modular, lightweight, and extensible WebRTC SFU (Selective Forwarding Unit) engine that can eventually support real-time audio/video communication, recording, and distributed media infrastructure.

The project should be developed incrementally in well-defined phases, starting with the core engine and basic media forwarding before introducing advanced features.

This is not intended to be a simple WebRTC demo. The long-term goal is to build a reusable media infrastructure component with clean APIs, reliable media transport, strong test coverage, and a maintainable architecture.

## 2. Core Architecture

Use a Selective Forwarding Unit (SFU) architecture.

The media server should receive encoded audio/video RTP packets from publishers and forward them to the appropriate subscribers without decoding or re-encoding media in the normal forwarding path.

The system should separate the following responsibilities:

- **Media Engine:** Core WebRTC session and media lifecycle management.
- **Signaling:** Communication between clients and the server for session negotiation and control messages.
- **Room Management:** Creation, lifecycle, and management of rooms and participants.
- **Peer Management:** WebRTC PeerConnection lifecycle and media track management.
- **SFU Router:** Routing published tracks to subscribed participants.
- **Transport and Connectivity:** ICE configuration, STUN/TURN connectivity, and related transport settings.
- **Observability:** Structured logging, metrics, and diagnostics.
- **Public API:** A clean Go API that allows other Go applications to embed and use the engine.

Keep these responsibilities modular without introducing unnecessary abstractions or premature complexity.

## 3. Technology Stack and Dependencies

### Primary Language

- Go

### Core WebRTC Library

- `github.com/pion/webrtc/v4`

Use Pion WebRTC as the foundation for WebRTC functionality, including PeerConnection management, SDP negotiation, ICE, DTLS, SRTP, media tracks, and data channels.

Do not reimplement existing WebRTC protocol implementations unless there is a specific technical reason.

### Signaling

- `github.com/gorilla/websocket`

Use WebSocket for the initial signaling transport between clients and the server.

The signaling layer should support exchanging SDP offers and answers, ICE candidates, and room-related control messages.

Keep the signaling protocol independent of the internal media engine so alternative signaling transports can be introduced later.

### RTP and RTCP

Use Pion's RTP and RTCP packages when direct packet-level handling is required:

- `github.com/pion/rtp`
- `github.com/pion/rtcp`

Use these packages for RTP packet processing, forwarding-related functionality, RTCP feedback, and media transport diagnostics where appropriate.

Avoid adding redundant direct dependencies when the required functionality is already accessible through Pion WebRTC.

### Logging

Prefer Go's standard `log/slog` initially. Use `go.uber.org/zap` only if additional logging functionality or performance characteristics justify it.

### Testing

Use Go's standard `testing` package. Add `github.com/stretchr/testify` if it improves test readability and maintainability.

### Future Dependencies

Depending on future requirements, consider:

- `github.com/pion/turn/v4` for operating a TURN server when needed.
- `github.com/prometheus/client_golang` for Prometheus metrics.
- FFmpeg integration or another suitable media pipeline for future recording and media processing.

Do not introduce future-phase dependencies before they are required.

## 4. Important Protocols

The implementation should use and understand the following protocols:

- **WebRTC:** Overall real-time communication framework.
- **SDP:** Session negotiation and media description.
- **ICE:** Connectivity establishment and candidate selection.
- **STUN:** Public-facing address discovery and connectivity assistance.
- **TURN:** Relayed connectivity when direct connectivity is not possible.
- **DTLS:** Secure transport and key establishment for WebRTC.
- **SRTP/SRTCP:** Encryption and protection of media packets and associated control traffic.
- **RTP:** Audio/video media packet transport.
- **RTCP:** Media feedback, statistics, and control.
- **WebSocket:** Initial application-level signaling and control transport.

Pion should handle protocol implementations whenever possible. The engine should focus on coordinating these components and implementing SFU-specific behavior.

## 5. Initial Project Structure

Use the following directory structure as the initial architectural baseline:

```text
sfu-engine/
├── cmd/
│   └── sfu/
│       └── main.go
│
├── internal/
│   ├── app/
│   │   └── app.go
│   │
│   ├── config/
│   │   └── config.go
│   │
│   ├── signaling/
│   │   └── websocket.go
│   │
│   ├── room/
│   │   ├── manager.go
│   │   ├── room.go
│   │   └── participant.go
│   │
│   ├── peer/
│   │   ├── peer.go
│   │   └── track.go
│   │
│   ├── sfu/
│   │   └── router.go
│   │
│   ├── transport/
│   │   └── ice.go
│   │
│   └── observability/
│       └── logger.go
│
├── pkg/
│   └── sfu/
│       ├── engine.go
│       └── options.go
│
├── tests/
│   └── integration/
│       └── two_peers_test.go
│
├── go.mod
├── go.sum
├── README.md
└── .gitignore
```

This structure is a starting point, not a requirement to create every file immediately. Create components when their responsibilities become necessary.

### Directory Responsibilities

- `cmd/sfu/main.go`: Executable entry point.
- `internal/app/`: Application initialization, dependency wiring, startup, and shutdown.
- `internal/config/`: Configuration loading and validation.
- `internal/signaling/`: WebSocket signaling and message handling.
- `internal/room/`: Room and participant lifecycle management.
- `internal/peer/`: WebRTC PeerConnection and media track lifecycle.
- `internal/sfu/`: Media routing and publisher/subscriber forwarding logic.
- `internal/transport/`: Transport configuration and connectivity-related helpers.
- `internal/observability/`: Logging and diagnostics.
- `pkg/sfu/`: Public Go API for embedding the engine in other applications.
- `tests/integration/`: Integration tests for real WebRTC sessions and media forwarding.

Keep implementation details under `internal/`. Expose only intentionally designed public interfaces through `pkg/sfu/`.

Avoid circular dependencies. Define interfaces only where they clarify ownership, decouple real responsibilities, or enable meaningful testing.

## 6. Development Roadmap

Development must follow these phases in order, with one change decided by the user on 2026-10-10: Phase 1 is complete, and the **Platform Track** below comes next, before Phases 2–4. It brings forward the SDK, multi-tenancy, authentication and control API parts of Phase 6, and the horizontal scaling of Phase 5.

### Phase 1: Core Media Engine

**Complete.** In addition, microphone, camera and screen share controls, participant roster and mute state (from Phase 3), and camera and screen quality settings are done.

Implement the minimum viable WebRTC SFU foundation.

Requirements:

1. Initialize the Go module and establish the project structure.
2. Create an engine initialization and lifecycle API.
3. Configure Pion WebRTC and its ICE settings.
4. Implement WebSocket signaling.
5. Support SDP offer/answer exchange and ICE candidate exchange.
6. Implement room creation and participant management.
7. Manage PeerConnection lifecycle, connection states, and cleanup.
8. Support publishing a media track and subscribing to a remote participant's track.
9. Implement basic RTP forwarding from a publisher to a subscriber.
10. Handle disconnects, errors, and resource cleanup correctly.
11. Add structured logs for connections, rooms, participants, and media lifecycle events.
12. Write unit tests and integration tests.

**Phase 1 acceptance criteria:**

- The server starts and shuts down cleanly.
- Clients can connect through the signaling layer.
- WebRTC negotiation succeeds.
- ICE connectivity is established.
- A participant can publish a video track.
- Another participant can receive that video through the SFU.
- Multiple participants can join the same room.
- Published tracks are routed to the appropriate subscribers.
- Participants and tracks are cleaned up correctly when clients disconnect.
- Tests cover successful negotiation, media forwarding, and important failure cases.

The primary milestone is a working, minimal SFU—not a complete production-grade media platform.

### Platform Track: Video Platform as a Service (SDKs, Control API, Scaling)

**Current development phase.**

**Product goal (decided by the user):** run the SFU as a paid video service, like LiveKit. The operator sells subscriptions and gives each customer (tenant) an API key and secret. The customer's backend uses a **server SDK** to create rooms, issue join tokens and control rooms. The customer's frontend (React, Next.js and others) uses a **client SDK** and **React components** to join rooms and publish and receive camera, microphone and screen, without touching WebRTC. The REST API and signaling protocol are what the SDKs call; the SDKs are the product surface.

Requirements:

1. Tenants with API keys and secrets; full isolation between tenants.
2. Join tokens (JWT HS256) carrying room, identity and grants; API tokens (JWT HS256, `aud: "sfu-api"`) for server-to-server calls. Secrets are never sent over the wire.
3. Server API `/v1`: rooms (create with options, list, get, close), participants (list, remove, server-side mute, update permissions), join token creation.
4. Client SDK (TypeScript): `Room`, local and remote participants, tracks, events, camera, microphone, screen share, devices, reconnect, quality settings.
5. Server SDKs: TypeScript (Node, Next.js) first, then Go: token builder, room service client, webhook receiver.
6. React SDK: context, hooks and components, including a prebuilt conference view; a Next.js example app.
7. Operator admin API: tenants, API key create, rotate and revoke, plan limits, usage metering (participant-minutes) for billing. Tenants, keys and usage live in the shared store (Valkey/Redis).
8. Signed webhooks to tenants on room and participant events.
9. Horizontal scaling: each room on exactly one node, shared store for registry, load, placement and tenant state, routing of joins and API calls to the room's node, node draining, and a multi-node test.

Order of work, in stages of about 300 changed lines: (1) tenants and join tokens — done; (2) server API v1 (2a rooms, participants, tokens; 2b server-side mute and permission updates); (3) client SDK; (4) server SDKs; (5) React SDK and Next.js example; (6) admin API, shared store and usage metering; (7) webhooks; (8) horizontal scaling and multi-node test.

SDKs live in this repository under `sdks/js/` (npm packages `client`, `react`, `server`, scope `@sfu-engine` until the user picks a product name). JS tooling (TypeScript, tsup, vitest, React as a peer dependency) needs the user's approval before it is downloaded.

Rooms that span several nodes (media relayed between nodes) and payment-provider integration are out of scope for now.

### Phase 2: Streaming Stability and Performance

Improve media reliability and performance.

Planned features:

- Correct RTP forwarding and header handling.
- RTCP feedback handling.
- NACK and PLI support where appropriate.
- Congestion control and bandwidth adaptation.
- Simulcast support.
- Packet-loss and jitter testing.
- Connection recovery and media interruption handling.
- Load testing and profiling.

Measure latency, packet loss, video freezes, bandwidth, CPU usage, and memory usage.

### Phase 3: Production Audio/Video Features

Expand the engine to support practical real-time communication scenarios.

Planned features:

- Audio tracks and Opus.
- Multiple audio/video tracks per participant.
- Track subscription management.
- Mute/unmute and track lifecycle controls.
- Data channels.
- Additional codec compatibility.
- Comprehensive integration and load tests.

### Phase 4: Recording

Introduce a dedicated recording pipeline.

Planned features:

- Individual-track recording.
- Optional composite recording.
- Suitable media container and codec handling.
- Asynchronous recording jobs where appropriate.
- Storage adapters, including S3-compatible storage.
- Recording status, error handling, and recovery.

Keep recording concerns separate from the core packet-forwarding path whenever possible.

### Phase 5: Distributed Media Infrastructure

Scale beyond a single media server.

Planned features:

- Multiple media nodes.
- Room placement and node selection.
- Service discovery and node health.
- Capacity-aware routing.
- Distributed room lifecycle coordination.
- Cross-service communication and shared state where necessary.

Introduce Redis or a messaging system only when concrete distributed coordination requirements justify them.

### Phase 6: Developer Platform

Make the engine convenient for application developers and operators.

Planned features:

- Stable public Go APIs.
- Client SDKs and example applications.
- Authentication and authorization.
- Webhooks and application events.
- Metrics, tracing, and operational dashboards.
- Docker-based deployment and production configuration.
- Comprehensive documentation and deployment guides.

## 7. Engineering Principles

Follow these principles throughout development:

1. **Incremental development:** Work on the current phase only unless explicitly instructed otherwise.
2. **Working software first:** Every phase must produce a testable, demonstrable result.
3. **Correctness before optimization:** Establish correct media forwarding before advanced performance optimization.
4. **Use existing protocol implementations:** Prefer Pion over reimplementing WebRTC protocols.
5. **Minimal dependencies:** Add dependencies only when they provide clear value.
6. **Clear ownership:** Define which component owns rooms, participants, PeerConnections, tracks, and cleanup.
7. **Concurrency safety:** Protect shared mutable state and avoid data races.
8. **Lifecycle management:** Implement deterministic cleanup for connections, goroutines, rooms, and tracks.
9. **Testability:** Design components so their behavior can be tested independently where practical.
10. **Observability:** Include useful structured logs and diagnostics from the beginning.
11. **Horizontal scaling:** Every feature must work when the service runs on several nodes. A room and its media live on exactly one node; any other state that more than one node needs (tenants, room placement, node health) goes in the shared store, never only in one node's memory. A request may reach any node, which must route it to the room's node.
12. **No premature abstractions:** Prefer simple, explicit designs over speculative frameworks.
13. **Documentation:** Document public APIs, signaling messages, configuration, and important architectural decisions.
14. **Compatibility:** Verify dependency APIs and versions instead of assuming that examples from other releases apply.
15. **Multi-tenant isolation:** Every API call, token, room, participant, webhook and log line is scoped to one tenant. One tenant can never see or affect another tenant's data.

## 8. Performance and Reliability

Streaming stability is a core objective, but it must be validated through testing rather than assumed.

As the implementation matures, measure:

- Time to establish a connection.
- Time to first video frame.
- End-to-end latency.
- Packet loss and jitter.
- Video frame drops and freezes.
- CPU and memory consumption.
- Bandwidth per participant.
- Recovery behavior after network disruptions.

Use reproducible tests and document the test environment and network conditions.

Do not claim production readiness or specific performance guarantees without supporting test results.

## 9. Scope Restrictions for the Current Phase

During the Platform Track:

- Do not implement recording.
- Do not relay media between nodes (a room stays on one node).
- Do not build Kubernetes orchestration (Docker Compose for tests is fine).
- Do not implement a complex transcoding pipeline.
- Do not add billing.
- Do not implement advanced congestion-control or simulcast systems prematurely.
- Do not build a custom implementation of ICE, DTLS, SRTP, or other existing protocol stacks.
- Do not add dependencies without the user's approval (Redis client only in stage 5).

Focus on a correct, secure, testable multi-tenant control API that scales out by rooms.

## 10. Instructions for AI Coding Agents

Before making changes:

1. Read `platform-context.md` first. It summarizes the architecture, ownership, protocol, key decisions, and stage history. Open source files only for the parts you will change or when the summary is not enough.
2. Read `go.mod` and the packages, tests, and documentation relevant to the change.
3. Follow the current architecture unless there is a clear, documented reason to change it.
4. Identify the smallest implementation step that advances the current phase.

When implementing a feature:

1. Explain the intended change briefly.
2. Implement it using idiomatic Go.
3. Add or update relevant tests.
4. Run formatting, tests, and static checks where available.
5. Fix regressions introduced by the change.
6. Update documentation when behavior or public APIs change.
7. In the same change, keep the API documents current: `api/openapi.yaml` for HTTP endpoints and `api/asyncapi.yaml` for WebSocket signaling messages. Other systems integrate from these files, so every endpoint, message, field, and error rule must match the code.
8. In the same change, update `platform-context.md` when architecture, ownership, protocol, public API, endpoints, or key decisions change, and add the stage to its history.

Do not create placeholder files for every future feature. Do not claim that tests passed unless they were actually executed successfully.

If an architectural decision has significant long-term consequences, explain the trade-offs and prefer the simplest design that satisfies the current requirements.

## 11. Current Objective

The immediate objective is the **Platform Track** (see section 6): turn the SFU into a LiveKit-like video platform service. Work in the listed order: server API v1, then the client SDK, server SDKs, React SDK with a Next.js example, the operator admin API with shared store and usage metering, webhooks, and horizontal scaling.

Every endpoint and message must be usable from the SDKs, documented in `api/openapi.yaml` and `api/asyncapi.yaml`, tenant-isolated, and correct on several nodes.
