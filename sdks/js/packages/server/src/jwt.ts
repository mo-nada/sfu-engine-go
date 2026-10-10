// HS256 JWT signing and HMAC helpers on the Web Crypto API, so the SDK runs
// on Node 18+, edge runtimes (Next.js edge, Cloudflare Workers), Deno and Bun.

const enc = new TextEncoder();

export function base64url(bytes: Uint8Array): string {
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

async function hmacKey(secret: string): Promise<CryptoKey> {
  return crypto.subtle.importKey("raw", enc.encode(secret), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
}

export async function hmacSHA256(secret: string, data: string): Promise<Uint8Array> {
  const sig = await crypto.subtle.sign("HMAC", await hmacKey(secret), enc.encode(data));
  return new Uint8Array(sig);
}

export function toHex(bytes: Uint8Array): string {
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

/** Signs claims as an HS256 JWT. */
export async function signJWT(secret: string, claims: object): Promise<string> {
  const header = base64url(enc.encode(JSON.stringify({ alg: "HS256", typ: "JWT" })));
  const payload = base64url(enc.encode(JSON.stringify(claims)));
  const signing = `${header}.${payload}`;
  return `${signing}.${base64url(await hmacSHA256(secret, signing))}`;
}

/** Compares two strings in time independent of where they differ. */
export function timingSafeEqual(a: string, b: string): boolean {
  if (a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return diff === 0;
}
