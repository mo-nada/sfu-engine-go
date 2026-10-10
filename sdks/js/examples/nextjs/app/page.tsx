"use client";

import { useState } from "react";
import { LiveRoom, VideoConference } from "@sfu-engine/react";

export default function Page() {
  const [session, setSession] = useState<{ token: string; serverUrl: string } | null>(null);
  const [error, setError] = useState("");

  async function join(form: FormData) {
    setError("");
    const res = await fetch("/api/token", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ room: form.get("room"), identity: form.get("identity") }),
    });
    const data = await res.json().catch(() => ({ error: `server error ${res.status}` }));
    if (!res.ok) return setError(data.error ?? "could not join");
    setSession(data);
  }

  if (!session) {
    return (
      <main style={{ maxWidth: 420, margin: "10vh auto", padding: 16 }}>
        <h1>Join a room</h1>
        <form action={join} style={{ display: "grid", gap: 8 }}>
          <input name="room" defaultValue="standup" required pattern="[A-Za-z0-9_\-]{1,64}" />
          <input name="identity" placeholder="your name" required pattern="[A-Za-z0-9_\-]{1,64}" />
          <button type="submit">Join</button>
        </form>
        {error && <p style={{ color: "#f87171" }}>{error}</p>}
      </main>
    );
  }

  return (
    <main style={{ padding: 16, height: "100vh", boxSizing: "border-box" }}>
      <LiveRoom
        serverUrl={session.serverUrl}
        token={session.token}
        onDisconnected={() => setSession(null)}
        onError={(e) => setError(e.message)}
      >
        <VideoConference onLeave={() => setSession(null)} />
      </LiveRoom>
    </main>
  );
}
