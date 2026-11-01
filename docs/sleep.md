# Sleep policy — "serve the mesh while you sleep"

The tagline means the **operator** sleeps, not the machine. This page is
the rule for everything flockd does to the operating system's power
management, and the SPEC's "never degrade the operator's machine
perceptibly" (§4.1, §10) is the constraint it is written under. Proposed
as a SPEC addendum; until it lands there, this file is the source of truth.

## The rule

flockd holds exactly one kind of power assertion, **system-required**
(do not let the automatic idle timer suspend the machine), and only
while one of two things is true:

| While | Reason shown to the OS |
|---|---|
| a request is in flight (admitted by the governor, not yet released) | `Teraflock: inference request in flight` |
| the governor is **serving** *and* the node is **enrolled** in a mesh | `Teraflock: serving the mesh` |

Everything else releases it: yielded (operator active), paused on battery
or temperature, outside the schedule window, serving but not enrolled
(`tera login` not done, or `--standalone`), daemon stop, daemon exit,
crash. A request in flight keeps the assertion through a yield for as
long as the request lives — at most `yield_grace` (default 2 s), after
which the governor cancels it — because a request the OS suspends
mid-stream is a failed request the coordinator has to retry.

What flockd **never** does:

- keep the **display** on, or wake it;
- block or delay a sleep the operator asks for (lid, menu, power button,
  `pmset sleepnow`, `systemctl suspend`) — the assertion stops the idle
  timer, nothing else;
- wake the machine (no wake timers, no wake-on-LAN, no scheduled wake);
- hold anything while on battery with `serve_on_battery = false`
  (the governor is paused, so the second row does not apply; a request
  cannot be in flight because the governor refuses admission);
- hold anything from a child process: the runtime (llama-server) is
  started with no power request of its own, and if an upstream build
  takes one that is a bug to report against the runtime pin, not a
  policy change here.

`governor.keep_awake = false` (config.toml) switches the whole mechanism
off; flockd then never touches power management and the machine sleeps
on the operator's schedule, mid-request or not. It is not a live limit:
it takes effect on restart.

## Why "serving and enrolled"

An enrolled node has told the coordinator it is available; placements,
dispatches and fingerprint challenges arrive without warning. If the OS
suspends it between requests the mesh sees a node that heartbeats, then
vanishes, then returns with cold runtimes — the coordinator's view of
capacity is wrong for every minute the node is asleep and the operator's
earnings stop. So "serving" alone is not enough to hold (a standalone or
not-yet-enrolled daemon serving the local API has nobody to disappoint),
and "enrolled" alone is not enough either (a yielded, paused or
off-schedule node is one the mesh is not routing to — let it sleep).

The cost is real: an enrolled idle-only node on mains never idle-sleeps
while the operator is away. That is the product — it is what the
operator opted into by enrolling — and `serve_policy = scheduled` is the
knob for "but only at night".

## Platform mechanism

The governor (`internal/governor/power.go`) owns the assertion: it is
the one component that knows about in-flight requests, serving state and
enrollment, so there is one `syncAwake` and no other code path that
takes or drops the assertion. Platform code implements `SleepInhibitor`
(`Hold(reason)` / `Release()`, both idempotent); the daemon's exit path
calls `Governor.Close`, which releases and refuses later holds.

| OS | Mechanism | Shown in |
|---|---|---|
| macOS | `caffeinate -i -w <flockd pid>` child (IOPMAssertion `PreventUserIdleSystemSleep`); `-w` ends it if flockd dies without releasing | `pmset -g assertions` |
| Windows | `PowerCreateRequest` + `PowerSetRequest(PowerRequestSystemRequired)` (kernel32 via `x/sys/windows`); cleared and closed on release. Not `SetThreadExecutionState`: it is per OS thread, and goroutines move | `powercfg /requests` → SYSTEM: `flockd.exe` with the reason |
| Linux | logind `idle` inhibitor via `systemd-inhibit --what=idle --mode=block` around a `tail --pid=<flockd pid>` child (ends with flockd). `idle` stops the automatic idle action only; `sleep` would also block the operator's own suspend, which the rule forbids | `systemd-inhibit --list` |

Known gaps, by design or platform:

- Linux desktops that only honour their own session inhibitors (not
  logind's) may idle-suspend anyway. GNOME and KDE both consult logind;
  a bare window manager with its own idle daemon may not.
- macOS `caffeinate -i` does not stop lid-close sleep (correct: that is
  the operator asking). With `serve_on_battery = true` the node serves on
  battery and holds the assertion there too — the operator's explicit
  choice.
- Windows Modern Standby laptops are unverified: whether a
  `PowerRequestSystemRequired` request keeps such a machine out of
  standby between requests has not been tested on real hardware.
  `powercfg /requests` is the diagnostic either way.
- No platform keeps the machine awake through a **display-required**
  path, so a Windows node whose display is kept on is holding a request
  flockd did not make. 2026-11-01: Anderson reports exactly that on the
  Windows desktop app; `powercfg /requests` names the holder. If it is
  `llama-server.exe`, the fix is upstream (runtimes pin, which changes
  `runtime_build_id` and needs a fingerprint regeneration decision), not
  here.

## What this does not cover (open)

- **Wake for a placement.** A node that is asleep when the coordinator
  wants it stays asleep; the mesh routes elsewhere. Wake-on-LAN or
  scheduled wake is a deliberate non-goal: waking a machine the operator
  put to sleep is the one thing the "never perceptible" rule rules out.
- **Status surface.** `/api/v1/status` does not yet report whether the
  assertion is held or why (`Governor.AwakeReason` has it); the desktop
  app's power panel is the natural place, with the one-line wording
  "keeping the system awake: serving the mesh".
