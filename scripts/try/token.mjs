// Prints a join token for the saved test customer, creating the room if needed.
//   node scripts/try/token.mjs <room> <name>
import { readFileSync } from "node:fs";
import { AccessToken, RoomServiceClient, SFUError } from "../../sdks/js/packages/server/dist/index.js";

const c = JSON.parse(readFileSync(new URL(".customer.json", import.meta.url), "utf8"));
const [room = "standup", identity = "alice"] = process.argv.slice(2);
const rooms = new RoomServiceClient(c.sfu, c.apiKey, c.apiSecret);
await rooms.createRoom({ name: room }).catch((e) => {
  if (!(e instanceof SFUError && e.code === "conflict")) throw e;
});
console.log(await new AccessToken(c.apiKey, c.apiSecret, { identity }).addGrant({ room }).toJwt());
