// Typed client for the daemon's local API (/api/v1). The bearer token comes
// from <data_dir>/local_api_token and is stored in localStorage after the
// operator pastes it once (tera dashboard --web prints it).

export interface GpuSummary {
  vendor: string;
  model: string;
  vram_mb: number;
  accel: string;
  unified_memory: boolean;
}

export interface HardwareSummary {
  os: string;
  arch: string;
  cpu_model: string;
  cpu_cores: number;
  ram_mb: number;
  gpus: GpuSummary[];
}

export interface StatsSnapshot {
  tokens_per_sec_1m: number;
  requests_per_min: number;
  total_requests: number;
  total_tokens: number;
  inflight: number;
  earned_microcredits: number;
}

export interface Status {
  node_id: string;
  version: string;
  standalone: boolean;
  state: string;
  uptime_seconds: number;
  default_model: string;
  models_loaded: number;
  inflight: number;
  on_battery: boolean;
  temp_celsius: number;
  hardware?: HardwareSummary;
  stats: StatsSnapshot;
}

export interface Earnings {
  earned_microcredits: number;
  earned_credits: number;
  est_usd: number;
  est_usd_per_day: number;
  lifetime_tokens: number;
  note: string;
}

export interface ModelRow {
  id: string;
  size_bytes: number;
  pinned: boolean;
  last_used: string;
  state: string;
  loaded: boolean;
  default: boolean;
}

export interface Limits {
  serve_policy: string;
  idle_after_seconds: number;
  yield_grace_seconds: number;
  serve_on_battery: boolean;
  max_temp_celsius: number;
  schedule: string[];
}

// Tokens are pasted by hand out of a file or a terminal, so trim on the way
// in and out: a stray leading space is invisible in a password field and
// produced an indistinguishable "invalid token".
export function getToken(): string {
  return (localStorage.getItem("flock_token") ?? "").trim();
}

export function setToken(t: string) {
  localStorage.setItem("flock_token", t.trim());
}

/** Thrown when the daemon answered but rejected the token. */
export class AuthError extends Error {}

async function req<T>(path: string, init?: RequestInit): Promise<T> {
  let res: Response;
  try {
    res = await fetch(path, {
      ...init,
      headers: {
        ...(init?.headers ?? {}),
        Authorization: `Bearer ${getToken()}`,
        "Content-Type": "application/json",
      },
    });
  } catch (e) {
    // Distinguishing this from a 401 is the whole point: "daemon is down"
    // and "token is wrong" need different fixes from the operator.
    throw new Error(`cannot reach the daemon at ${location.host}`);
  }
  if (res.status === 401) {
    throw new AuthError(
      getToken() ? "token rejected by the daemon" : "no token set",
    );
  }
  if (!res.ok) {
    throw new Error(`${res.status} ${(await res.text()).slice(0, 200)}`);
  }
  return res.json() as Promise<T>;
}

/** SSE stream of live daemon events. EventSource cannot set headers, so the
 *  token goes in the query string — the daemon accepts it for this route
 *  only. Pass logs=true to also receive each log line as a `log` event. */
export function eventsURL(logs = false): string {
  return `/api/v1/events?token=${encodeURIComponent(getToken())}${logs ? "&logs=1" : ""}`;
}

export const api = {
  status: () => req<Status>("/api/v1/status"),
  earnings: () => req<Earnings>("/api/v1/earnings"),
  models: () => req<{ models: ModelRow[] }>("/api/v1/models"),
  limits: () => req<Limits>("/api/v1/limits"),
  putLimits: (l: Limits) =>
    req<Limits>("/api/v1/limits", { method: "PUT", body: JSON.stringify(l) }),
  pin: (id: string, pinned: boolean) =>
    req<{ ok: boolean }>(`/api/v1/models/${id}/pin`, {
      method: "POST",
      body: JSON.stringify({ pinned }),
    }),
  removeModel: (id: string) =>
    req<{ ok: boolean }>(`/api/v1/models/${id}`, { method: "DELETE" }),
};

export interface LogEntry {
  time: string;
  level: string;
  message: string;
  attrs?: string;
}

export function fetchLogs(n = 500): Promise<{ logs: LogEntry[] }> {
  return req<{ logs: LogEntry[] }>(`/api/v1/logs?n=${n}`);
}
