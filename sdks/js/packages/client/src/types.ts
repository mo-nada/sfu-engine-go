/** What a track carries. */
export type Source = "camera" | "microphone" | "screen" | "screen-audio";

export const SOURCES: readonly Source[] = ["camera", "microphone", "screen", "screen-audio"];

/** What a participant may do; set by the join token and changed by the server API. */
export interface Grants {
  publish: boolean;
  subscribe: boolean;
  /** Sources allowed to publish; empty means all. */
  sources?: Source[];
}

export type ConnectionState = "disconnected" | "connecting" | "connected" | "reconnecting";

/** Sending quality for one source. */
export interface SourceQuality {
  /** Maximum bitrate in bits per second. */
  maxBitrate?: number;
  /** What to give up when bandwidth is short. */
  degradationPreference?: RTCDegradationPreference;
}

export interface RoomOptions {
  /** ICE servers for both connections (default: none, host candidates only). */
  iceServers?: RTCIceServer[];
  /** Reconnect automatically when the connection drops (default true). */
  autoReconnect?: boolean;
  /** How many reconnect attempts before giving up (default 5). */
  maxReconnectAttempts?: number;
  /** Camera capture constraints (default 1280x720 at 30 fps). */
  cameraConstraints?: MediaTrackConstraints;
  /** Screen capture constraints (default up to 1920x1080 at 30 fps). */
  screenConstraints?: MediaTrackConstraints;
  /** Sending quality per source (defaults: camera 2.5 Mbps balanced, screen 4 Mbps maintain-resolution). */
  quality?: Partial<Record<Source, SourceQuality>>;
  /** For tests: replacements for browser APIs. */
  env?: Partial<Environment>;
}

/** Browser APIs the SDK uses; tests replace them. */
export interface Environment {
  WebSocket: typeof WebSocket;
  RTCPeerConnection: typeof RTCPeerConnection;
  getUserMedia: (c: MediaStreamConstraints) => Promise<MediaStream>;
  getDisplayMedia: (c: DisplayMediaStreamOptions) => Promise<MediaStream>;
  MediaStream: typeof MediaStream;
}

/** Why the room disconnected. */
export type DisconnectReason =
  | "client" // disconnect() was called
  | "removed" // the server removed this participant or closed the room
  | "error" // the server refused (for example an invalid token or a full room)
  | "connection"; // the connection was lost and could not be restored
