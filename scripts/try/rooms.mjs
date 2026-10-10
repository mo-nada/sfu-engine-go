// Server API actions as the saved test customer.
//   node scripts/try/rooms.mjs list
//   node scripts/try/rooms.mjs get <room>
//   node scripts/try/rooms.mjs mute <room> <name> <camera|microphone|screen> <true|false>
//   node scripts/try/rooms.mjs allow <room> <name> <source,source,...|all>
//   node scripts/try/rooms.mjs kick <room> <name>
//   node scripts/try/rooms.mjs close <room>
import { readFileSync } from "node:fs";
import { RoomServiceClient } from "../../sdks/js/packages/server/dist/index.js";

const c = JSON.parse(readFileSync(new URL(".customer.json", import.meta.url), "utf8"));
const rooms = new RoomServiceClient(c.sfu, c.apiKey, c.apiSecret);
const [cmd, room, name, a, b] = process.argv.slice(2);
const actions = {
  list: () => rooms.listRooms(),
  get: () => rooms.getRoom(room),
  mute: () => rooms.mutePublishedTrack(room, name, a, b !== "false"),
  allow: () => rooms.updateParticipant(room, name, { sources: a === "all" ? [] : a.split(",") }),
  kick: () => rooms.removeParticipant(room, name),
  close: () => rooms.deleteRoom(room),
};
if (!actions[cmd]) {
  console.log("commands: list, get, mute, allow, kick, close (see the top of this file)");
  process.exit(1);
}
console.dir((await actions[cmd]()) ?? "done", { depth: 5 });
