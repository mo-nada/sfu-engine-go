// Creates a test customer (tenant) through the admin API and saves its
// credentials in scripts/try/.customer.json (git-ignored).
//   node scripts/try/setup-customer.mjs [webhook-url]
import { writeFileSync } from "node:fs";
import { AdminClient } from "../../sdks/js/packages/server/dist/index.js";

const SFU = process.env.SFU_URL ?? "http://localhost:8080";
const admin = new AdminClient(SFU, process.env.SFU_ADMIN_SECRET ?? "");
const id = "test" + Math.floor(Date.now() / 1000);
const created = await admin.createTenant({
  id,
  name: "Test customer",
  webhookUrl: process.argv[2] ?? "",
  limits: { max_rooms: 10, max_participants: 50, max_room_size: 10 },
});
const saved = { sfu: SFU, tenant: id, apiKey: created.api_key, apiSecret: created.api_secret, webhookSecret: created.tenant.webhook_secret };
writeFileSync(new URL(".customer.json", import.meta.url), JSON.stringify(saved, null, 2));
console.log(`customer "${id}" created; credentials saved in scripts/try/.customer.json`);
console.log(saved);
