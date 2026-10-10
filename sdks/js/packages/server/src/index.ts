import { hmacSHA256, signJWT, timingSafeEqual, toHex } from "./jwt";

/** What a track carries. */
export type Source = "camera" | "microphone" | "screen" | "screen-audio";

/** What a participant may do in the room. */
export interface Grants {
  publish?: boolean;
  subscribe?: boolean;
  /** Sources allowed to publish; empty or missing means all. */
  sources?: Source[];
}

const ID = /^[A-Za-z0-9_-]{1,64}$/;

// ---------- join tokens ----------

export interface AccessTokenOptions {
  /** The participant's identity in the room (1–64 letters, digits, '-' or '_'). */
  identity: string;
  /** Lifetime in seconds (default 3600, at most 86400). */
  ttl?: number;
}

/**
 * A join token for one participant. Give it to the browser, which passes it
 * to `room.connect()`.
 *
 * ```ts
 * const token = await new AccessToken(apiKey, apiSecret, { identity: "alice" })
 *   .addGrant({ room: "standup", publish: true, subscribe: true })
 *   .toJwt();
 * ```
 */
export class AccessToken {
  private room = "";
  private grants: Required<Pick<Grants, "publish" | "subscribe">> & Grants = { publish: true, subscribe: true };

  constructor(
    private readonly apiKey: string,
    private readonly apiSecret: string,
    private readonly opts: AccessTokenOptions,
  ) {}

  /** Sets the room and permissions. Publish and subscribe default to true. */
  addGrant(grant: { room: string } & Grants): this {
    this.room = grant.room;
    this.grants = {
      publish: grant.publish ?? true,
      subscribe: grant.subscribe ?? true,
      ...(grant.sources?.length ? { sources: grant.sources } : {}),
    };
    return this;
  }

  /** Returns the signed token. */
  async toJwt(): Promise<string> {
    const ttl = Math.floor(this.opts.ttl ?? 3600); // the server needs whole seconds
    if (!ID.test(this.opts.identity)) throw new Error("identity must be 1-64 letters, digits, '-' or '_'");
    if (!ID.test(this.room)) throw new Error("call addGrant with a room of 1-64 letters, digits, '-' or '_'");
    if (!(ttl > 0 && ttl <= 86400)) throw new Error("ttl must be 1 to 86400 seconds");
    const now = Math.floor(Date.now() / 1000);
    return signJWT(this.apiSecret, {
      iss: this.apiKey,
      sub: this.opts.identity,
      room: this.room,
      iat: now,
      nbf: now,
      exp: now + ttl,
      grants: this.grants,
    });
  }
}

/** Creates a short-lived API token for the server API (at most 3600 s). */
export async function createApiToken(apiKey: string, apiSecret: string, ttlSeconds = 300): Promise<string> {
  const ttl = Math.floor(ttlSeconds); // the server needs whole seconds
  if (!(ttl > 0 && ttl <= 3600)) throw new Error("API token ttl must be 1 to 3600 seconds");
  const now = Math.floor(Date.now() / 1000);
  return signJWT(apiSecret, { iss: apiKey, aud: "sfu-api", iat: now, exp: now + ttl });
}

// ---------- server API ----------

export interface Room {
  name: string;
  created_at: string;
  num_participants: number;
  max_participants: number;
  empty_timeout_seconds: number;
  participants?: Participant[];
}

export interface Participant {
  identity: string;
  joined_at: string;
  sources: Source[];
  muted: Partial<Record<Source, boolean>>;
  grants: { publish: boolean; subscribe: boolean; sources: Source[] };
}

/** An error answer from the SFU ({"error": {code, message}}). */
export class SFUError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = "SFUError";
  }
}

async function call<T>(base: string, auth: string, method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(base.replace(/\/+$/, "") + path, {
    method,
    headers: { Authorization: `Bearer ${auth}`, ...(body !== undefined ? { "Content-Type": "application/json" } : {}) },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (res.status === 204) return undefined as T;
  const data = (await res.json().catch(() => ({}))) as { error?: { code: string; message: string } };
  if (!res.ok) throw new SFUError(res.status, data.error?.code ?? "unknown", data.error?.message ?? res.statusText);
  return data as T;
}

/**
 * Controls rooms and participants through the server API (/v1).
 *
 * ```ts
 * const rooms = new RoomServiceClient("https://sfu.example.com", apiKey, apiSecret);
 * await rooms.createRoom({ name: "standup", maxParticipants: 10 });
 * ```
 */
export class RoomServiceClient {
  constructor(
    private readonly host: string,
    private readonly apiKey: string,
    private readonly apiSecret: string,
  ) {}

  private async req<T>(method: string, path: string, body?: unknown): Promise<T> {
    return call<T>(this.host, await createApiToken(this.apiKey, this.apiSecret, 60), method, path, body);
  }

  /** Creates a room. emptyTimeout is in seconds (default 300). */
  createRoom(opts: { name: string; maxParticipants?: number; emptyTimeout?: number }): Promise<Room> {
    return this.req("POST", "/v1/rooms", {
      name: opts.name,
      ...(opts.maxParticipants !== undefined ? { max_participants: opts.maxParticipants } : {}),
      ...(opts.emptyTimeout !== undefined ? { empty_timeout_seconds: opts.emptyTimeout } : {}),
    });
  }

  async listRooms(): Promise<Room[]> {
    return (await this.req<{ rooms: Room[] }>("GET", "/v1/rooms")).rooms;
  }

  /** The room with its participants. */
  getRoom(name: string): Promise<Room> {
    return this.req("GET", `/v1/rooms/${encodeURIComponent(name)}`);
  }

  /** Closes the room: everyone is disconnected. */
  deleteRoom(name: string): Promise<void> {
    return this.req("DELETE", `/v1/rooms/${encodeURIComponent(name)}`);
  }

  async listParticipants(room: string): Promise<Participant[]> {
    return (await this.req<{ participants: Participant[] }>("GET", `/v1/rooms/${encodeURIComponent(room)}/participants`))
      .participants;
  }

  /** Removes (kicks) a participant. */
  removeParticipant(room: string, identity: string): Promise<void> {
    return this.req("DELETE", `/v1/rooms/${encodeURIComponent(room)}/participants/${encodeURIComponent(identity)}`);
  }

  /** Mutes or unmutes a participant's source from the server; it cannot unmute itself. */
  mutePublishedTrack(room: string, identity: string, source: Source, muted: boolean): Promise<void> {
    return this.req(
      "POST",
      `/v1/rooms/${encodeURIComponent(room)}/participants/${encodeURIComponent(identity)}/mute`,
      { source, muted },
    );
  }

  /** Changes a participant's permissions live; missing fields keep their value. */
  updateParticipant(room: string, identity: string, grants: Grants): Promise<Participant> {
    return this.req("PATCH", `/v1/rooms/${encodeURIComponent(room)}/participants/${encodeURIComponent(identity)}`, {
      grants,
    });
  }

  /** Asks the server to sign a join token (instead of AccessToken). */
  createToken(opts: { room: string; identity: string; ttl?: number; grants?: Grants }): Promise<{ token: string; expires_at: string }> {
    return this.req("POST", "/v1/tokens", {
      room: opts.room,
      identity: opts.identity,
      ...(opts.ttl ? { ttl_seconds: Math.floor(opts.ttl) } : {}),
      ...(opts.grants ? { grants: opts.grants } : {}),
    });
  }
}

// ---------- webhooks ----------

export interface WebhookEvent {
  id: string;
  event:
    | "room_started"
    | "room_finished"
    | "participant_joined"
    | "participant_left"
    | "track_published"
    | "track_unpublished";
  created_at: string;
  room: { name: string };
  participant?: { identity: string; duration_seconds?: number };
  track?: { source: Source };
}

/**
 * Checks and parses webhooks. Pass the raw request body (not re-serialized
 * JSON) and the X-SFU-Signature header.
 *
 * ```ts
 * const receiver = new WebhookReceiver(webhookSecret);
 * const event = await receiver.receive(await req.text(), req.headers.get("x-sfu-signature"));
 * ```
 */
export class WebhookReceiver {
  /** toleranceSeconds rejects signatures older (or newer) than this (default 300). */
  constructor(
    private readonly webhookSecret: string,
    private readonly toleranceSeconds = 300,
  ) {}

  /** Verifies the signature and returns the event; throws if it is invalid. */
  async receive(rawBody: string, signatureHeader: string | null | undefined): Promise<WebhookEvent> {
    const m = /^t=(\d+),v1=([0-9a-f]{64})$/.exec(signatureHeader ?? "");
    if (!m) throw new Error("webhook: missing or malformed signature");
    const t = Number(m[1]);
    if (Math.abs(Date.now() / 1000 - t) > this.toleranceSeconds) throw new Error("webhook: signature too old");
    const expected = toHex(await hmacSHA256(this.webhookSecret, `${m[1]}.${rawBody}`));
    if (!timingSafeEqual(expected, m[2])) throw new Error("webhook: bad signature");
    return JSON.parse(rawBody) as WebhookEvent;
  }
}

// ---------- admin API (operator) ----------

export interface Limits {
  max_rooms?: number;
  max_participants?: number;
  max_room_size?: number;
}

export interface Tenant {
  id: string;
  name: string;
  auto_create_rooms: boolean;
  webhook_url: string;
  limits: Limits;
  disabled: boolean;
  created_at: string;
}

/** The operator admin API (/admin/v1). Never ship this or the admin secret to tenants. */
export class AdminClient {
  constructor(
    private readonly host: string,
    private readonly adminSecret: string,
  ) {}

  private req<T>(method: string, path: string, body?: unknown): Promise<T> {
    return call<T>(this.host, this.adminSecret, method, path, body);
  }

  /** Creates a tenant; the API secret and webhook secret are returned only here. */
  createTenant(t: { id: string; name?: string; autoCreateRooms?: boolean; webhookUrl?: string; limits?: Limits }): Promise<{
    tenant: Tenant & { webhook_secret: string };
    api_key: string;
    api_secret: string;
  }> {
    return this.req("POST", "/admin/v1/tenants", {
      id: t.id,
      name: t.name ?? "",
      auto_create_rooms: t.autoCreateRooms ?? false,
      webhook_url: t.webhookUrl ?? "",
      limits: t.limits ?? {},
    });
  }

  async listTenants(): Promise<Tenant[]> {
    return (await this.req<{ tenants: Tenant[] }>("GET", "/admin/v1/tenants")).tenants;
  }

  getTenant(id: string): Promise<Tenant> {
    return this.req("GET", `/admin/v1/tenants/${encodeURIComponent(id)}`);
  }

  updateTenant(
    id: string,
    change: { name?: string; autoCreateRooms?: boolean; webhookUrl?: string; limits?: Limits; disabled?: boolean; rotateWebhookSecret?: boolean },
  ): Promise<Tenant & { webhook_secret?: string }> {
    return this.req("PATCH", `/admin/v1/tenants/${encodeURIComponent(id)}`, {
      ...(change.name !== undefined ? { name: change.name } : {}),
      ...(change.autoCreateRooms !== undefined ? { auto_create_rooms: change.autoCreateRooms } : {}),
      ...(change.webhookUrl !== undefined ? { webhook_url: change.webhookUrl } : {}),
      ...(change.limits !== undefined ? { limits: change.limits } : {}),
      ...(change.disabled !== undefined ? { disabled: change.disabled } : {}),
      ...(change.rotateWebhookSecret ? { rotate_webhook_secret: true } : {}),
    });
  }

  createKey(tenantId: string): Promise<{ api_key: string; api_secret: string }> {
    return this.req("POST", `/admin/v1/tenants/${encodeURIComponent(tenantId)}/keys`);
  }

  async listKeys(tenantId: string): Promise<{ api_key: string; created_at: string; revoked: boolean }[]> {
    return (await this.req<{ keys: { api_key: string; created_at: string; revoked: boolean }[] }>(
      "GET",
      `/admin/v1/tenants/${encodeURIComponent(tenantId)}/keys`,
    )).keys;
  }

  revokeKey(tenantId: string, apiKey: string): Promise<void> {
    return this.req("DELETE", `/admin/v1/tenants/${encodeURIComponent(tenantId)}/keys/${encodeURIComponent(apiKey)}`);
  }

  /** Participant-minutes per day; from and to are "YYYY-MM-DD" (default: this month). */
  usage(tenantId: string, from?: string, to?: string): Promise<{
    tenant_id: string;
    from: string;
    to: string;
    days: { day: string; participant_minutes: number }[];
    total_participant_minutes: number;
  }> {
    const q = new URLSearchParams({ ...(from ? { from } : {}), ...(to ? { to } : {}) }).toString();
    return this.req("GET", `/admin/v1/tenants/${encodeURIComponent(tenantId)}/usage${q ? `?${q}` : ""}`);
  }
}
