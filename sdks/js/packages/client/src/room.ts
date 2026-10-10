import { Emitter } from "./emitter";
import type {
  ConnectionState,
  DisconnectReason,
  Environment,
  Grants,
  RoomOptions,
  Source,
  SourceQuality,
} from "./types";

/** A track received from another participant. */
export class RemoteTrack {
  constructor(
    readonly source: Source,
    readonly mediaStreamTrack: MediaStreamTrack,
    /** The stream it belongs to: "<identity>/media" (camera + microphone) or "<identity>/screen". */
    readonly stream: MediaStream,
  ) {}

  get kind(): "audio" | "video" {
    return this.mediaStreamTrack.kind as "audio" | "video";
  }
}

/** Something every participant has. */
export class Participant {
  /** Sources that are muted, as last reported (by the participant or the server). */
  readonly muted: Partial<Record<Source, boolean>> = {};
  /** Sources muted by the server; the participant cannot unmute them. */
  readonly mutedByServer: Partial<Record<Source, boolean>> = {};

  constructor(readonly identity: string) {}

  isMuted(source: Source): boolean {
    return !!this.muted[source];
  }
}

/** Another participant in the room. */
export class RemoteParticipant extends Participant {
  /** Tracks being received, by source. */
  readonly tracks = new Map<Source, RemoteTrack>();

  /** The camera and microphone stream, ready for a <video> element. */
  get mediaStream(): MediaStream | undefined {
    return (this.tracks.get("camera") ?? this.tracks.get("microphone"))?.stream;
  }

  /** The screen share stream, if sharing. */
  get screenStream(): MediaStream | undefined {
    return (this.tracks.get("screen") ?? this.tracks.get("screen-audio"))?.stream;
  }
}

export interface RoomEvents {
  connectionStateChanged: (state: ConnectionState) => void;
  connected: () => void;
  /** The room is closed for good (no more reconnects). */
  disconnected: (reason: DisconnectReason, message?: string) => void;
  reconnecting: () => void;
  reconnected: () => void;
  participantConnected: (participant: RemoteParticipant) => void;
  participantDisconnected: (participant: RemoteParticipant) => void;
  trackSubscribed: (track: RemoteTrack, participant: RemoteParticipant) => void;
  trackUnsubscribed: (track: RemoteTrack, participant: RemoteParticipant) => void;
  /** A participant (remote or local) was muted or unmuted. */
  muteChanged: (participant: Participant, source: Source, muted: boolean, byServer: boolean) => void;
  localTrackPublished: (source: Source, track: MediaStreamTrack) => void;
  localTrackUnpublished: (source: Source) => void;
  /** The server changed this participant's permissions. */
  permissionsChanged: (grants: Grants) => void;
  /** A non-fatal error reported by the server (for example "not allowed to publish screen"). */
  error: (message: string) => void;
}

const DEFAULT_QUALITY: Partial<Record<Source, SourceQuality>> = {
  camera: { maxBitrate: 2_500_000, degradationPreference: "balanced" },
  screen: { maxBitrate: 4_000_000, degradationPreference: "maintain-resolution" },
};
const DEFAULT_CAMERA: MediaTrackConstraints = {
  width: { ideal: 1280 },
  height: { ideal: 720 },
  frameRate: { ideal: 30, max: 30 },
};
const DEFAULT_SCREEN: MediaTrackConstraints = {
  width: { max: 1920 },
  height: { max: 1080 },
  frameRate: { ideal: 30, max: 30 },
};
// Screen audio is music or video sound, not a voice: keep it unprocessed.
const SCREEN_AUDIO: MediaTrackConstraints = { echoCancellation: false, noiseSuppression: false, autoGainControl: false };

type Target = "publisher" | "subscriber";

// Join errors that are temporary while reconnecting.
const RETRYABLE = new Set(["participant already in room", "room unavailable"]);

interface ServerMessage {
  type: string;
  room?: string;
  participant?: string;
  participants?: { id: string; muted?: Partial<Record<Source, boolean>> }[];
  sdp?: string;
  target?: Target;
  candidate?: RTCIceCandidateInit;
  message?: string;
  source?: Source;
  muted?: boolean;
  by_server?: boolean;
  grants?: Grants;
}

/** The local participant: publishes camera, microphone and screen. */
export class LocalParticipant extends Participant {
  /** Local tracks being sent, by source. */
  readonly tracks = new Map<Source, MediaStreamTrack>();
  /** Camera and microphone preview stream (local tracks). */
  readonly stream: MediaStream;
  /** Current permissions. */
  grants: Grants = { publish: true, subscribe: true };

  constructor(
    identity: string,
    private readonly room: Room,
  ) {
    super(identity);
    this.stream = room.newMediaStream();
  }

  isMicrophoneEnabled(): boolean {
    const t = this.tracks.get("microphone");
    return !!t && t.enabled && !this.muted.microphone;
  }

  isCameraEnabled(): boolean {
    return !!this.tracks.get("camera") && !this.muted.camera;
  }

  isScreenShareEnabled(): boolean {
    return this.tracks.has("screen");
  }

  /** Turns the microphone on or off. Off keeps the track (instant) and sends silence. */
  async setMicrophoneEnabled(enabled: boolean, deviceId?: string): Promise<void> {
    if (enabled && this.mutedByServer.microphone) throw new Error("muted by the server");
    let track = this.tracks.get("microphone");
    if (enabled && !track) {
      track = await this.room.capture("audio", deviceId);
      this.stream.addTrack(track);
      await this.room.publish("microphone", track, this.stream);
    }
    if (track) track.enabled = enabled;
    this.setMuted("microphone", !enabled);
  }

  /** Turns the camera on or off. Off really stops the camera (its light goes out). */
  async setCameraEnabled(enabled: boolean, deviceId?: string): Promise<void> {
    if (enabled && this.mutedByServer.camera) throw new Error("muted by the server");
    const current = this.tracks.get("camera");
    if (!enabled) {
      if (current) {
        await this.room.replace("camera", null);
        current.stop();
        this.stream.removeTrack(current);
        this.tracks.delete("camera");
      }
      this.setMuted("camera", true);
      return;
    }
    if (current && current.readyState === "live") {
      current.enabled = true; // for example after a server mute was lifted
      this.setMuted("camera", false);
      return;
    }
    const track = await this.room.capture("video", deviceId);
    this.stream.addTrack(track);
    if (this.room.hasSender("camera")) {
      await this.room.replace("camera", track); // same slot: no renegotiation
      this.tracks.set("camera", track);
    } else {
      await this.room.publish("camera", track, this.stream);
    }
    this.setMuted("camera", false);
  }

  /** Starts or stops sharing the screen (with screen audio when the browser offers it). */
  async setScreenShareEnabled(enabled: boolean): Promise<void> {
    if (!enabled) {
      await this.room.unpublish("screen");
      await this.room.unpublish("screen-audio");
      return;
    }
    if (this.tracks.has("screen")) return;
    const display = await this.room.captureScreen();
    const video = display.getVideoTracks()[0];
    if (!video) throw new Error("no screen video");
    video.contentHint = "detail"; // text and detail matter more than motion
    video.addEventListener("ended", () => void this.setScreenShareEnabled(false));
    try {
      await this.room.publish("screen", video, display, false);
    } catch (err) {
      display.getTracks().forEach((t) => t.stop());
      throw err;
    }
    const audio = display.getAudioTracks()[0];
    if (audio) {
      // Screen audio is optional: if it is not allowed, share the picture only.
      await this.room.publish("screen-audio", audio, display, false).catch(() => audio.stop());
    }
    await this.room.negotiate();
  }

  /** Switches the microphone or camera to another device while keeping its state. */
  async switchDevice(kind: "audioinput" | "videoinput", deviceId: string): Promise<void> {
    const source: Source = kind === "audioinput" ? "microphone" : "camera";
    const old = this.tracks.get(source);
    if (!old) return;
    const track = await this.room.capture(kind === "audioinput" ? "audio" : "video", deviceId);
    track.enabled = old.enabled;
    await this.room.replace(source, track);
    this.stream.removeTrack(old);
    old.stop();
    this.stream.addTrack(track);
    this.tracks.set(source, track);
  }

  private setMuted(source: Source, muted: boolean): void {
    if (!!this.muted[source] === muted) return;
    this.muted[source] = muted;
    this.room.sendMute(source, muted);
  }
}

/**
 * A room on the SFU.
 *
 * ```ts
 * const room = new Room();
 * room.on("trackSubscribed", (track, participant) => attach(track, participant));
 * await room.connect("wss://sfu.example.com", token);
 * await room.localParticipant.setCameraEnabled(true);
 * await room.localParticipant.setMicrophoneEnabled(true);
 * ```
 */
export class Room extends Emitter<RoomEvents> {
  state: ConnectionState = "disconnected";
  name = "";
  localParticipant: LocalParticipant;
  readonly remoteParticipants = new Map<string, RemoteParticipant>();

  private readonly env: Environment;
  private readonly opts: Required<Omit<RoomOptions, "env" | "quality">> & { quality: Partial<Record<Source, SourceQuality>> };
  private url = "";
  private token = "";
  private ws?: WebSocket;
  private pub?: RTCPeerConnection;
  private sub?: RTCPeerConnection;
  private pending: Record<Target, RTCIceCandidateInit[]> = { publisher: [], subscriber: [] };
  private senders = new Map<Source, RTCRtpSender>();
  private queue: Promise<void> = Promise.resolve();
  private negotiating = false;
  private negotiateAgain = false;
  private closing = false;
  private attempt = 0;

  constructor(options: RoomOptions = {}) {
    super();
    const g = globalThis as unknown as Partial<Environment> & { navigator?: Navigator };
    const md = g.navigator?.mediaDevices;
    this.env = {
      WebSocket: options.env?.WebSocket ?? (g.WebSocket as typeof WebSocket),
      RTCPeerConnection: options.env?.RTCPeerConnection ?? (g.RTCPeerConnection as typeof RTCPeerConnection),
      MediaStream: options.env?.MediaStream ?? (g.MediaStream as typeof MediaStream),
      getUserMedia: options.env?.getUserMedia ?? ((c) => md!.getUserMedia(c)),
      getDisplayMedia: options.env?.getDisplayMedia ?? ((c) => md!.getDisplayMedia(c)),
    };
    this.opts = {
      iceServers: options.iceServers ?? [],
      autoReconnect: options.autoReconnect ?? true,
      maxReconnectAttempts: options.maxReconnectAttempts ?? 5,
      cameraConstraints: options.cameraConstraints ?? DEFAULT_CAMERA,
      screenConstraints: options.screenConstraints ?? DEFAULT_SCREEN,
      quality: { ...DEFAULT_QUALITY, ...options.quality },
    };
    this.localParticipant = new LocalParticipant("", this);
  }

  /** Lists media devices (labels appear after the user allowed camera or microphone). */
  static async getDevices(kind?: MediaDeviceKind): Promise<MediaDeviceInfo[]> {
    const all = await navigator.mediaDevices.enumerateDevices();
    return kind ? all.filter((d) => d.kind === kind) : all;
  }

  /**
   * Connects with a join token. url is the SFU address ("https://sfu.example.com",
   * "wss://sfu.example.com" or the full ".../ws"). Resolves when joined; rejects
   * with the server's reason (for example "invalid token", "room not found").
   */
  connect(url: string, token: string): Promise<void> {
    if (this.state !== "disconnected") return Promise.reject(new Error(`already ${this.state}`));
    this.url = signalingURL(url);
    this.token = token;
    this.closing = false;
    this.attempt = 0;
    this.setState("connecting");
    return this.open();
  }

  /** Leaves the room and stops all local tracks. */
  disconnect(): void {
    if (this.state === "disconnected") return;
    this.closing = true;
    this.send({ type: "leave" });
    this.teardown();
    for (const t of this.localParticipant.tracks.values()) t.stop();
    this.localParticipant.tracks.clear();
    this.finish("client");
  }

  // ---------- connection ----------

  private open(): Promise<void> {
    return new Promise((resolve, reject) => {
      let joined = false;
      const ws = new this.env.WebSocket(this.url);
      this.ws = ws;
      this.pub = this.newPC("publisher");
      this.sub = this.newPC("subscriber");
      this.sub.ontrack = (e) => this.onTrack(e);

      ws.onopen = () => this.send({ type: "join", token: this.token });
      ws.onmessage = (ev) => {
        const msg = JSON.parse(String(ev.data)) as ServerMessage;
        this.queue = this.queue
          .then(() => this.handle(msg))
          .then(() => {
            if (msg.type === "joined" && !joined) {
              joined = true;
              resolve();
            }
          })
          .catch((err) => this.emit("error", String(err?.message ?? err)));
        if (msg.type === "error" && !joined) {
          ws.onclose = null;
          this.teardown();
          // While reconnecting, the server may still hold the old session
          // (until it notices the drop) or the room may be moving: retry.
          if (this.state === "reconnecting" && RETRYABLE.has(msg.message ?? "")) {
            void this.retry();
          } else {
            this.finish("error", msg.message);
          }
          reject(new Error(msg.message));
        }
      };
      ws.onclose = () => {
        if (this.closing || this.ws !== ws) return;
        if (!joined) {
          this.teardown();
          if (this.state === "reconnecting") return void this.retry().then(resolve, reject);
          this.finish("connection", "could not connect");
          reject(new Error("could not connect"));
          return;
        }
        void this.retry();
      };
    });
  }

  /** Reconnects after a lost connection, publishing the same local tracks again. */
  private async retry(): Promise<void> {
    this.teardown();
    if (!this.opts.autoReconnect || this.attempt >= this.opts.maxReconnectAttempts) {
      this.finish("connection", "connection lost");
      return;
    }
    if (this.state !== "reconnecting") {
      this.setState("reconnecting");
      this.emit("reconnecting");
    }
    const delay = Math.min(1000 * 2 ** this.attempt, 10_000);
    this.attempt++;
    await new Promise((r) => setTimeout(r, delay));
    if (this.closing) return;
    for (const p of [...this.remoteParticipants.values()]) this.removeRemote(p.identity);
    try {
      await this.open();
    } catch {
      return; // open already finished or retried
    }
  }

  private teardown(): void {
    const ws = this.ws;
    this.ws = undefined;
    if (ws) {
      ws.onclose = null;
      ws.onmessage = null;
      try {
        ws.close();
      } catch {
        /* already closed */
      }
    }
    this.pub?.close();
    this.sub?.close();
    this.pub = this.sub = undefined;
    this.senders.clear();
    this.pending = { publisher: [], subscriber: [] };
    this.negotiating = this.negotiateAgain = false;
    this.queue = Promise.resolve();
  }

  private finish(reason: DisconnectReason, message?: string): void {
    this.closing = true;
    for (const p of [...this.remoteParticipants.values()]) this.removeRemote(p.identity);
    for (const t of this.localParticipant.tracks.values()) t.stop();
    // A fresh local participant: no stale tracks or mute state on the next connect.
    this.localParticipant = new LocalParticipant("", this);
    this.setState("disconnected");
    this.emit("disconnected", reason, message);
  }

  private setState(s: ConnectionState): void {
    if (this.state === s) return;
    this.state = s;
    this.emit("connectionStateChanged", s);
  }

  private newPC(target: Target): RTCPeerConnection {
    const pc = new this.env.RTCPeerConnection({ iceServers: this.opts.iceServers });
    pc.onicecandidate = (e) => {
      if (e.candidate) this.send({ type: "candidate", target, candidate: e.candidate.toJSON() });
    };
    return pc;
  }

  private send(msg: object): void {
    if (this.ws?.readyState === 1 /* OPEN */) this.ws.send(JSON.stringify(msg));
  }

  // ---------- messages ----------

  private async handle(msg: ServerMessage): Promise<void> {
    switch (msg.type) {
      case "joined":
        return this.onJoined(msg);
      case "participant-joined":
        this.addRemote(msg.participant!);
        return;
      case "participant-left":
        this.removeRemote(msg.participant!);
        return;
      case "mute":
        return this.onMute(msg);
      case "permissions":
        this.localParticipant.grants = msg.grants!;
        this.emit("permissionsChanged", msg.grants!);
        return;
      case "answer":
        await this.setRemote("publisher", this.pub!, { type: "answer", sdp: msg.sdp });
        this.negotiating = false;
        if (this.negotiateAgain) {
          this.negotiateAgain = false;
          await this.negotiate();
        }
        return;
      case "offer": {
        const sub = this.sub!;
        await this.setRemote("subscriber", sub, { type: "offer", sdp: msg.sdp });
        const answer = await sub.createAnswer();
        await sub.setLocalDescription(answer);
        this.send({ type: "answer", sdp: answer.sdp });
        return;
      }
      case "candidate": {
        const pc = msg.target === "publisher" ? this.pub : this.sub;
        if (!pc) return;
        if (!pc.remoteDescription) this.pending[msg.target!].push(msg.candidate!);
        else await pc.addIceCandidate(msg.candidate!).catch(() => {});
        return;
      }
      case "removed":
        this.closing = true;
        this.teardown();
        for (const t of this.localParticipant.tracks.values()) t.stop();
        this.localParticipant.tracks.clear();
        this.finish("removed", msg.message);
        return;
      case "error":
        if (this.negotiating && this.pub?.signalingState === "have-local-offer") {
          // The server rejected our offer and sends no answer: undo it, so
          // later changes can be offered again.
          await this.pub.setLocalDescription({ type: "rollback" }).catch(() => {});
          this.negotiating = false;
          if (this.negotiateAgain) {
            this.negotiateAgain = false;
            await this.negotiate();
          }
        }
        this.emit("error", msg.message ?? "error");
        return;
    }
  }

  private async onJoined(msg: ServerMessage): Promise<void> {
    const reconnect = this.state === "reconnecting";
    this.name = msg.room ?? "";
    if (this.localParticipant.identity !== msg.participant) {
      const old = this.localParticipant;
      this.localParticipant = new LocalParticipant(msg.participant!, this);
      for (const [s, t] of old.tracks) {
        this.localParticipant.tracks.set(s, t);
        if (t.kind && (s === "camera" || s === "microphone")) this.localParticipant.stream.addTrack(t);
      }
    }
    for (const p of msg.participants ?? []) {
      const rp = this.addRemote(p.id);
      Object.assign(rp.muted, p.muted ?? {});
    }
    this.attempt = 0;
    this.setState("connected");
    // Send the local tracks on this connection: after a reconnect, and any
    // published while still connecting (transceivers are only added when
    // connected, so nothing is added twice).
    const local = this.localParticipant;
    for (const [source, track] of local.tracks) {
      if (this.senders.has(source)) continue;
      const stream = source === "screen" || source === "screen-audio" ? this.newMediaStream([track]) : local.stream;
      this.addTransceiver(source, track, stream);
    }
    if (local.tracks.size > 0) await this.negotiate();
    if (reconnect) {
      for (const [source, muted] of Object.entries(local.muted)) {
        if (muted) this.send({ type: "mute", source, muted: true });
      }
      this.emit("reconnected");
    } else {
      this.emit("connected");
    }
  }

  private onMute(msg: ServerMessage): void {
    const source = msg.source!;
    const local = this.localParticipant;
    const p: Participant = msg.participant === local.identity ? local : this.addRemote(msg.participant!);
    p.muted[source] = msg.muted!;
    if (msg.by_server) p.mutedByServer[source] = msg.muted!;
    if (p === local && msg.by_server) {
      // A server mute stops the track (the server drops it anyway); a server
      // unmute turns it back on.
      const t = local.tracks.get(source);
      if (t) t.enabled = !msg.muted;
    }
    this.emit("muteChanged", p, source, msg.muted!, !!msg.by_server);
  }

  private addRemote(identity: string): RemoteParticipant {
    let p = this.remoteParticipants.get(identity);
    if (!p) {
      p = new RemoteParticipant(identity);
      this.remoteParticipants.set(identity, p);
      this.emit("participantConnected", p);
    }
    return p;
  }

  private removeRemote(identity: string): void {
    const p = this.remoteParticipants.get(identity);
    if (!p) return;
    for (const t of [...p.tracks.values()]) {
      p.tracks.delete(t.source);
      this.emit("trackUnsubscribed", t, p);
    }
    this.remoteParticipants.delete(identity);
    this.emit("participantDisconnected", p);
  }

  private onTrack(e: RTCTrackEvent): void {
    const stream = e.streams[0];
    if (!stream) return;
    const [identity, group] = stream.id.split("/");
    const kind = e.track.kind;
    const source: Source =
      group === "screen" ? (kind === "audio" ? "screen-audio" : "screen") : kind === "audio" ? "microphone" : "camera";
    const p = this.addRemote(identity);
    const track = new RemoteTrack(source, e.track, stream);
    p.tracks.set(source, track);
    this.emit("trackSubscribed", track, p);
    const gone = () => {
      if (p.tracks.get(source) === track) {
        p.tracks.delete(source);
        this.emit("trackUnsubscribed", track, p);
      }
    };
    stream.addEventListener?.("removetrack", (ev) => {
      if ((ev as MediaStreamTrackEvent).track === e.track) gone();
    });
    e.track.addEventListener?.("ended", gone);
  }

  private async setRemote(target: Target, pc: RTCPeerConnection, desc: RTCSessionDescriptionInit): Promise<void> {
    await pc.setRemoteDescription(desc);
    for (const c of this.pending[target].splice(0)) await pc.addIceCandidate(c).catch(() => {});
  }

  // ---------- publishing (used by LocalParticipant) ----------

  /** @internal */
  newMediaStream(tracks?: MediaStreamTrack[]): MediaStream {
    return tracks ? new this.env.MediaStream(tracks) : new this.env.MediaStream();
  }

  /** @internal */
  async capture(kind: "audio" | "video", deviceId?: string): Promise<MediaStreamTrack> {
    const base: MediaTrackConstraints = kind === "video" ? this.opts.cameraConstraints : {};
    const c: MediaTrackConstraints = deviceId ? { ...base, deviceId: { exact: deviceId } } : base;
    const stream = await this.env.getUserMedia(kind === "video" ? { video: c } : { audio: deviceId ? c : true });
    return stream.getTracks()[0];
  }

  /** @internal */
  captureScreen(): Promise<MediaStream> {
    return this.env.getDisplayMedia({ video: this.opts.screenConstraints, audio: SCREEN_AUDIO });
  }

  /** @internal */
  hasSender(source: Source): boolean {
    return this.senders.has(source);
  }

  /** @internal Adds a track and (by default) renegotiates. */
  async publish(source: Source, track: MediaStreamTrack, stream: MediaStream, negotiate = true): Promise<void> {
    const g = this.localParticipant.grants;
    if (!g.publish || (g.sources?.length && !g.sources.includes(source))) {
      throw new Error(`not allowed to publish ${source}`);
    }
    this.localParticipant.tracks.set(source, track);
    if (this.pub && this.state === "connected") this.addTransceiver(source, track, stream);
    this.emit("localTrackPublished", source, track);
    if (negotiate && this.pub) await this.negotiate();
  }

  private addTransceiver(source: Source, track: MediaStreamTrack, stream: MediaStream): void {
    const q = this.opts.quality[source];
    const init: RTCRtpTransceiverInit = { direction: "sendonly", streams: [stream] };
    if (q?.maxBitrate) init.sendEncodings = [{ maxBitrate: q.maxBitrate }];
    const t = this.pub!.addTransceiver(track, init);
    this.senders.set(source, t.sender);
    if (q?.degradationPreference) {
      try {
        const params = t.sender.getParameters() as RTCRtpSendParameters & { degradationPreference?: string };
        params.degradationPreference = q.degradationPreference;
        void t.sender.setParameters(params).catch(() => {});
      } catch {
        /* optional */
      }
    }
  }

  /** @internal Removes a source and renegotiates. */
  async unpublish(source: Source): Promise<void> {
    const track = this.localParticipant.tracks.get(source);
    if (!track) return;
    track.stop();
    this.localParticipant.tracks.delete(source);
    const sender = this.senders.get(source);
    this.senders.delete(source);
    if (sender && this.pub) {
      this.pub.removeTrack(sender);
      await this.negotiate();
    }
    this.emit("localTrackUnpublished", source);
  }

  /** @internal */
  async replace(source: Source, track: MediaStreamTrack | null): Promise<void> {
    await this.senders.get(source)?.replaceTrack(track);
  }

  /** @internal */
  sendMute(source: Source, muted: boolean): void {
    this.send({ type: "mute", source, muted });
    this.emit("muteChanged", this.localParticipant, source, muted, false);
  }

  /**
   * @internal Sends a publisher offer describing every track. Only one offer
   * is in flight; a request while waiting runs once the answer arrives.
   */
  async negotiate(): Promise<void> {
    if (!this.pub || this.state !== "connected") return;
    if (this.negotiating) {
      this.negotiateAgain = true;
      return;
    }
    this.negotiating = true;
    const tracks: Record<string, Source> = {};
    for (const [source, t] of this.localParticipant.tracks) tracks[t.id] = source;
    const offer = await this.pub.createOffer();
    await this.pub.setLocalDescription(offer);
    this.send({ type: "offer", sdp: offer.sdp, tracks });
  }
}

/** Turns a server address into its signaling WebSocket URL. */
export function signalingURL(url: string): string {
  let u = url.trim().replace(/\/+$/, "");
  if (u.startsWith("https://")) u = "wss://" + u.slice(8);
  else if (u.startsWith("http://")) u = "ws://" + u.slice(7);
  else if (!u.startsWith("ws://") && !u.startsWith("wss://")) u = "wss://" + u;
  return u.endsWith("/ws") ? u : u + "/ws";
}
