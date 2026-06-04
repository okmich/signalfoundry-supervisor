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

**Optional:** `OKMICH_QUANT_PYTHON` (interpreter to spawn `run.py`, default `python`);
`OKMICH_QUANT_GLOBAL_CONFIG` (the `.global` dir holding `notifier.env` for Telegram alerts);
`OKMICH_QUANT_STOP_TIMEOUT` / `OKMICH_QUANT_START_TIMEOUT` (default `60s`);
`OKMICH_QUANT_WEDGE_MULTIPLE` / `OKMICH_QUANT_WEDGE_GRACE` (wedge threshold seeds, default `3` / `1m`).

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
- **TUI** — color-coded fleet grouped by terminal, per-row + bulk control, confirm gate, a live
  settings screen.

**Deferred:**

- Real MT5 end-to-end validation (stand-ins prove the mechanism + engine logic).
- Broker-session start gate (§13/§14) — MVP-deferred.
