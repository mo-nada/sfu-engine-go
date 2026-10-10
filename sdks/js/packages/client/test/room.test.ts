import { beforeEach, describe, expect, it, vi } from "vitest";
import { Room, RemoteParticipant, signalingURL } from "../src";
import { FakeMediaStream, FakePC, FakeTrack, FakeWebSocket, fakeEnv, tick } from "./fakes";

async function joinedRoom(participants: object[] = []) {
  const room = new Room({ env: fakeEnv(), autoReconnect: false });
  const p = room.connect("https://sfu.example.com", "tok");
  await tick();
  const ws = FakeWebSocket.last;
  ws.push({ type: "joined", room: "standup", participant: "alice", participants });
  await p;
  const [pub, sub] = FakePC.all.slice(-2);
  return { room, ws, pub, sub };
}

beforeEach(() => {
  FakePC.all = [];
});

describe("signalingURL", () => {
  it("turns any server address into the /ws URL", () => {
    expect(signalingURL("https://sfu.example.com")).toBe("wss://sfu.example.com/ws");
    expect(signalingURL("http://localhost:8080/")).toBe("ws://localhost:8080/ws");
    expect(signalingURL("wss://sfu.example.com/ws")).toBe("wss://sfu.example.com/ws");
    expect(signalingURL("sfu.example.com")).toBe("wss://sfu.example.com/ws");
  });
});

describe("Room.connect", () => {
  it("sends the token, resolves on joined and lists the others", async () => {
    const connected = vi.fn();
    const { room, ws } = await joinedRoom([{ id: "bob", muted: { microphone: true } }]);
    room.on("connected", connected);
    expect(ws.url).toBe("wss://sfu.example.com/ws");
    expect(ws.sent[0]).toEqual({ type: "join", token: "tok" });
    expect(room.state).toBe("connected");
    expect(room.name).toBe("standup");
    expect(room.localParticipant.identity).toBe("alice");
    const bob = room.remoteParticipants.get("bob")!;
    expect(bob).toBeInstanceOf(RemoteParticipant);
    expect(bob.isMuted("microphone")).toBe(true);
  });

  it("rejects with the server's reason", async () => {
    const room = new Room({ env: fakeEnv() });
    const disconnected = vi.fn();
    room.on("disconnected", disconnected);
    const p = room.connect("https://sfu", "bad");
    await tick();
    FakeWebSocket.last.push({ type: "error", message: "invalid token" });
    await expect(p).rejects.toThrow("invalid token");
    expect(room.state).toBe("disconnected");
    expect(disconnected).toHaveBeenCalledWith("error", "invalid token");
  });
});

describe("subscribing", () => {
  it("answers server offers and queues early candidates", async () => {
    const { ws, sub } = await joinedRoom();
    ws.push({ type: "candidate", target: "subscriber", candidate: { candidate: "c1" } });
    ws.push({ type: "offer", sdp: "server-offer" });
    await tick();
    await tick();
    expect(sub.remoteDescription).toEqual({ type: "offer", sdp: "server-offer" });
    expect(sub.candidates).toEqual([{ candidate: "c1" }]);
    expect(ws.of("answer")).toEqual([{ type: "answer", sdp: "answer" }]);
  });

  it("reports remote tracks by participant and source", async () => {
    const { room, sub } = await joinedRoom();
    const subscribed = vi.fn();
    const unsubscribed = vi.fn();
    room.on("trackSubscribed", subscribed);
    room.on("trackUnsubscribed", unsubscribed);

    const media = new FakeMediaStream();
    media.id = "bob/media";
    const cam = new FakeTrack("video");
    const mic = new FakeTrack("audio");
    media.addTrack(cam);
    media.addTrack(mic);
    const screen = new FakeMediaStream([new FakeTrack("video")]);
    screen.id = "bob/screen";
    sub.ontrack!({ track: cam, streams: [media] });
    sub.ontrack!({ track: mic, streams: [media] });
    sub.ontrack!({ track: screen.tracks[0], streams: [screen] });

    const bob = room.remoteParticipants.get("bob")!;
    expect([...bob.tracks.keys()].sort()).toEqual(["camera", "microphone", "screen"]);
    expect(bob.mediaStream).toBe(media);
    expect(bob.screenStream).toBe(screen);
    expect(subscribed).toHaveBeenCalledTimes(3);

    media.removeTrack(cam); // the server stopped forwarding bob's camera
    expect(bob.tracks.has("camera")).toBe(false);
    expect(unsubscribed).toHaveBeenCalledTimes(1);
  });

  it("follows participants joining, leaving and muting", async () => {
    const { room, ws } = await joinedRoom();
    const left = vi.fn();
    const mute = vi.fn();
    room.on("participantDisconnected", left);
    room.on("muteChanged", mute);
    ws.push({ type: "participant-joined", participant: "carol" });
    ws.push({ type: "mute", participant: "carol", source: "camera", muted: true });
    await tick();
    expect(room.remoteParticipants.get("carol")!.isMuted("camera")).toBe(true);
    expect(mute).toHaveBeenCalledWith(expect.anything(), "camera", true, false);
    ws.push({ type: "participant-left", participant: "carol" });
    await tick();
    expect(room.remoteParticipants.has("carol")).toBe(false);
    expect(left).toHaveBeenCalledTimes(1);
  });
});

describe("publishing", () => {
  it("publishes camera and microphone with sources, quality and one offer at a time", async () => {
    const { room, ws, pub } = await joinedRoom();
    await room.localParticipant.setCameraEnabled(true);
    await room.localParticipant.setMicrophoneEnabled(true); // queued: first offer unanswered
    expect(ws.of("offer")).toHaveLength(1);
    const cam = pub.transceivers[0];
    expect(cam.init.direction).toBe("sendonly");
    expect(cam.init.sendEncodings).toEqual([{ maxBitrate: 2_500_000 }]);

    ws.push({ type: "answer", sdp: "a1" });
    await tick();
    await tick();
    const offers = ws.of("offer");
    expect(offers).toHaveLength(2);
    expect(Object.values(offers[1].tracks).sort()).toEqual(["camera", "microphone"]);
    expect(room.localParticipant.isCameraEnabled()).toBe(true);
    expect(room.localParticipant.isMicrophoneEnabled()).toBe(true);
  });

  it("mutes the microphone instantly and turns the camera off for real", async () => {
    const { room, ws, pub } = await joinedRoom();
    const lp = room.localParticipant;
    await lp.setMicrophoneEnabled(true);
    await lp.setCameraEnabled(true);
    const mic = lp.tracks.get("microphone")! as unknown as FakeTrack;
    const cam = lp.tracks.get("camera")! as unknown as FakeTrack;

    await lp.setMicrophoneEnabled(false);
    expect(mic.enabled).toBe(false);
    expect(ws.of("mute")).toContainEqual({ type: "mute", source: "microphone", muted: true });

    await lp.setCameraEnabled(false);
    expect(cam.readyState).toBe("ended"); // the camera light goes out
    const camSender = pub.transceivers.find((t) => t.track === cam)!.sender;
    expect(camSender.track).toBeNull();
    expect(lp.isCameraEnabled()).toBe(false);

    await lp.setCameraEnabled(true); // same sender, no new transceiver
    expect(camSender.track).not.toBeNull();
    expect(pub.transceivers.filter((t) => t.track.kind === "video")).toHaveLength(1);
  });

  it("shares the screen as detail and stops by removing the tracks", async () => {
    const { room, ws, pub } = await joinedRoom();
    await room.localParticipant.setScreenShareEnabled(true);
    const screen = pub.transceivers.find((t) => t.track.kind === "video")!;
    expect(screen.track.contentHint).toBe("detail");
    expect(screen.init.sendEncodings).toEqual([{ maxBitrate: 4_000_000 }]);
    expect(Object.values(ws.of("offer")[0].tracks).sort()).toEqual(["screen", "screen-audio"]);

    ws.push({ type: "answer", sdp: "a" });
    await tick();
    await room.localParticipant.setScreenShareEnabled(false);
    expect(pub.removed).toHaveLength(2);
    expect(room.localParticipant.isScreenShareEnabled()).toBe(false);
  });

  it("refuses sources the token does not allow", async () => {
    const { room, ws } = await joinedRoom();
    ws.push({ type: "permissions", grants: { publish: true, subscribe: true, sources: ["microphone"] } });
    await tick();
    await expect(room.localParticipant.setCameraEnabled(true)).rejects.toThrow("not allowed to publish camera");
  });

  it("obeys a server mute", async () => {
    const { room, ws } = await joinedRoom();
    await room.localParticipant.setMicrophoneEnabled(true);
    ws.push({ type: "mute", participant: "alice", source: "microphone", muted: true, by_server: true });
    await tick();
    const mic = room.localParticipant.tracks.get("microphone")!;
    expect(mic.enabled).toBe(false);
    await expect(room.localParticipant.setMicrophoneEnabled(true)).rejects.toThrow("muted by the server");
  });
});

describe("leaving", () => {
  it("disconnect sends leave and stops local tracks", async () => {
    const { room, ws } = await joinedRoom();
    await room.localParticipant.setCameraEnabled(true);
    const cam = room.localParticipant.tracks.get("camera")! as unknown as FakeTrack;
    const disconnected = vi.fn();
    room.on("disconnected", disconnected);
    room.disconnect();
    expect(ws.of("leave")).toHaveLength(1);
    expect(cam.readyState).toBe("ended");
    expect(disconnected).toHaveBeenCalledWith("client", undefined);
  });

  it("reports removal by the server", async () => {
    const { room, ws } = await joinedRoom([{ id: "bob" }]);
    const disconnected = vi.fn();
    room.on("disconnected", disconnected);
    ws.push({ type: "removed", message: "room closed" });
    await tick();
    expect(disconnected).toHaveBeenCalledWith("removed", "room closed");
    expect(room.remoteParticipants.size).toBe(0);
  });

  it("reconnects after a dropped connection and publishes again", async () => {
    vi.useFakeTimers();
    try {
      const room = new Room({ env: fakeEnv() });
      const p = room.connect("https://sfu", "tok");
      await vi.advanceTimersByTimeAsync(0);
      FakeWebSocket.last.push({ type: "joined", room: "r", participant: "alice", participants: [] });
      await p;
      await room.localParticipant.setCameraEnabled(true);

      const events: string[] = [];
      room.on("reconnecting", () => events.push("reconnecting"));
      room.on("reconnected", () => events.push("reconnected"));
      FakeWebSocket.last.drop();
      expect(room.state).toBe("reconnecting");
      await vi.advanceTimersByTimeAsync(1100);
      const ws2 = FakeWebSocket.last;
      expect(ws2.sent[0]).toEqual({ type: "join", token: "tok" });
      ws2.push({ type: "joined", room: "r", participant: "alice", participants: [] });
      await vi.advanceTimersByTimeAsync(0);
      expect(events).toEqual(["reconnecting", "reconnected"]);
      expect(room.state).toBe("connected");
      expect(Object.values(ws2.of("offer")[0].tracks)).toEqual(["camera"]);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("fixes found in review", () => {
  it("offers again after the server rejects an offer", async () => {
    const { room, ws, pub } = await joinedRoom();
    await room.localParticipant.setCameraEnabled(true);
    expect(ws.of("offer")).toHaveLength(1);
    await room.localParticipant.setMicrophoneEnabled(true); // queued behind the first offer
    ws.push({ type: "error", message: "not allowed to publish" }); // rejected: no answer comes
    await tick();
    await tick();
    // The rejected offer was rolled back and the queued change offered.
    const offers = ws.of("offer");
    expect(offers).toHaveLength(2);
    expect(Object.values(offers[1].tracks).sort()).toEqual(["camera", "microphone"]);
    expect(pub.localDescription?.sdp).toBe(offers[1].sdp);
  });

  it("publishes tracks enabled while still connecting, once", async () => {
    const room = new Room({ env: fakeEnv(), autoReconnect: false });
    const p = room.connect("https://sfu", "tok");
    await tick();
    await room.localParticipant.setCameraEnabled(true); // before "joined"
    FakeWebSocket.last.push({ type: "joined", room: "r", participant: "alice", participants: [] });
    await p;
    await tick();
    const pub = FakePC.all.at(-2)!;
    expect(pub.transceivers).toHaveLength(1);
    expect(Object.values(FakeWebSocket.last.of("offer")[0].tracks)).toEqual(["camera"]);
  });

  it("turns the camera back on after a server unmute", async () => {
    const { room, ws } = await joinedRoom();
    await room.localParticipant.setCameraEnabled(true);
    const cam = room.localParticipant.tracks.get("camera")!;
    ws.push({ type: "mute", participant: "alice", source: "camera", muted: true, by_server: true });
    await tick();
    expect(cam.enabled).toBe(false);
    ws.push({ type: "mute", participant: "alice", source: "camera", muted: false, by_server: true });
    await tick();
    expect(cam.enabled).toBe(true);
  });

  it("shares the screen without audio when screen audio is not allowed", async () => {
    const { room, ws } = await joinedRoom();
    ws.push({ type: "permissions", grants: { publish: true, subscribe: true, sources: ["screen"] } });
    await tick();
    await room.localParticipant.setScreenShareEnabled(true);
    expect(Object.values(ws.of("offer")[0].tracks)).toEqual(["screen"]);
  });

  it("keeps retrying a reconnect while the server still holds the old session", async () => {
    vi.useFakeTimers();
    try {
      const room = new Room({ env: fakeEnv() });
      const p = room.connect("https://sfu", "tok");
      await vi.advanceTimersByTimeAsync(0);
      FakeWebSocket.last.push({ type: "joined", room: "r", participant: "alice", participants: [] });
      await p;
      FakeWebSocket.last.drop();
      await vi.advanceTimersByTimeAsync(1100);
      FakeWebSocket.last.push({ type: "error", message: "participant already in room" });
      await vi.advanceTimersByTimeAsync(0);
      expect(room.state).toBe("reconnecting"); // not given up
      await vi.advanceTimersByTimeAsync(2100);
      FakeWebSocket.last.push({ type: "joined", room: "r", participant: "alice", participants: [] });
      await vi.advanceTimersByTimeAsync(0);
      expect(room.state).toBe("connected");
    } finally {
      vi.useRealTimers();
    }
  });

  it("starts fresh after leaving", async () => {
    const { room, ws } = await joinedRoom();
    ws.push({ type: "mute", participant: "alice", source: "microphone", muted: true, by_server: true });
    await tick();
    room.disconnect();
    expect(room.localParticipant.mutedByServer.microphone).toBeUndefined();
    expect(room.localParticipant.tracks.size).toBe(0);
  });
});
