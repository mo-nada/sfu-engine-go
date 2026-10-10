import { renderToString } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { Room } from "@sfu-engine/client";
import { LiveRoom, useParticipants, VideoConference, ParticipantTile, useRoom } from "../src";
import { FakeWebSocket, fakeEnv, tick } from "../../client/test/fakes";

async function joinedRoom() {
  const room = new Room({ env: fakeEnv(), autoReconnect: false });
  const p = room.connect("https://sfu", "tok");
  await tick();
  FakeWebSocket.last.push({
    type: "joined",
    room: "standup",
    participant: "alice",
    participants: [{ id: "carol" }, { id: "bob", muted: { microphone: true } }],
  });
  await p;
  return room;
}

function Names() {
  return <ul>{useParticipants().map((p) => <li key={p.identity}>{p.identity}</li>)}</ul>;
}

describe("React SDK", () => {
  it("lists the local participant first, then the others by name", async () => {
    const room = await joinedRoom();
    const html = renderToString(
      <LiveRoom serverUrl="" token="" connect={false} room={room}>
        <Names />
      </LiveRoom>,
    );
    expect(html).toBe("<ul><li>alice</li><li>bob</li><li>carol</li></ul>");
  });

  it("renders a full conference with tiles and controls", async () => {
    const room = await joinedRoom();
    const html = renderToString(
      <LiveRoom serverUrl="" token="" connect={false} room={room}>
        <VideoConference />
      </LiveRoom>,
    );
    expect(html).toContain('data-identity="alice"');
    expect(html).toContain("alice<!-- --> (you)");
    expect(html).toContain('data-identity="bob"');
    expect(html).toContain("🔇"); // bob's microphone is muted
    for (const label of ["Mic off", "Camera off", "Share screen", "Leave"]) expect(html).toContain(label);
  });

  it("shows the initial when a participant has no video", async () => {
    const room = await joinedRoom();
    function Tile() {
      return <ParticipantTile participant={useRoom().remoteParticipants.get("carol")!} />;
    }
    const html = renderToString(
      <LiveRoom serverUrl="" token="" connect={false} room={room}>
        <Tile />
      </LiveRoom>,
    );
    expect(html).toContain(">C</div>");
    expect(html).not.toContain("<video");
  });

  it("requires <LiveRoom> around the hooks", () => {
    expect(() => renderToString(<Names />)).toThrow("inside <LiveRoom>");
  });
});
