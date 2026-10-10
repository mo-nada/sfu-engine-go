# How to test everything

Follow the parts in order. Each part says what you should see.

## 0. Once: prepare

```bash
cd ~/workspace/sfu-engine-go
docker start sfu-postgres valkey        # or: docker compose up -d
cd sdks/js && npm install && npm run build && cd ../..
```

Use two terminals. Run these lines in **every** terminal you use:

```bash
cd ~/workspace/sfu-engine-go
export SFU_ADMIN_SECRET=change-me-admin-secret-at-least-32-chars
export SFU_DATABASE_URL=postgres://sfu:sfu@localhost:5432/sfu
export SFU_REDIS_URL=redis://localhost:6379/0
export SFU_URL=http://localhost:8080
```

## 1. Automatic tests (about 1 minute)

```bash
go test -race ./...                                   # Go, in-memory stores
SFU_TEST_DATABASE_URL=$SFU_DATABASE_URL SFU_TEST_REDIS_URL=redis://localhost:6379/15 \
  go test -race ./...                                 # Go, real PostgreSQL + Valkey
(cd sdks/js && npm test)                              # JS SDKs
```

**Expect:** every line says `ok`, and the JS summary says all tests passed.

## 2. Start the server

Terminal 1:

```bash
go run ./cmd/sfu -dev -stun ""
```

**Expect:** the log shows `using PostgreSQL`, `cluster mode`, `admin API enabled`
and `server started`.

## 3. Video in the browser (dev mode)

1. Open http://localhost:8080 in two tabs.
2. Join room `room1` as `alice` in one tab and as `bob` in the other. Leave the
   token field empty.
3. Try each control:
   - Mic off/on: the other tab shows or hides 🔇.
   - Camera off/on: the camera light goes out, and the other tab shows the
     initial.
   - Share screen: a large tile appears in the other tab.
   - Device menus switch the camera or microphone while in the call.
   - Leave: the other tab removes your tile.
4. Look at the quality label on each video, for example `1280×720 · 30 fps · 2.1 Mbps`.

## 4. Act as the operator: create a customer and get webhooks

Terminal 2 — the webhook receiver (keep it running):

```bash
node scripts/try/webhook-receiver.mjs
```

Terminal 3 (do the exports again):

```bash
node scripts/try/setup-customer.mjs http://localhost:4000/hook
```

**Expect:** the customer's `apiKey`, `apiSecret` and `webhookSecret`, saved in
`scripts/try/.customer.json`.

## 5. Act as the customer: tokens and rooms

```bash
node scripts/try/token.mjs standup alice     # prints a join token (creates the room)
node scripts/try/token.mjs standup bob
```

In the two browser tabs:

1. Join room `standup` with names `alice` and `bob`.
2. **Paste each token** into **Join token**.

**Expect:** terminal 2 prints `room_started`, `participant_joined` and
`track_published` (camera, microphone).

Now control the call from terminal 3 and watch the browser:

```bash
node scripts/try/rooms.mjs get standup                         # participants, sources, mute state
node scripts/try/rooms.mjs mute standup bob microphone true     # bob is muted by the server
node scripts/try/rooms.mjs mute standup bob microphone false    # unmuted
node scripts/try/rooms.mjs allow standup bob microphone         # bob may only send his microphone
node scripts/try/rooms.mjs allow standup bob all                # everything allowed again
node scripts/try/rooms.mjs kick standup bob                     # bob is removed
node scripts/try/rooms.mjs close standup                        # everyone is disconnected
```

**Expect:**

- bob's tab shows the server mute.
- His mic button cannot unmute it ("muted by the server").
- His camera disappears for alice while it is not allowed.
- The kick and the close disconnect the tabs, and each webhook appears in
  terminal 2.

## 6. Usage for billing

Stay in a call for at least 2 minutes (usage is written every minute), then run:

```bash
A="Authorization: Bearer $SFU_ADMIN_SECRET"
T=$(node -e "console.log(require('./scripts/try/.customer.json').tenant)")
curl -s -H "$A" $SFU_URL/admin/v1/tenants/$T/usage
```

**Expect:** today with `participant_minutes` above 0.

## 7. Plan limits

```bash
curl -s -X PATCH -H "$A" -d '{"limits":{"max_room_size":1}}' $SFU_URL/admin/v1/tenants/$T
sleep 11      # tenant settings are cached for up to 10 seconds
node scripts/try/token.mjs small alice; node scripts/try/token.mjs small bob
```

Join `small` with both tokens. **Expect:** the second tab gets `room is full`.

## 8. Disable a customer and revoke keys

```bash
curl -s -X PATCH -H "$A" -d '{"disabled":true}' $SFU_URL/admin/v1/tenants/$T
sleep 11
node scripts/try/token.mjs standup alice      # fails: invalid API token (disabled)
curl -s -X PATCH -H "$A" -d '{"disabled":false}' $SFU_URL/admin/v1/tenants/$T
```

## 9. Two nodes (horizontal scaling)

1. Stop the server in terminal 1 (Ctrl+C).
2. Run `scripts/run-cluster.sh`, which starts node-1 on :8081 and node-2 on :8082.
3. Open http://localhost:8081 in one tab and http://localhost:8082 in another.
4. Join the **same** room in both tabs.

**Expect:** both tabs see and hear each other, although they talk to different
nodes. The second join is forwarded to the room's node, and media goes
straight to that node.

Now check where the room lives, and which nodes are alive:

```bash
docker exec valkey valkey-cli -n 1 keys 'sfu:room:*'      # the room and its owner node
docker exec valkey valkey-cli -n 1 smembers sfu:nodes     # node-1, node-2
```

To test a node going down:

1. Stop the cluster with Ctrl+C. The nodes drain for up to 30 s, or stop at
   once when the rooms are empty.
2. Start it again with `scripts/run-cluster.sh`.

## 10. API docs

- http://localhost:8080/docs: server API, admin API and webhooks.
- http://localhost:8080/docs/signaling: the WebSocket messages.

## 11. Next.js example (optional)

This needs `next` to be installed. See `sdks/js/examples/nextjs/README.md`.

## Troubleshooting

- **Connection refused to PostgreSQL or Valkey:** run
  `docker start sfu-postgres valkey`.
- **Video connects but stays black on WSL:** UDP between Windows and WSL2 may
  be blocked. Add these two lines to `C:\Users\<you>\.wslconfig`:

  ```
  [wsl2]
  networkingMode=mirrored
  ```

  Then run `wsl --shutdown` in PowerShell and reopen WSL.
- **`invalid token` on join:** the token belongs to another customer, or it
  expired (tokens last 1 hour). Make a new one.
- **`room not found`:** customers made by the admin API need the room created
  first. `token.mjs` does that for you.
