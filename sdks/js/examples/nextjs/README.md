# Next.js example

> **Demo only.** The token route gives anyone a token for any room and any
> name. In your app, check the user's login first: see the comment in
> `app/api/token/route.ts`.

A minimal video call app on the SFU engine:

- `app/api/token/route.ts`: the backend part. It creates the room (if needed)
  and a join token with `@sfu-engine/server`. The API secret never leaves the
  server.
- `app/page.tsx`: the browser part. It asks for a token and shows
  `<VideoConference>` from `@sfu-engine/react`.

## Run

1. Start the SFU with a tenant (see the main README), for example in platform
   mode, and create a tenant with the admin API. Keep its `api_key` and
   `api_secret`.
2. Build the SDKs once, from `sdks/js`:

   ```bash
   npm install
   npm run build
   ```

3. In this folder:

   ```bash
   cp .env.example .env.local    # fill in SFU_URL, SFU_API_KEY, SFU_API_SECRET
   npm install
   npm run dev                   # http://localhost:3000
   ```

4. Open http://localhost:3000 in two tabs and join the same room with
   different names.

The browser must reach the SFU directly, both for WebSocket and for WebRTC
media.
