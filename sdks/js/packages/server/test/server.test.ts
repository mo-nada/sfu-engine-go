// Runs the SDK against the real Go server: it builds and starts the binary
// (memory store, admin API on), then uses the SDK like a tenant's backend.
import { spawn, execFileSync, type ChildProcess } from "node:child_process";
import { createServer, type Server } from "node:http";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { AccessToken, AdminClient, RoomServiceClient, SFUError, WebhookReceiver, type WebhookEvent } from "../src";

const ROOT = new URL("../../../../../", import.meta.url).pathname;
const ADMIN = "sdk-test-admin-secret-0123456789abcdef";
const PORT = 18300 + Math.floor(Math.random() * 500);
const HOST = `http://127.0.0.1:${PORT}`;

let server: ChildProcess;
let hooks: Server;
const received: WebhookEvent[] = [];
let receiver: WebhookReceiver;
let rooms: RoomServiceClient;
let apiKey = "";
let apiSecret = "";

async function waitFor(cond: () => boolean | Promise<boolean>, ms = 10000) {
  const end = Date.now() + ms;
  while (!(await cond())) {
    if (Date.now() > end) throw new Error("timed out");
    await new Promise((r) => setTimeout(r, 50));
  }
}

beforeAll(async () => {
  execFileSync("go", ["build", "-o", "/tmp/sfu-sdk-test", "./cmd/sfu"], { cwd: ROOT, stdio: "inherit" });
  server = spawn("/tmp/sfu-sdk-test", ["-addr", `127.0.0.1:${PORT}`, "-stun", "", "-log-level", "error"], {
    env: { ...process.env, SFU_ADMIN_SECRET: ADMIN, SFU_DATABASE_URL: "", SFU_REDIS_URL: "" },
    stdio: "inherit",
  });
  await waitFor(async () => (await fetch(`${HOST}/healthz`).catch(() => null))?.ok ?? false);

  // A local webhook receiver.
  hooks = createServer(async (req, res) => {
    let body = "";
    for await (const chunk of req) body += chunk;
    try {
      received.push(await receiver.receive(body, req.headers["x-sfu-signature"] as string));
      res.end("ok");
    } catch {
      res.statusCode = 400;
      res.end();
    }
  });
  await new Promise<void>((r) => hooks.listen(0, "127.0.0.1", () => r()));
  const hookURL = `http://127.0.0.1:${(hooks.address() as { port: number }).port}/hook`;

  const admin = new AdminClient(HOST, ADMIN);
  const created = await admin.createTenant({ id: "acme", name: "Acme", webhookUrl: hookURL, limits: { max_rooms: 5 } });
  apiKey = created.api_key;
  apiSecret = created.api_secret;
  receiver = new WebhookReceiver(created.tenant.webhook_secret);
  rooms = new RoomServiceClient(HOST, apiKey, apiSecret);
}, 120_000);

afterAll(() => {
  server?.kill("SIGINT");
  hooks?.close();
});

/** Joins over signaling (no media) with a token, returning the WebSocket and received messages. */
async function join(token: string) {
  const ws = new WebSocket(`ws://127.0.0.1:${PORT}/ws`);
  const messages: any[] = [];
  ws.onmessage = (e) => messages.push(JSON.parse(String(e.data)));
  await new Promise((r) => (ws.onopen = r));
  ws.send(JSON.stringify({ type: "join", token }));
  await waitFor(() => messages.some((m) => m.type === "joined" || m.type === "error"));
  return { ws, messages };
}

describe("server SDK against the real server", () => {
  it("creates and lists rooms, and reports API errors", async () => {
    const room = await rooms.createRoom({ name: "standup", maxParticipants: 10, emptyTimeout: 600 });
    expect(room).toMatchObject({ name: "standup", max_participants: 10, empty_timeout_seconds: 600 });
    expect((await rooms.listRooms()).map((r) => r.name)).toEqual(["standup"]);
    await expect(rooms.createRoom({ name: "standup" })).rejects.toMatchObject({ status: 409, code: "conflict" });
    const wrong = new RoomServiceClient(HOST, apiKey, "wrong-secret-wrong-secret-wrong-s");
    await expect(wrong.listRooms()).rejects.toBeInstanceOf(SFUError);
  });

  it("signs join tokens the server accepts, and controls participants", async () => {
    const token = await new AccessToken(apiKey, apiSecret, { identity: "alice" })
      .addGrant({ room: "standup", publish: true, subscribe: true })
      .toJwt();
    const alice = await join(token);
    expect(alice.messages[0]).toMatchObject({ type: "joined", room: "standup", participant: "alice" });

    const parts = await rooms.listParticipants("standup");
    expect(parts.map((p) => p.identity)).toEqual(["alice"]);

    await rooms.mutePublishedTrack("standup", "alice", "microphone", true);
    await waitFor(() => alice.messages.some((m) => m.type === "mute" && m.by_server));

    const updated = await rooms.updateParticipant("standup", "alice", { sources: ["microphone"] });
    expect(updated.grants.sources).toEqual(["microphone"]);
    await waitFor(() => alice.messages.some((m) => m.type === "permissions"));

    await rooms.removeParticipant("standup", "alice");
    await waitFor(() => alice.messages.some((m) => m.type === "removed"));
  });

  it("creates tokens on the server too", async () => {
    const { token } = await rooms.createToken({ room: "standup", identity: "bob", grants: { publish: false } });
    const bob = await join(token);
    expect(bob.messages[0]).toMatchObject({ type: "joined", participant: "bob" });
    bob.ws.close();
  });

  it("receives signed webhooks", async () => {
    await waitFor(() => received.some((e) => e.event === "participant_left" && e.participant?.identity === "alice"));
    const names = received.map((e) => e.event);
    expect(names).toContain("room_started");
    expect(names).toContain("participant_joined");
  });

  it("rejects forged and old webhooks", async () => {
    const r = new WebhookReceiver("secret");
    await expect(r.receive("{}", "t=1,v1=" + "0".repeat(64))).rejects.toThrow("too old");
    const now = Math.floor(Date.now() / 1000);
    await expect(r.receive("{}", `t=${now},v1=${"0".repeat(64)}`)).rejects.toThrow("bad signature");
    await expect(r.receive("{}", null)).rejects.toThrow("malformed");
  });

  it("validates tokens before signing", async () => {
    await expect(new AccessToken(apiKey, apiSecret, { identity: "a b" }).addGrant({ room: "r" }).toJwt()).rejects.toThrow();
    await expect(new AccessToken(apiKey, apiSecret, { identity: "a" }).toJwt()).rejects.toThrow("room");
  });
});

describe("token lifetimes", () => {
  it("rounds to whole seconds and rejects bad API token lifetimes", async () => {
    const tok = await new AccessToken(apiKey, apiSecret, { identity: "dana", ttl: 90.7 }).addGrant({ room: "standup" }).toJwt();
    const payload = JSON.parse(atob(tok.split(".")[1].replace(/-/g, "+").replace(/_/g, "/")));
    expect(Number.isInteger(payload.exp)).toBe(true);
    const { createApiToken } = await import("../src");
    await expect(createApiToken(apiKey, apiSecret, 0)).rejects.toThrow();
    await expect(createApiToken(apiKey, apiSecret, 7200)).rejects.toThrow();
  });
});
