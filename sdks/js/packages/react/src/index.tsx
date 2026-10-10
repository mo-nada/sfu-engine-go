import {
  Room,
  type ConnectionState,
  type DisconnectReason,
  type LocalParticipant,
  type Participant,
  type RemoteParticipant,
  type RoomOptions,
} from "@sfu-engine/client";
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
  type CSSProperties,
  type ReactNode,
} from "react";

export { Room } from "@sfu-engine/client";

const RoomContext = createContext<Room | null>(null);

export interface LiveRoomProps {
  /** SFU address, for example "https://sfu.example.com". */
  serverUrl: string;
  /** Join token from your backend. */
  token: string;
  /** Connect now (default true). */
  connect?: boolean;
  /** Turn the microphone / camera on after joining (default true). */
  audio?: boolean;
  video?: boolean;
  options?: RoomOptions;
  onConnected?: (room: Room) => void;
  onDisconnected?: (reason: DisconnectReason, message?: string) => void;
  onError?: (error: Error) => void;
  /** Use an existing Room instead of creating one. */
  room?: Room;
  children?: ReactNode;
}

/**
 * Connects to a room and makes it available to the hooks and components inside.
 *
 * ```tsx
 * <LiveRoom serverUrl={url} token={token}>
 *   <VideoConference />
 * </LiveRoom>
 * ```
 */
export function LiveRoom({
  serverUrl,
  token,
  connect = true,
  audio = true,
  video = true,
  options,
  onConnected,
  onDisconnected,
  onError,
  room: given,
  children,
}: LiveRoomProps) {
  const [room] = useState(() => given ?? new Room(options));
  const callbacks = useRef({ onConnected, onDisconnected, onError });
  callbacks.current = { onConnected, onDisconnected, onError };

  useEffect(() => room.on("disconnected", (r, m) => callbacks.current.onDisconnected?.(r, m)), [room]);

  useEffect(() => {
    if (!connect || !token) return;
    let cancelled = false;
    room
      .connect(serverUrl, token)
      .then(async () => {
        if (cancelled) return;
        callbacks.current.onConnected?.(room);
        const lp = room.localParticipant;
        // Each device on its own: a missing camera must not block the microphone.
        if (audio) await lp.setMicrophoneEnabled(true).catch((e) => callbacks.current.onError?.(e));
        if (video) await lp.setCameraEnabled(true).catch((e) => callbacks.current.onError?.(e));
      })
      .catch((e) => !cancelled && callbacks.current.onError?.(e));
    return () => {
      cancelled = true;
      room.disconnect();
    };
    // audio/video only matter at connect time.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [room, serverUrl, token, connect]);

  return <RoomContext.Provider value={room}>{children}</RoomContext.Provider>;
}

/** The room of the nearest <LiveRoom>. */
export function useRoom(): Room {
  const room = useContext(RoomContext);
  if (!room) throw new Error("useRoom must be used inside <LiveRoom>");
  return room;
}

const ROOM_EVENTS = [
  "connectionStateChanged",
  "participantConnected",
  "participantDisconnected",
  "trackSubscribed",
  "trackUnsubscribed",
  "muteChanged",
  "localTrackPublished",
  "localTrackUnpublished",
  "permissionsChanged",
  "reconnected",
] as const;

/** Re-renders when anything in the room changes; returns a change counter. */
function useRoomVersion(room: Room): number {
  const version = useRef(0);
  const subscribe = useCallback(
    (onChange: () => void) => {
      const offs = ROOM_EVENTS.map((e) =>
        room.on(e, () => {
          version.current++;
          onChange();
        }),
      );
      return () => offs.forEach((off) => off());
    },
    [room],
  );
  return useSyncExternalStore(subscribe, () => version.current, () => version.current);
}

/** The connection state: "disconnected", "connecting", "connected" or "reconnecting". */
export function useConnectionState(): ConnectionState {
  const room = useRoom();
  useRoomVersion(room);
  return room.state;
}

/** Everyone in the room: the local participant first, then the others by identity. */
export function useParticipants(): Participant[] {
  const room = useRoom();
  const v = useRoomVersion(room);
  return useMemo(() => {
    const remote = [...room.remoteParticipants.values()].sort((a, b) => a.identity.localeCompare(b.identity));
    return room.localParticipant.identity ? [room.localParticipant, ...remote] : remote;
    // v: recompute on every room change
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [room, v]);
}

/** The other participants. */
export function useRemoteParticipants(): RemoteParticipant[] {
  const room = useRoom();
  return useParticipants().filter((p): p is RemoteParticipant => p !== room.localParticipant);
}

/** The local participant and its device states. */
export function useLocalParticipant(): {
  participant: LocalParticipant;
  isMicrophoneEnabled: boolean;
  isCameraEnabled: boolean;
  isScreenShareEnabled: boolean;
} {
  const room = useRoom();
  useRoomVersion(room);
  const lp = room.localParticipant;
  return {
    participant: lp,
    isMicrophoneEnabled: lp.isMicrophoneEnabled(),
    isCameraEnabled: lp.isCameraEnabled(),
    isScreenShareEnabled: lp.isScreenShareEnabled(),
  };
}

/** Streams of one participant: camera+microphone and screen. */
export function useParticipantMedia(p: Participant): { media?: MediaStream; screen?: MediaStream; hasVideo: boolean } {
  const room = useRoom();
  useRoomVersion(room);
  if (p === room.localParticipant) {
    const lp = p as LocalParticipant;
    return { media: lp.stream, hasVideo: lp.isCameraEnabled() };
  }
  const rp = p as RemoteParticipant;
  return { media: rp.mediaStream, screen: rp.screenStream, hasVideo: rp.tracks.has("camera") && !rp.isMuted("camera") };
}

// ---------- components ----------

export interface VideoTrackProps {
  stream?: MediaStream;
  /** Mute the element's audio (always true for your own stream, to avoid echo). */
  muted?: boolean;
  /** Mirror the picture (natural for your own camera). */
  mirror?: boolean;
  fit?: "cover" | "contain";
  className?: string;
  style?: CSSProperties;
}

/** A <video> element showing a stream. */
export function VideoTrack({ stream, muted = true, mirror, fit = "cover", className, style }: VideoTrackProps) {
  const ref = useRef<HTMLVideoElement>(null);
  useEffect(() => {
    if (ref.current && ref.current.srcObject !== (stream ?? null)) ref.current.srcObject = stream ?? null;
  }, [stream]);
  return (
    <video
      ref={ref}
      autoPlay
      playsInline
      muted={muted}
      className={className}
      style={{ width: "100%", height: "100%", objectFit: fit, transform: mirror ? "scaleX(-1)" : undefined, ...style }}
    />
  );
}

/** Plays every other participant's audio (camera/microphone and screen audio). Render it once. */
export function RoomAudioRenderer() {
  const room = useRoom();
  useRoomVersion(room);
  const streams: { key: string; stream: MediaStream }[] = [];
  for (const p of room.remoteParticipants.values()) {
    if (p.tracks.has("microphone") && p.mediaStream) streams.push({ key: `${p.identity}-media`, stream: p.mediaStream });
    if (p.tracks.has("screen-audio") && p.screenStream) streams.push({ key: `${p.identity}-screen`, stream: p.screenStream });
  }
  return (
    <div style={{ display: "none" }}>
      {streams.map((s) => (
        <AudioElement key={s.key} stream={s.stream} />
      ))}
    </div>
  );
}

function AudioElement({ stream }: { stream: MediaStream }) {
  const ref = useRef<HTMLAudioElement>(null);
  useEffect(() => {
    if (ref.current) ref.current.srcObject = stream;
  }, [stream]);
  return <audio ref={ref} autoPlay />;
}

const tileStyle: CSSProperties = {
  position: "relative",
  background: "#111827",
  borderRadius: 12,
  overflow: "hidden",
  aspectRatio: "16 / 9",
  color: "#fff",
};

/** One participant: video (or initial), name and microphone state. */
export function ParticipantTile({ participant, className }: { participant: Participant; className?: string }) {
  const room = useRoom();
  const { media, hasVideo } = useParticipantMedia(participant);
  const isLocal = participant === room.localParticipant;
  return (
    <div className={className ?? "sfu-tile"} style={tileStyle} data-identity={participant.identity}>
      {hasVideo ? (
        <VideoTrack stream={media} mirror={isLocal} />
      ) : (
        <div style={{ position: "absolute", inset: 0, display: "grid", placeItems: "center", fontSize: 40 }}>
          {participant.identity.slice(0, 1).toUpperCase()}
        </div>
      )}
      <div style={{ position: "absolute", left: 8, bottom: 6, background: "#0009", padding: "2px 8px", borderRadius: 4, fontSize: 13 }}>
        {participant.identity}
        {isLocal ? " (you)" : ""}
        {participant.isMuted("microphone") ? " 🔇" : ""}
      </div>
    </div>
  );
}

const buttonStyle = (on: boolean): CSSProperties => ({
  padding: "10px 16px",
  borderRadius: 999,
  border: 0,
  cursor: "pointer",
  background: on ? "#2563eb" : "#dc2626",
  color: "#fff",
  fontSize: 14,
});

/** Buttons for microphone, camera, screen share and leave. */
export function ControlBar({ onLeave, className }: { onLeave?: () => void; className?: string }) {
  const room = useRoom();
  const { participant, isMicrophoneEnabled, isCameraEnabled, isScreenShareEnabled } = useLocalParticipant();
  const [error, setError] = useState("");
  const run = (f: () => Promise<void>) => () => {
    setError("");
    f().catch((e: Error) => setError(e.message));
  };
  return (
    <div className={className ?? "sfu-controls"} style={{ display: "flex", gap: 8, justifyContent: "center", flexWrap: "wrap", padding: 12 }}>
      <button style={buttonStyle(isMicrophoneEnabled)} onClick={run(() => participant.setMicrophoneEnabled(!isMicrophoneEnabled))}>
        {isMicrophoneEnabled ? "🎤 Mic on" : "🔇 Mic off"}
      </button>
      <button style={buttonStyle(isCameraEnabled)} onClick={run(() => participant.setCameraEnabled(!isCameraEnabled))}>
        {isCameraEnabled ? "📷 Camera on" : "🚫 Camera off"}
      </button>
      <button
        style={{ ...buttonStyle(true), background: isScreenShareEnabled ? "#2563eb" : "#374151" }}
        onClick={run(() => participant.setScreenShareEnabled(!isScreenShareEnabled))}
      >
        {isScreenShareEnabled ? "🖥️ Stop sharing" : "🖥️ Share screen"}
      </button>
      <button
        style={buttonStyle(false)}
        onClick={() => {
          room.disconnect();
          onLeave?.();
        }}
      >
        Leave
      </button>
      {error && <span style={{ color: "#dc2626", alignSelf: "center" }}>{error}</span>}
    </div>
  );
}

/** A complete call: screen shares, a grid of participants, audio and the control bar. */
export function VideoConference({ onLeave }: { onLeave?: () => void }) {
  const room = useRoom();
  const participants = useParticipants();
  const state = useConnectionState();
  const screens = [...room.remoteParticipants.values()].filter((p) => p.screenStream);
  return (
    <div className="sfu-conference" style={{ display: "flex", flexDirection: "column", gap: 12, height: "100%" }}>
      {state !== "connected" && <div style={{ textAlign: "center", opacity: 0.7 }}>{state}…</div>}
      {screens.map((p) => (
        <div key={p.identity} style={{ ...tileStyle, aspectRatio: "16 / 9" }}>
          <VideoTrack stream={p.screenStream} fit="contain" />
          <div style={{ position: "absolute", left: 8, bottom: 6, background: "#0009", padding: "2px 8px", borderRadius: 4 }}>
            {p.identity} – screen
          </div>
        </div>
      ))}
      <div style={{ display: "grid", gap: 12, gridTemplateColumns: "repeat(auto-fill, minmax(260px, 1fr))" }}>
        {participants.map((p) => (
          <ParticipantTile key={p.identity} participant={p} />
        ))}
      </div>
      <RoomAudioRenderer />
      <ControlBar onLeave={onLeave} />
    </div>
  );
}
