// Receives and verifies webhooks on http://localhost:4000/hook and prints them.
// Start it before setup-customer.mjs, and pass that URL to it.
//   node scripts/try/webhook-receiver.mjs
import { createServer } from "node:http";
import { readFileSync } from "node:fs";
import { WebhookReceiver } from "../../sdks/js/packages/server/dist/index.js";

createServer(async (req, res) => {
  let body = "";
  for await (const chunk of req) body += chunk;
  try {
    // Read the secret on each request: setup-customer.mjs may run after this starts.
    const { webhookSecret } = JSON.parse(readFileSync(new URL(".customer.json", import.meta.url), "utf8"));
    const ev = await new WebhookReceiver(webhookSecret).receive(body, req.headers["x-sfu-signature"]);
    console.log(new Date().toLocaleTimeString(), ev.event, ev.room.name, ev.participant?.identity ?? "", ev.track?.source ?? "",
      ev.participant?.duration_seconds ? `${ev.participant.duration_seconds}s` : "");
    res.end("ok");
  } catch (err) {
    console.log("REJECTED:", err.message);
    res.statusCode = 400;
    res.end();
  }
}).listen(4000, () => console.log("webhook receiver on http://localhost:4000/hook"));
