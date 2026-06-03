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

## Env (required; fail-fast)

`OKMICH_QUANT_LIVE_BASE`, `OKMICH_QUANT_LOG_BASE`, `OKMICH_QUANT_ENV_DIR`,
`OKMICH_QUANT_SUPERVISOR_STATE_DIR`.

## Build

```
go mod tidy
go build ./cmd/supervisor                 # native
GOOS=windows GOARCH=amd64 go build ./cmd/supervisor   # the trader-box target
```

Develop the engine/TUI/state logic on any OS — the Windows console control is behind a build
tag (`proc/control_windows.go`), with a no-op stub elsewhere (`proc/stub_other.go`).

> Status: scaffold. Load-bearing pieces are real (control, IPC types, engine loop, TUI shell);
> reconcile/commands/registry wiring carry `TODO`s.
