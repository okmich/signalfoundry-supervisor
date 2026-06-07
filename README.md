# signalfoundry-supervisor

Per-box **process control plane** for the live trading systems — the implementation of
`FLEET_SUPERVISOR_SPEC.md`. A single Go binary with two faces:

```
supervisor engine    # the always-on headless daemon: discover, re-attach, watch (liveness),
                     # alert, and control (graceful stop via a targeted console Ctrl+C)
supervisor tui       # the on-demand Bubble Tea UI: render the fleet, issue start/stop/restart
supervisor ctrlc     # internal helper: borrow a child's console + fire CTRL_C_EVENT, then exit
```

## Boundary

It shares **no code** with the Python trading framework. Its only dependency is the
**logging contract** (`LOGGING_CONTRACT.md` v1.1.0): it reads each system's `status.json`
(runner-root, incl. the `-multi` suffix) and inference JSONL, and stops a system by
delivering a `CTRL_C_EVENT` to its PID (reusing the system's existing Ctrl+C graceful path —
**nothing is added to the trading systems**).

## Engine ↔ TUI IPC (file-based, no listening port)

- **state out:** the engine atomically writes `fleet_state.json` under `STATE_DIR`; the TUI polls it.
- **commands in:** the TUI drops `commands/<id>.json`; the engine polls, executes (it is the
  sole actor on systems), and writes `<id>.result.json`.

## Env

**Required (fail-fast):** `OKMICH_QUANT_LIVE_BASE`, `OKMICH_QUANT_LOG_BASE`, `OKMICH_QUANT_ENV_DIR`,
`OKMICH_QUANT_SUPERVISOR_STATE_DIR`.

**Optional:** `OKMICH_QUANT_PYTHON` (interpreter to spawn `run.py`, default `python`) — for a venv,
point it at the venv's interpreter, e.g. `D:\...\.venv\Scripts\python.exe` (no activation needed; a
venv python uses its own site-packages). The engine resolves it at startup and **warns loudly** if it
is missing (start/restart then fail with a clear `python=...` error until it's fixed);
`OKMICH_QUANT_GLOBAL_CONFIG` (the `.global` dir holding `notifier.env` for Telegram alerts);
`OKMICH_QUANT_STOP_TIMEOUT` / `OKMICH_QUANT_START_TIMEOUT` (default `60s`);
`OKMICH_QUANT_WEDGE_MULTIPLE` / `OKMICH_QUANT_WEDGE_GRACE` (wedge threshold seeds, default `3` / `1m`).

The state dir may also hold `session_health.json` — an optional operator override mapping
`broker_session_id` → `green|red|unknown` for the start gate (§13); a `red` entry refuses starts and
shows the session DOWN in the fleet view (also a manual maintenance lockout).

## Build

```
go mod tidy
go build ./cmd/supervisor                 # native
GOOS=windows GOARCH=amd64 go build ./cmd/supervisor   # the trader-box target
```

Develop the engine/TUI/state logic on any OS — the Windows console control is behind a build
tag (`proc/control_windows.go`), with a no-op stub elsewhere (`proc/stub_other.go`).

## Implementation status

MVP + most of the hardening pass are **implemented and tested** (unit tests across all packages +
Windows integration scripts under `_dev/`; builds on windows/linux/darwin).

**Done:**

- **Discovery / reconcile** — config-driven (single vs multi-trader via `config.json`), state from
  `status.json` + PID liveness + inference freshness, `logical_systems[]` coverage gate.
- **Lifecycle control** — `stop` / `start` / `restart`, single **and** bulk (`*-all`, with a confirm
  gate on fleet-wide stops), via targeted console Ctrl+C; hard-kill fallback → `orphan_suspected`;
  start-failure → `Crashed`.
- **Liveness + alerting** — `wedged` detection with an operator-tunable alert gate (`settings.json`,
  live-read); Telegram alerts from `notifier.env`.
- **Re-attach + registry (§12)** — create-time PID-reuse guard with a persistent identity baseline;
  the engine re-attaches to running children on restart without relaunching.
- **Blast-radius (§7)** — broker-terminal grouping + the ≤10-logical-systems cap.
- **Multi-trader (§16)** — one runner row (`<strategy>-multi`), stopped as a unit, with
  **runner-level liveness**: one wedge clock per logical system, each judged at its own cadence
  (the row wedges if any leg is stale; the fleet view shows the stalest leg's bar-age).
- **Singleton** — a deployment-specific Windows named mutex (state-dir-hashed, OS-freed on exit, no
  stale-pidfile race); the pidfile remains for observability / the non-Windows guard.
- **TUI** — btop-style boxed panels: fleet grouped by terminal, per-row + bulk control, a live
  settings screen, and a per-system **details page** (status panel + live `z_system_log` and
  per-symbol inference tails, the inference tabbed by symbol) with start/stop/restart/kill in place.
  Every destructive action — single/bulk **stop** and **restart**, the operator **force-kill** (`K`),
  and **quitting** the TUI — is behind a y/n confirm, so an errant keystroke can't fire it (start is
  additive, so it isn't gated).
- **Force-kill (`K`)** — an operator escalation that `TerminateProcess`es a live PID immediately,
  bypassing the graceful Ctrl+C path (use when a graceful stop won't take). Because graceful shutdown
  never runs, the system lands in `orphan_suspected` and the broker session must be verified by hand.
- **Broker-session start gate (§13/§14) — scaffold** — the engine refuses to start/restart a system
  whose broker session is `red`, and the fleet view shows per-terminal session health. Resolution is
  pluggable (`internal/session`): a per-broker `Adapter` (the real MT5/IB probe, not yet built) with a
  file-backed operator override (`session_health.json`) as the stand-in + maintenance lockout. With no
  adapter/override, sessions are `unknown` and the gate allows (absence of a probe never blocks).
- **Import / decommission** — operator-driven, engine-independent (`internal/importsys`, TUI `i`):
  validate a source artefact dir, archive any existing copy, install into `LIVE_BASE` via a staged
  atomic rename (the engine re-discovers it read-only next tick); decommission is the inverse.
- **Env passthrough** — the engine spawns each `run.py` inheriting `OKMICH_QUANT_ENV_DIR`, so the
  system resolves its own broker `.env` from that root with no flags. The trading systems' `run.py`
  default `--env-file` now resolves against `OKMICH_QUANT_ENV_DIR` (falling back to the artefact dir
  only when the var is unset), so a Supervisor-spawned (argument-less) launch and a manual launch agree.

**Deferred:**

- Real MT5/IB session **probe** adapters + the stopped-system→session mapping (the `Adapter`
  drop-in); real MT5 end-to-end validation (stand-ins prove the mechanism + engine logic).
