export const DEFAULT_SERVER = "192.168.3.2:7778";

export const COMMANDS = {
  status: { method: "GET", path: "/status" },
  playpause: { method: "POST", path: "/playpause" },
  next: { method: "POST", path: "/next" },
  prev: { method: "POST", path: "/prev" },
  overlay: { method: "POST", path: "/overlay/toggle" },
  volumeUp: { method: "POST", path: "/volume/up" },
  volumeDown: { method: "POST", path: "/volume/down" },
};

export function baseUrl(server) {
  const value = String(server || "").trim().replace(/\/+$/, "") || DEFAULT_SERVER;
  return value.includes("://") ? value : "http://" + value;
}

export function commandRequest(action, params = {}) {
  if (action === "seek") {
    const offset = Math.trunc(Number(params.offsetMs));
    if (!Number.isFinite(offset)) throw new Error("seek: нет offsetMs");
    return { method: "POST", path: "/seek?offset_ms=" + offset };
  }
  const command = COMMANDS[action];
  if (!command) throw new Error("неизвестная команда: " + action);
  return command;
}

export function parseBody(body) {
  return typeof body === "string" ? JSON.parse(body) : body;
}

export function compactStatus(body) {
  const mpc = body.mpc || {};
  return {
    online: Boolean(mpc.online),
    file: mpc.file || "",
    state: mpc.state || "none",
    positionMs: Number(mpc.position_ms) || 0,
    durationMs: Number(mpc.duration_ms) || 0,
    volume: body.volume ? body.volume.percent : null,
    muted: Boolean(body.volume && body.volume.muted),
  };
}

export function positionAt(status, receivedAt, now) {
  if (status.state !== "playing") return status.positionMs;
  return Math.min(status.positionMs + Math.max(now - receivedAt, 0), status.durationMs);
}

export function formatTime(ms) {
  const total = Math.floor(Math.max(ms, 0) / 1000);
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  const pad = (n) => String(n).padStart(2, "0");
  return h > 0 ? h + ":" + pad(m) + ":" + pad(s) : pad(m) + ":" + pad(s);
}
