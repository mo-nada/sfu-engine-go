// Your backend gives each user a join token.
//
// DEMO ONLY: this route gives anyone a token for any room and any name, and
// creates rooms on request, which uses your plan's limits. In a real app,
// check who the user is first (your login or session), decide which room they
// may join, and use their real user ID as the identity.
import { AccessToken, RoomServiceClient, SFUError } from "@sfu-engine/server";

const { SFU_URL = "", SFU_API_KEY = "", SFU_API_SECRET = "" } = process.env;
const ID = /^[A-Za-z0-9_-]{1,64}$/;

export async function POST(req: Request) {
  const { room, identity } = (await req.json().catch(() => ({}))) as { room?: string; identity?: string };
  if (!room || !identity || !ID.test(room) || !ID.test(identity)) {
    return Response.json({ error: "room and identity must be 1-64 letters, digits, '-' or '_'" }, { status: 400 });
  }

  // Rooms must exist before anyone joins (unless the tenant has
  // auto_create_rooms): create it if needed.
  const rooms = new RoomServiceClient(SFU_URL, SFU_API_KEY, SFU_API_SECRET);
  try {
    await rooms.createRoom({ name: room, maxParticipants: 20 });
  } catch (err) {
    if (!(err instanceof SFUError && err.code === "conflict")) throw err; // already exists: fine
  }

  const token = await new AccessToken(SFU_API_KEY, SFU_API_SECRET, { identity, ttl: 3600 })
    .addGrant({ room, publish: true, subscribe: true })
    .toJwt();
  return Response.json({ token, serverUrl: SFU_URL });
}
