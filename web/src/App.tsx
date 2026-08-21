import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { AuthError, api, getToken, setToken, type Limits } from "./api";

type Tab = "status" | "earnings" | "models" | "limits";

export default function App() {
  const [tab, setTab] = useState<Tab>("status");
  const [tokenInput, setTokenInput] = useState(getToken());
  const qc = useQueryClient();

  const status = useQuery({ queryKey: ["status"], queryFn: api.status });

  return (
    <div className="min-h-screen">
      <header className="flex items-baseline gap-4 border-b border-slate-800 px-6 py-4">
        <h1 className="text-lg font-bold tracking-widest text-amber-400">
          <svg viewBox="0 0 48 48" width="20" height="20" aria-hidden="true" className="inline align-[-4px] mr-1">
            <path d="M6 40 L24 40 L31 32 L13 32 Z" fill="#566079" />
            <path d="M11 28 L29 28 L36 20 L18 20 Z" fill="#8b95a9" />
            <path d="M16 16 L34 16 L41 8 L23 8 Z" fill="#f5b60d" />
          </svg>
          TERAFLOCK
        </h1>
        <span className="text-xs text-slate-500">node dashboard</span>
        {status.data && (
          <span
            className={
              "ml-auto text-sm " +
              (status.data.state === "serving"
                ? "text-green-400"
                : "text-amber-400")
            }
          >
            {status.data.state.toUpperCase()}
            {status.data.standalone && (
              <span className="text-slate-500"> · standalone</span>
            )}
          </span>
        )}
      </header>

      <div className="mx-auto max-w-5xl p-6">
        <div className="mb-4 flex gap-2">
          <input
            type="password"
            value={tokenInput}
            onChange={(e) => setTokenInput(e.target.value)}
            placeholder="bearer token from <data_dir>/local_api_token"
            className="flex-1 rounded border border-slate-800 bg-slate-900 px-3 py-2 text-sm"
          />
          <button
            className="rounded bg-amber-400 px-4 py-2 text-sm font-semibold text-slate-950 disabled:opacity-40"
            disabled={!tokenInput.trim()}
            onClick={() => {
              const t = tokenInput.trim();
              setTokenInput(t);
              setToken(t);
              // resetQueries, not invalidateQueries: a query parked in an
              // error state should visibly retry from scratch, so the
              // operator can tell the click did something.
              qc.resetQueries();
            }}
          >
            connect
          </button>
        </div>

        {status.isSuccess && (
          <p className="mb-4 text-sm text-green-400">
            connected to {status.data.node_id}
          </p>
        )}

        <nav className="mb-6 flex gap-1">
          {(["status", "earnings", "models", "limits"] as Tab[]).map((t) => (
            <button
              key={t}
              onClick={() => setTab(t)}
              className={
                "rounded px-4 py-1.5 text-sm capitalize " +
                (tab === t
                  ? "bg-slate-800 text-amber-300"
                  : "text-slate-400 hover:text-slate-200")
              }
            >
              {t}
            </button>
          ))}
        </nav>

        {status.isError && (
          <p className="mb-4 text-sm text-red-400">
            {status.error instanceof AuthError ? (
              <>
                {status.error.message} — paste the token from{" "}
                <code className="text-slate-300">
                  &lt;data_dir&gt;/local_api_token
                </code>{" "}
                (or run <code className="text-slate-300">flock token</code>) and
                press connect. Whitespace is trimmed for you.
              </>
            ) : (
              <>{String(status.error?.message ?? status.error)} — is flockd
                running? Try <code className="text-slate-300">flock up</code> or{" "}
                <code className="text-slate-300">flockd --standalone</code>.</>
            )}
          </p>
        )}

        {tab === "status" && <StatusPage />}
        {tab === "earnings" && <EarningsPage />}
        {tab === "models" && <ModelsPage />}
        {tab === "limits" && <LimitsPage />}
      </div>
    </div>
  );
}

function Card(props: { title: string; children: React.ReactNode }) {
  return (
    <div className="rounded-lg border border-slate-800 bg-slate-900 p-4">
      <h2 className="mb-2 text-xs uppercase tracking-widest text-slate-500">
        {props.title}
      </h2>
      {props.children}
    </div>
  );
}

function StatusPage() {
  const { data: st } = useQuery({ queryKey: ["status"], queryFn: api.status });
  if (!st) return <p className="text-slate-500">loading…</p>;
  return (
    <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
      <Card title="Node">
        <div className="text-2xl font-semibold">{st.node_id.slice(0, 12)}</div>
        <p className="text-sm text-slate-400">
          v{st.version} · up {Math.floor(st.uptime_seconds / 60)}m · model{" "}
          {st.default_model}
        </p>
      </Card>
      <Card title="Throughput">
        <div className="text-2xl font-semibold text-green-400">
          {st.stats.tokens_per_sec_1m.toFixed(1)}{" "}
          <span className="text-sm text-slate-400">tok/s (1m)</span>
        </div>
        <p className="text-sm text-slate-400">
          in-flight {st.inflight} · total {st.stats.total_requests} reqs ·{" "}
          {st.stats.total_tokens} tokens
        </p>
      </Card>
      {st.hardware && (
        <Card title="Hardware">
          <p className="text-sm">
            {st.hardware.os}/{st.hardware.arch} · {st.hardware.cpu_model} ×{" "}
            {st.hardware.cpu_cores} · {Math.round(st.hardware.ram_mb / 1024)}GB
          </p>
          {st.hardware.gpus.map((g) => (
            <p key={g.model} className="text-sm text-slate-400">
              {g.model} · {g.accel} · {Math.round(g.vram_mb / 1024)}GB
              {g.unified_memory ? " unified" : ""}
            </p>
          ))}
        </Card>
      )}
      <Card title="Power">
        <p className="text-sm">
          {st.on_battery ? "🔋 on battery (serving paused by default)" : "⚡ AC power"}
          {st.temp_celsius > 0 ? ` · ${st.temp_celsius.toFixed(0)}°C` : ""}
        </p>
      </Card>
    </div>
  );
}

function EarningsPage() {
  const { data: e } = useQuery({ queryKey: ["earnings"], queryFn: api.earnings });
  if (!e) return <p className="text-slate-500">loading…</p>;
  return (
    <div className="grid grid-cols-1 gap-4 md:grid-cols-3">
      <Card title="Earned">
        <div className="text-2xl font-semibold text-amber-300">
          ${e.est_usd.toFixed(6)}
        </div>
        <p className="text-sm text-slate-400">{e.earned_credits.toFixed(4)} credits</p>
      </Card>
      <Card title="Est / day">
        <div className="text-2xl font-semibold">${e.est_usd_per_day.toFixed(4)}</div>
      </Card>
      <Card title="Lifetime tokens">
        <div className="text-2xl font-semibold">{e.lifetime_tokens}</div>
      </Card>
      <p className="col-span-full text-xs text-slate-500">{e.note}</p>
    </div>
  );
}

function ModelsPage() {
  const qc = useQueryClient();
  const { data } = useQuery({ queryKey: ["models"], queryFn: api.models });
  if (!data) return <p className="text-slate-500">loading…</p>;
  return (
    <Card title="Local models">
      <table className="w-full text-sm">
        <thead>
          <tr className="text-left text-slate-500">
            <th className="py-1">model</th>
            <th>state</th>
            <th>size</th>
            <th>pinned</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {data.models.map((m) => (
            <tr key={m.id} className="border-t border-slate-800">
              <td className="py-1.5">
                {m.loaded ? "● " : "○ "}
                {m.id}
                {m.default && <span className="text-slate-500"> (default)</span>}
              </td>
              <td>{m.state}</td>
              <td>{m.size_bytes ? `${(m.size_bytes / 1e9).toFixed(1)}GB` : "—"}</td>
              <td>
                <button
                  className="text-amber-300"
                  onClick={() =>
                    api.pin(m.id, !m.pinned).then(() =>
                      qc.invalidateQueries({ queryKey: ["models"] }),
                    )
                  }
                >
                  {m.pinned ? "📌 unpin" : "pin"}
                </button>
              </td>
              <td>
                <button
                  className="text-red-400"
                  onClick={() =>
                    api.removeModel(m.id).then(() =>
                      qc.invalidateQueries({ queryKey: ["models"] }),
                    )
                  }
                >
                  remove
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </Card>
  );
}

function LimitsPage() {
  const qc = useQueryClient();
  const { data } = useQuery({ queryKey: ["limits"], queryFn: api.limits });
  const [draft, setDraft] = useState<Limits | null>(null);
  if (!data) return <p className="text-slate-500">loading…</p>;
  const lim = draft ?? data;
  return (
    <Card title="Resource limits">
      <div className="flex flex-col gap-3 text-sm">
        <label className="flex items-center justify-between gap-4">
          serve policy
          <select
            value={lim.serve_policy}
            onChange={(e) => setDraft({ ...lim, serve_policy: e.target.value })}
            className="rounded border border-slate-800 bg-slate-950 px-2 py-1"
          >
            <option value="idle-only">idle-only</option>
            <option value="always">always</option>
            <option value="scheduled">scheduled</option>
          </select>
        </label>
        <label className="flex items-center justify-between gap-4">
          serve on battery
          <input
            type="checkbox"
            checked={lim.serve_on_battery}
            onChange={(e) =>
              setDraft({ ...lim, serve_on_battery: e.target.checked })
            }
          />
        </label>
        <label className="flex items-center justify-between gap-4">
          max temp (°C)
          <input
            type="number"
            value={lim.max_temp_celsius}
            onChange={(e) =>
              setDraft({ ...lim, max_temp_celsius: Number(e.target.value) })
            }
            className="w-24 rounded border border-slate-800 bg-slate-950 px-2 py-1"
          />
        </label>
        <label className="flex items-center justify-between gap-4">
          schedule (comma-sep, HH:MM-HH:MM)
          <input
            type="text"
            value={lim.schedule.join(",")}
            onChange={(e) =>
              setDraft({
                ...lim,
                schedule: e.target.value.split(",").filter(Boolean),
              })
            }
            className="w-56 rounded border border-slate-800 bg-slate-950 px-2 py-1"
          />
        </label>
        <button
          className="self-start rounded bg-amber-400 px-4 py-1.5 font-semibold text-slate-950 disabled:opacity-40"
          disabled={!draft}
          onClick={() =>
            draft &&
            api.putLimits(draft).then(() => {
              setDraft(null);
              qc.invalidateQueries({ queryKey: ["limits"] });
            })
          }
        >
          apply
        </button>
        <p className="text-xs text-slate-500">
          yield grace {lim.yield_grace_seconds}s · idle threshold{" "}
          {lim.idle_after_seconds}s · changes apply live, reset on daemon
          restart (persistence TODO)
        </p>
      </div>
    </Card>
  );
}
