// Minimal fakes of the browser APIs the SDK uses, so it can be tested in Node.

let nextId = 1;

export class FakeTrack {
  id = `track-${nextId++}`;
  enabled = true;
  readyState: "live" | "ended" = "live";
  contentHint = "";
  private listeners: Record<string, (() => void)[]> = {};
  constructor(readonly kind: "audio" | "video") {}
  stop() {
    this.readyState = "ended";
  }
  addEventListener(type: string, fn: () => void) {
    (this.listeners[type] ??= []).push(fn);
  }
  fire(type: string) {
    for (const fn of this.listeners[type] ?? []) fn();
  }
}

export class FakeMediaStream {
  id = `stream-${nextId++}`;
  tracks: FakeTrack[];
  private listeners: Record<string, ((e: { track: FakeTrack }) => void)[]> = {};
  constructor(tracks: FakeTrack[] = []) {
    this.tracks = [...tracks];
  }
  getTracks() {
    return this.tracks;
  }
  getVideoTracks() {
    return this.tracks.filter((t) => t.kind === "video");
  }
  getAudioTracks() {
    return this.tracks.filter((t) => t.kind === "audio");
  }
  addTrack(t: FakeTrack) {
    this.tracks.push(t);
  }
  removeTrack(t: FakeTrack) {
    this.tracks = this.tracks.filter((x) => x !== t);
    for (const fn of this.listeners.removetrack ?? []) fn({ track: t });
  }
  addEventListener(type: string, fn: (e: { track: FakeTrack }) => void) {
    (this.listeners[type] ??= []).push(fn);
  }
}

export class FakeSender {
  track: FakeTrack | null;
  params: Record<string, unknown> = { encodings: [] };
  constructor(track: FakeTrack) {
    this.track = track;
  }
  async replaceTrack(t: FakeTrack | null) {
    this.track = t;
  }
  getParameters() {
    return this.params;
  }
  async setParameters(p: Record<string, unknown>) {
    this.params = p;
  }
}

export class FakePC {
  static all: FakePC[] = [];
  localDescription: RTCSessionDescriptionInit | null = null;
  remoteDescription: RTCSessionDescriptionInit | null = null;
  transceivers: { track: FakeTrack; init: RTCRtpTransceiverInit; sender: FakeSender }[] = [];
  removed: FakeSender[] = [];
  candidates: RTCIceCandidateInit[] = [];
  closed = false;
  onicecandidate: ((e: { candidate: { toJSON(): RTCIceCandidateInit } | null }) => void) | null = null;
  ontrack: ((e: { track: FakeTrack; streams: FakeMediaStream[] }) => void) | null = null;
  constructor() {
    FakePC.all.push(this);
  }
  addTransceiver(track: FakeTrack, init: RTCRtpTransceiverInit) {
    const sender = new FakeSender(track);
    this.transceivers.push({ track, init, sender });
    return { sender };
  }
  removeTrack(sender: FakeSender) {
    this.removed.push(sender);
  }
  async createOffer() {
    return { type: "offer" as const, sdp: `offer-${this.transceivers.length}` };
  }
  async createAnswer() {
    return { type: "answer" as const, sdp: "answer" };
  }
  get signalingState() {
    return this.localDescription?.type === "offer" && !this.remoteDescription ? "have-local-offer" : "stable";
  }
  async setLocalDescription(d: RTCSessionDescriptionInit) {
    this.localDescription = d.type === "rollback" ? null : d;
    if (d.type === "offer") this.remoteDescription = null;
  }
  async setRemoteDescription(d: RTCSessionDescriptionInit) {
    this.remoteDescription = d;
  }
  async addIceCandidate(c: RTCIceCandidateInit) {
    this.candidates.push(c);
  }
  close() {
    this.closed = true;
  }
}

export class FakeWebSocket {
  static last: FakeWebSocket;
  readyState = 0;
  sent: Record<string, any>[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((e: { data: string }) => void) | null = null;
  onclose: (() => void) | null = null;
  constructor(readonly url: string) {
    FakeWebSocket.last = this;
    queueMicrotask(() => {
      this.readyState = 1;
      this.onopen?.();
    });
  }
  send(data: string) {
    this.sent.push(JSON.parse(data));
  }
  close() {
    this.readyState = 3;
  }
  /** The server sends msg. */
  push(msg: object) {
    this.onmessage?.({ data: JSON.stringify(msg) });
  }
  /** The connection drops. */
  drop() {
    this.readyState = 3;
    this.onclose?.();
  }
  of(type: string) {
    return this.sent.filter((m) => m.type === type);
  }
}

export const fakeEnv = () => ({
  WebSocket: FakeWebSocket as unknown as typeof WebSocket,
  RTCPeerConnection: FakePC as unknown as typeof RTCPeerConnection,
  MediaStream: FakeMediaStream as unknown as typeof MediaStream,
  getUserMedia: async (c: MediaStreamConstraints) =>
    new FakeMediaStream([new FakeTrack(c.video ? "video" : "audio")]) as unknown as MediaStream,
  getDisplayMedia: async () =>
    new FakeMediaStream([new FakeTrack("video"), new FakeTrack("audio")]) as unknown as MediaStream,
});

export const tick = () => new Promise((r) => setTimeout(r, 0));
