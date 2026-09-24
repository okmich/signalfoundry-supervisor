# Fleet Supervisor Specification

> Status: design spec — interface-precise. Settled 2026-06-01; control mechanism revised 2026-06-02 (targeted Ctrl+C via AttachConsole, §9); bulk operations specified 2026-06-03 (§11.1).
> **Largely implemented** in this repo (`signalfoundry-supervisor`): MVP + re-attach (§12), multi-trader with runner-level liveness (§15/§16), blast-radius (§7), a named-mutex singleton, and the broker-session start-gate scaffold (§13/§14 — gate + override + UI; real MT5/IB probe pending); see `../README.md` for the implementation status. It also adds an operator-driven import/decommission utility (TUI `i` to import, `D` to decommission) that provisions artefacts into `LIVE_BASE` via a staged atomic rename — both fully wired into the TUI (dialog + confirm) and covered by regression tests. **Under live MT5 testing as of 2026-06-07** — single + multi-trader start/stop/restart, import, and decommission exercised via the TUI on a Deriv-Demo terminal, and it is looking good.
> **Account layout (2026-09-25).** `<live_base>` and `<log_base>` are laid out by account, `<base>\<account>\...`, the account being the broker env-file stem (`fxify.demo`). The account folder is the single source of truth for which broker a system trades: the Supervisor passes it to each system it starts as `OKMICH_QUANT_ACCOUNT`, groups the fleet by it, checks each running system against it, and refuses a start whose account env file is missing or incomplete (§7, §13, §16). The Account Admin is an ordinary runner in its account folder whose governance is never undone as a side effect (§3, §16). See [ACCOUNT_LAYOUT_CHANGE_PLAN.md](../../signalfoundry-lab/docs/ops/ACCOUNT_LAYOUT_CHANGE_PLAN.md).
> Companion contracts (`LOGGING_CONTRACT.md`, `OPS_REFERENCE_GUIDE.md`) live in the sibling `signalfoundry-lab` repo.
> Scope: the per-box **Supervisor** (a process control plane for live trading systems) and a high-level pointer to the future cross-box **Fleet Manager**.
> Companion to [OPS_REFERENCE_GUIDE.md](../../signalfoundry-lab/docs/ops/OPS_REFERENCE_GUIDE.md): this spec *consumes* that guide's folder layouts, environment variables, log conventions, and `<account>/<strategy>/<symbol>/<timeframe>` path shape. The two are complementary — the OPS guide defines the deployment substrate; this spec defines the process that starts, stops, and watches the things deployed onto it.

---

## Table of contents

1. [Purpose and scope](#1-purpose-and-scope)
2. [Relationship to the OPS guide](#2-relationship-to-the-ops-guide)
3. [Principles](#3-principles)
4. [Organizational boundary — ops admin vs trade analyst](#4-organizational-boundary--ops-admin-vs-trade-analyst)
5. [Vocabulary: Supervisor vs Fleet Manager](#5-vocabulary-supervisor-vs-fleet-manager)
6. [Process model](#6-process-model)
7. [Terminal / account topology and blast radius](#7-terminal--account-topology-and-blast-radius)
8. [Lifecycle state machine](#8-lifecycle-state-machine)
9. [Control — targeted console Ctrl+C](#9-control--targeted-console-ctrlc)
10. [Graceful-shutdown contract (system-owned)](#10-graceful-shutdown-contract-system-owned)
11. [Stop / restart verification and sequences](#11-stop--restart-verification-and-sequences)
12. [Supervisor restart and child re-attach](#12-supervisor-restart-and-child-re-attach)
13. [Startup — operator-initiated; auto-start deferred](#13-startup--operator-initiated-auto-start-deferred)
14. [Broker-session adapters](#14-broker-session-adapters)
15. [Live monitoring](#15-live-monitoring)
16. [Folder and IPC layout](#16-folder-and-ipc-layout)
17. [Office telemetry — Fleet Manager (future)](#17-office-telemetry--fleet-manager-future)
18. [Open items](#18-open-items)
19. [Glossary](#19-glossary)

---

## 1. Purpose and scope

The Supervisor is a **single pane of glass for the live trading box**. It lets an operator discover every configured trading system, start them (all at once or individually), stop or restart them, and watch their logs — without opening one terminal per system. With 10 systems on a box, 20 across two, the "one terminal each" model does not scale; the Supervisor replaces it.

**In scope (the Supervisor, built first):**

- Discovery of trading systems from the on-disk config tree.
- Lifecycle control: start, graceful stop, restart, hard-kill fallback.
- Process and log observation: liveness, last-event, raw stdout capture.
- A local UI client (TUI or local web dashboard) over the Supervisor's state.
- A broker-session **precondition check** gating start (a check, not bring-up).

**Out of scope (deferred or never):**

- **Trade management of any kind** — never (see [§3](#3-principles), [§4](#4-organizational-boundary--ops-admin-vs-trade-analyst)).
- **Automatic application startup at boot** — deferred (see [§13](#13-startup--operator-initiated-auto-start-deferred)).
- **Bringing up broker sessions** (launching/authing MT5 terminals or IB Gateway) — deferred; human-initiated (see [§13](#13-startup--operator-initiated-auto-start-deferred), [§14](#14-broker-session-adapters)).
- **Cross-box aggregation and office reporting** — that is the Fleet Manager, a later evolution (see [§5](#5-vocabulary-supervisor-vs-fleet-manager), [§17](#17-office-telemetry--fleet-manager-future)).
- **Inbound remote control over WAN** — never, until a real auth model and threat model exist (see [§17](#17-office-telemetry--fleet-manager-future)).

## 2. Relationship to the OPS guide

This spec does not redefine the deployment substrate; it reuses it. From [OPS_REFERENCE_GUIDE.md](../../signalfoundry-lab/docs/ops/OPS_REFERENCE_GUIDE.md):

| Borrowed from the OPS guide | Used here for |
|---|---|
| `<account>/` folders, each holding `<strategy>/<symbol>/<timeframe>/`, `<strategy>-multi/` and runner folders | Identifying, grouping and discovering systems ([§16](#16-folder-and-ipc-layout)) |
| `OKMICH_QUANT_LIVE_BASE` (`<live_base>`) | Where the artefact + `run.py` per system live (trader box), one folder per account |
| `OKMICH_QUANT_LOG_BASE` (`<log_base>`) | Where each system's inference JSONL is read from; deployment examples such as `D:\quant_logs` live in the OPS guide, not in source defaults |
| `OKMICH_QUANT_ENV_DIR` (`<env_dir>`) | Where machine-local broker/session `.env.<account>` files live; the list of accounts on the box; never copied into artefacts |
| `OKMICH_QUANT_ACCOUNT` | Set by the Supervisor for each process it starts, from the system's account folder; never a machine-level variable ([§16](#16-folder-and-ipc-layout)) |
| `config.json` per (strategy, symbol, timeframe) | Source of `signal_params`, broker bindings — read-only to the Supervisor |
| JSONL inference log (`JsonlEventLogger`) | The liveness + behavior stream (per-bar heartbeat + breaker events) |
| Runner status file (`status.json` / `RunnerStatus`) | Runner lifecycle state + the clean-disconnect (shutdown) proof |
| `TelegramNotifier` | Alert transport for hard-kill / orphan-flag / crash-loop events |

The OPS guide gains a short pointer to this spec in its Operational Capabilities section. Neither document repeats the other.

## 3. Principles

The design derives from six invariants. Every contract below traces to one.

| Principle | Implication |
|---|---|
| **Control plane, never execution path** | The Supervisor spawns `python run.py`, watches it, relays. It never connects to a broker, never places/cancels an order, never reads or writes a position. Supervisor failure cannot, by construction, move the market. |
| **Supervisor lifecycle ≠ trade lifecycle** | Stopping/restarting/crashing the Supervisor (e.g. for an OS patch) must leave every running trading system trading, untouched. A fleet stop is *only* the explicit per-system stop commands an operator issues — never a side effect of the Supervisor's own exit. This is the *deliberate-admin-shutdown* case, stronger than mere crash-independence. |
| **Ops never manages trades** | "Stop a system" means *stop the process, cleanly, leaving the book exactly as it is.* There is no flatten/close/position capability anywhere on this plane — the safety is **structural, not a safe default**. |
| **Separation is organizational, not enforced** | Ops admin and trade analyst are distinct roles that ideally communicate, but the system does not enforce that handshake. The Supervisor makes no trade decision and *surfaces* what an analyst would need to see — it does not block on, or arbitrate, the human coordination. |
| **One system, one OS process** | Each trading system is one top-level process — one PID, one Task Manager entry. The Supervisor's killable/signalable unit is the OS process. |
| **Governance is never undone as a side effect** | Once an Account Admin governs an account, no ops action of the Supervisor removes or resets that governance in passing: importing new Admin code keeps its governance files, and decommissioning the Admin is refused. Ending governance is a deliberate runbook step ([ACCOUNT_ADMIN_SPEC §8.3](../../signalfoundry-lab/docs/ops/ACCOUNT_ADMIN_SPEC.md), [§16](#16-folder-and-ipc-layout)). |

## 4. Organizational boundary — ops admin vs trade analyst

The Supervisor encodes an org chart, not just a tool. Two roles:

- **Ops admin** — operates the box. Receives operational instructions ("shut down systems A, B, C for maintenance"), and executes them through the Supervisor by stopping the listed systems. The ops admin **does not decide what happens to the open positions** of a stopped system — that is not theirs to figure out.
- **Trade analyst** — owns positions, risk, and trade management. If a stopped system has open exposure, the analyst decides what to do with it, using their own tools (not the Supervisor).

The Supervisor enforces this by **omission**: it has no code path that touches a position, and it makes **no judgment about exposure**. Whether a stopped system's positions are protected — broker-resident stop orders vs in-loop protection — is a property of the trade, decided at trade-design time and owned by the analyst. It is **not the Supervisor's concern and not an input to any Supervisor state**: the Supervisor does not store it, display it, or color a system by it. (Coloring unprotected exposure as a warning would itself be a trade-risk judgment the Supervisor must not make.) The Supervisor never waits for, nor verifies, analyst action. Ideally ops tells the analyst; the system does not require it.

> **The one adjacent concern that *is* ops business: connection hygiene.** A hard-kill may orphan a broker *connection* — a half-open socket, a stuck IB `clientId` — which is a process/connection failure, not a trade decision. The Supervisor flags that (see [§11](#11-stop--restart-verification-and-sequences)). Orphaned **connection** = ops; open or unprotected **position** = trade. The line is sharp, and the Supervisor sits entirely on the connection side of it.

> **The account check is identity, not a trade judgment.** The Supervisor checks that each running system is logged into the account its folder claims: the `account` in its `status.json` against the folder, and the terminal's login against the account env file's `LOGIN_ID` ([§7](#7-terminal--account-topology-and-blast-radius)). A mismatch is flagged and alerted. That is the same kind of fact as "is this PID the process we started" — it says nothing about positions, exposure or whether the trades are right, and the Supervisor acts on it only by telling the operator.

## 5. Vocabulary: Supervisor vs Fleet Manager

| Term | Scope | When | This spec |
|---|---|---|---|
| **Supervisor** | Per-box process control plane: discover / start / stop / restart / watch the systems on **one** machine, with a local UI client. | Now | §§6–16 |
| **Fleet Manager** | Cross-box office layer: aggregates many Supervisors, receives their outbound telemetry, presents fleet-wide state at the office. | Later | §17 (pointer only) |

"Control plane" is the architectural role both share (control plane vs execution plane). When unqualified in this document, **Supervisor** is meant.

## 6. Process model

- **One OS process per trading system.** One PID, one Task Manager entry. Not a host-spawning-many-children model.
- **`trader`** — one process, one symbol, one config.
- **`multi-trader`** — one process managing N symbols under one strategy, each with its own config. Still **one** OS process / one PID. Internally it runs N symbols; externally it is a single killable/signalable unit. This distinction matters for blast radius ([§7](#7-terminal--account-topology-and-blast-radius)): one PID can carry several *logical systems*.
- **Spawn flags (critical).** Each child is spawned with its **own console** (`CREATE_NEW_CONSOLE`) — not detached — so the Supervisor can deliver a targeted `CTRL_C_EVENT` to that one child by *attaching to its console* ([§9](#9-control--targeted-console-ctrlc)) without disturbing siblings or itself. Children are **NOT** placed inside a Windows **Job Object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`**: that common "clean up my children on exit" pattern would silently terminate every trading system when the Supervisor exits — which directly violates *Supervisor lifecycle ≠ trade lifecycle* ([§3](#3-principles)). The spec states this negative explicitly because it is the default trap.
- **Consequence — control is by the OS process, not a held handle.** Because children must outlive the Supervisor, control cannot rely on an in-memory process handle or an open socket. The Supervisor stops a system by its **PID** — re-derived from `process_registry.json` ([§16](#16-folder-and-ipc-layout)) on restart and verified against the live `status.json` start token — which is exactly what makes Supervisor re-attach ([§12](#12-supervisor-restart-and-child-re-attach)) cheap. **Nothing is added to the trading system** ([§9](#9-control--targeted-console-ctrlc)).

## 7. Terminal / account topology and blast radius

The **broker terminal/session is the unit of correlated failure**, not the individual system. One MT5 terminal = one broker login = one account: all systems on it **share margin, share the magic-number namespace, and die together if the terminal crashes.**

- **Cap: ≤ 10 *logical systems* per MT5 terminal / account, where one logical system is one OS process.** `mt5.initialize()` binds **one terminal IPC slot per process**, whatever that process's symbol count, so a `multi-trader` trading 5 symbols is **1 logical system on 1 slot**, not 5. The cap rations the terminal's connection slots and the blast radius of one crash, not an arbitrary number.
- **Symbols are *legs*, and legs are not capped.** A terminal's leg count (Σ symbols across its processes) is the **account-concentration** figure: shared margin and the magic-number namespace scale with legs, not with slots. The fleet view **reports** legs beside the cap and never refuses on them. Counting legs against the ≤10 was a pre-`multi-trader` conflation — back then 1 system = 1 PID = 1 slot = 1 symbol, and the four numbers were indistinguishable.
- The Supervisor tracks **two numbers** and must not conflate them:
  - **Logical systems (PIDs)** — the unit it can start / signal / kill, one terminal IPC slot each; the unit of blast radius and the ≤10 cap.
  - **Legs (symbols)** — the exposure/concentration unit; reported, never capped.
- The fleet view must answer: *"if terminal X dies, which systems go dark?"* — i.e. it models `systems → terminal (broker session) → account`.
- **The grouping is declared, not observed.** Each account folder `<live_base>\<account>\` names one broker env file, `.env.<account>`, which names one terminal (`TERMINAL_PATH`) and one login (`LOGIN_ID`). So the Supervisor groups the fleet by account folder: a group exists, with its stopped systems, even when nothing in it runs, and a system cannot drift into another account's group by where it happened to log in.
- The cap and the leg count cover the account's **live** processes (running or in flight), the ones that hold terminal slots. The cap is **reported, never enforced**: an account over 10 is shown `OVER CAP`, and no start is refused for it.
- **Account check.** Each running system is checked against its folder: the `account` its `status.json` was written under must be the folder, and the login its terminal reports (`account_id`) must be the env file's `LOGIN_ID`. A mismatch is shown on the row and alerted once per episode ([§4](#4-organizational-boundary--ops-admin-vs-trade-analyst)). A system started by hand under a different account logs under that account's folder, so its own row reads stopped rather than mismatched; the login check still catches a terminal on the wrong account.

## 8. Lifecycle state machine

Per-system state, owned by the Supervisor:

| State | Meaning | Auto-restart? |
|---|---|---|
| `Stopped` | Known to config, not running | No (operator starts it) |
| `Starting` | Spawn issued, not yet confirmed live | n/a |
| `Running` | Process alive and emitting JSONL | n/a |
| `Stopping` | Graceful stop (targeted Ctrl+C) delivered, awaiting clean exit | No |
| `StoppedByOperator` | Exited cleanly after an operator stop | **No** — deliberate, do not resurrect |
| `Crashed` | Exited non-zero / died without an operator stop | Yes — with backoff (when restart policy is enabled) |
| `Restarting` | Graceful-stop-then-relaunch in flight | n/a |
| `CrashLoopHalted` | Exceeded N crashes in M minutes | **No** — halt, alert, stop touching the market |

Key transitions:

- `Running → Stopping → StoppedByOperator` on an operator stop (clean exit within T). `StoppedByOperator` is terminal until an operator restarts — never auto-restarted, because the stop was deliberate.
- `Running → Crashed` on unexpected exit. `Crashed → Starting` only if restart policy is enabled, throttled by backoff.
- `Crashed → CrashLoopHalted` when restarts exceed the crash-loop threshold; from here only an operator clears it. This prevents a bad artefact or a broker outage from hammer-restarting against the market.
- Any `Stopping` that does not reach clean exit within **T = 60s** ([§11](#11-stop--restart-verification-and-sequences)) → **hard-kill**, mark `orphan_suspected`, alert.

> **Restart policy note.** Auto-restart on `Crashed` is listed for completeness but its full design (including the *adopt-don't-relaunch* concern) is **deferred alongside auto-startup** ([§13](#13-startup--operator-initiated-auto-start-deferred)). The MVP may treat `Crashed` as a terminal-until-operator state too. What is *not* deferred is `CrashLoopHalted` as a backstop the moment any auto-restart exists.

## 9. Control — targeted console Ctrl+C

The Supervisor stops a system by delivering a **targeted `CTRL_C_EVENT` to that one child process** — reusing the system's existing `SIGINT`/`KeyboardInterrupt` graceful path ([§10](#10-graceful-shutdown-contract-system-owned)). **Nothing is added to the trading system**: it already shuts down cleanly on Ctrl+C (validated live). All the machinery lives in the Supervisor (ops side), which is exactly where complexity belongs.

Windows has no POSIX `kill(pid, SIGINT)` — console control events target a **console**, not a PID. The Supervisor therefore *borrows the child's console* to fire the event into it, then restores its own:

```
FreeConsole()                              # detach from own console (no-op for a headless daemon)
AttachConsole(child_pid)                   # attach to the CHILD's console
SetConsoleCtrlHandler(NULL, TRUE)          # Supervisor IGNORES Ctrl+C — so it does NOT suicide
GenerateConsoleCtrlEvent(CTRL_C_EVENT, 0)  # → the child's console group = the child only
SetConsoleCtrlHandler(NULL, FALSE)         # re-enable own handling
FreeConsole(); AttachConsole(ATTACH_PARENT_PROCESS)   # restore
```

The child receives a normal `CTRL_C_EVENT` → `KeyboardInterrupt` in its main thread → the `graceful_shutdown()` it already runs. The properties that make this correct:

- **Each child has its own console** (`CREATE_NEW_CONSOLE`, [§6](#6-process-model)), so attaching reaches exactly one child — no console-sharing, no sibling spillover, and per-process log capture is preserved. (This is why the earlier "console signals force console-sharing" objection does not apply: the Supervisor *attaches per stop*; it does not co-habit a console.)
- **Self-protection is explicit.** `SetConsoleCtrlHandler(NULL, TRUE)` before the send is what keeps the Supervisor alive while it fires Ctrl+C into the borrowed console; it is re-enabled immediately after. This is the "without shutting itself down" guarantee — it is armed, not automatic.
- **The `ctrlc` helper must get its own console.** The engine runs headless (Task-Scheduler- at-logon, [§13](#13-startup--operator-initiated-auto-start-deferred)) with **no console of its own**, so a helper that inherits "no console" will `AttachConsole`/fire and **report success while delivering nothing**. The engine therefore spawns the helper with `CREATE_NEW_CONSOLE`, giving its `FreeConsole → AttachConsole(child)` borrow a deterministic starting console independent of the engine's. (Found in live testing; a compile cannot catch a silent no-op stop.)
- **One console attachment at a time.** The attach→send→detach is serialized and takes microseconds; *issuing* stops is sequential, but the children's `graceful_shutdown()`s then run **in parallel**, so "stop all 10" is ~60s, not 10×60s ([§11](#11-stop--restart-verification-and-sequences)).
- **PID-reuse guard.** Before firing, the Supervisor verifies the PID is still *this* incarnation — the live `status.json` `runner_start_token` (or the process create-time) must match `process_registry.json`. It never Ctrl+C's a recycled PID.

The confirmation that a stop was *clean* does **not** ride this path — it rides the `status.json` + exit code ([§10](#10-graceful-shutdown-contract-system-owned), [§11](#11-stop--restart-verification-and-sequences)). The control path only has to reliably *trigger* `graceful_shutdown()`. There is no payload (a console event carries none) and none is needed: the stop `reason` is the Supervisor's own state, the system records `reason="keyboard_interrupt"`, and the stale-command race a sentinel file would have is absent because the Supervisor targets a **live, verified PID**, not a file.

**`os.kill(pid, CTRL_C_EVENT)` is NOT sufficient** — Python's wrapper calls `GenerateConsoleCtrlEvent` *without* the AttachConsole borrow, so it only works on a shared console. The explicit sequence above (via `ctypes`/`pywin32`) is required.

**Fallback.** A child with **no console** (a true Session-0 service) cannot receive a console event; the only stop is a hard-kill ([§11](#11-stop--restart-verification-and-sequences)), an orphan-connection risk. In the operator-launched model every child has a console, so this is a non-case; it is noted only for the deferred service path.

**Upgrade path.** If synchronous request/ACK with returned reason codes is ever a real need, the drop-in replacement is a **named pipe** (`\\.\pipe\okmich-trader-<system_id>`). That *would* add an in-system listener, so it is taken only if the no-payload one-way trigger actually bites — not before.

## 10. Graceful-shutdown contract (system-owned)

Graceful shutdown is **owned by the trading system, not the Supervisor** — because what "stop" does to a broker session and a book is a system/trade concern, and the Supervisor must not reach inside it. The Supervisor's only job is to trigger it and verify the result.

Each system exposes a single `graceful_shutdown()` routine reachable by **two converging triggers** — and crucially, **the system adds nothing for the Supervisor**:

1. its `SIGINT` / `CTRL_C_EVENT` handler — which serves **both** the Supervisor-driven stop (a targeted Ctrl+C delivered via AttachConsole, [§9](#9-control--targeted-console-ctrlc)) **and** the human who opens the system's own terminal and presses Ctrl+C. Same handler, same `KeyboardInterrupt`, same clean path — the Supervisor just delivers the event the human otherwise would.
2. normal end-of-life exit.

> **Why a targeted Ctrl+C and not a control file?** Because the system's existing Ctrl+C handling already does the right thing, and the AttachConsole trick ([§9](#9-control--targeted-console-ctrlc)) lets the Supervisor aim a `CTRL_C_EVENT` at one child without console-sharing or any in-system listener. The earlier objection that "console signals can't be targeted / force console-sharing" assumed `GenerateConsoleCtrlEvent` from a *shared* console; attaching to the child's own console per-stop sidesteps it entirely. This keeps the trading system dumb — zero control housekeeping — which is the governing constraint. (Ctrl+D is Unix stdin-EOF and has no role on Windows.)

`graceful_shutdown()` MUST, in order:

1. **Stop opening new positions / new working orders.** (Stop taking new risk.)
2. **Leave the existing book exactly as it is.** No close, no flatten, no cancel of protective orders. Positions and working orders persist at the broker, independent of the API connection.
3. **Disconnect the broker session cleanly** — `eDisconnect()` (IB) / `mt5.shutdown()` (MT5). This is the user's top priority: avoid orphan broker connections. A clean `eDisconnect()` frees the IB `clientId` immediately (a hard kill leaves the socket half-open and the `clientId` rejected on reconnect until TCP timeout); `mt5.shutdown()` closes the terminal IPC slot. Disconnecting the session is orthogonal to the book — it frees the connection slot without touching a position.
4. **Write the runner status file `stopped`** (see below).
5. **`exit 0`.**

**Shutdown proof = the runner status file** (`status.json`, not a JSONL record — see LOGGING_CONTRACT §7.1/§7.4). The framework runner writes it atomically (tmp → fsync → `os.replace`) as ONE file at the runner root `<log_base>/<strategy>/status.json` (`<strategy>-multi/...` for a multi-trader); the runner's symbols are listed inside it (`logical_systems[]`) — not mirrored per logical system:

```json
{
  "state": "stopped",
  "runner_id": "mt5_runner-12345",
  "runner_start_token": "a1b2…",
  "broker_disconnected": true,
  "clean": true,
  "reason": "operator_stop",
  "stopped_at": "2026-06-01T14:32:09Z"
}
```

- `state == "stopped"` **and** `broker_disconnected == true` — the Supervisor's clean-stop proof. `broker_disconnected` is the proof the **connection** was released cleanly *and verified*; the status file still at `running` when the PID is gone is an orphan-*connection* suspicion ([§11](#11-stop--restart-verification-and-sequences)).
- `clean` / `reason` — audit (`clean` reflects whether the full graceful path ran).
- **The status file carries no position/exposure fields, by design.** Open positions, working orders, and whether any exposure is protected are **trade concerns owned by the analyst, not the Supervisor** ([§3](#3-principles), [§4](#4-organizational-boundary--ops-admin-vs-trade-analyst)). The Supervisor does not store, display, or judge them. The connection-disconnect proof is the Supervisor's sole post-stop signal; it makes no statement about the book.

## 11. Stop / restart verification and sequences

`T` = **60s** maximum for a graceful exit. A clean disconnect is normally seconds; the headroom covers an IB/MT5 session mid-operation.

**Stop a single system:**

1. Verify the PID from `process_registry.json` is still this incarnation (live `status.json` `runner_start_token` / create-time match), then deliver a targeted `CTRL_C_EVENT` to it via AttachConsole ([§9](#9-control--targeted-console-ctrlc)).
2. Watch the system's `status.json` for `state == "stopped"` with `broker_disconnected: true`.
3. Await `exit 0` within **T = 60s**.
4. **Clean** (status file `stopped` + `broker_disconnected: true` *and* `exit 0`): state → `StoppedByOperator`. The Supervisor makes **no statement about the book** — open positions, if any, are the analyst's concern, invisible to this plane ([§4](#4-organizational-boundary--ops-admin-vs-trade-analyst)).
5. **Timeout / dirty** (no clean exit within T, or exit with the status file still `running` / `broker_disconnected` not true, or a non-zero exit): **hard-kill** (`TerminateProcess`), mark `orphan_suspected`, fire a Telegram alert. A hard-kill is treated as *"I may have just orphaned a broker **connection**"* — the exact case needing human / reaper eyes on the IB side. (This is connection hygiene, not a position judgment.)

**Restart a single system:** steps 1–4 above, then relaunch (→ hard-kill-then- relaunch if it hangs past T). Graceful-shutdown-then-relaunch *is* the clean restart primitive.

**Stop / restart all (or a selected set):** the AttachConsole sends are serialized (microseconds each), but the resulting `graceful_shutdown()`s run **in parallel** — do not serialize the *waiting*. Each system disconnects independently — a separate `clientId` on IB, a separate IPC handle on a shared MT5 terminal, so one system's `mt5.shutdown()` does not disturb the other nine. Worst-case wall-clock for "stop all 10" is therefore **~60s, not 10 × 60s.** Do not serialize the fleet.

### 11.1 Bulk operations — start-all / stop-all / restart-all

The operator's natural verbs over the whole box: *start everything*, *stop everything*, *restart everything*. These are **first-class operations**, but they add **no new control transport and no new in-system contract** — each decomposes entirely into the single-system sequences above and the start of [§13](#13-startup--operator-initiated-auto-start-deferred).

**Scope: the whole box, or one account.** "All" means *every eligible system on this box*. The same operation can be narrowed to one account, the natural [§7](#7-terminal--account-topology-and-blast-radius) blast-radius selector (one broker down: stop that account, keep the others). In the TUI, `S` / `X` / `R` arm one confirm that offers both: `[y]` the whole box, `[a]` only the selected system's account; any other key cancels. An arbitrary selected set remains a future extension.

**Eligibility (idempotent by construction).** A bulk op acts on the eligible subset and **skips** the rest; a skip is success, not error. An empty eligible set is a no-op.

| Op | Targets (eligible) | Skips | Confirm? |
|---|---|---|---|
| **start-all** | `Stopped` | live / in-flight (`Starting` / `Running` / `Stopping` / `Restarting`); **and** the latched states `StoppedByOperator` / `Crashed` / `CrashLoopHalted` | **Yes** |
| **stop-all** | `Running` | already-stopped; in-flight (`Stopping` / `Restarting`) | **Yes** |
| **restart-all** | `Running` | non-running (nothing to restart) | **Yes** |

- **start-all** deliberately starts only `Stopped` systems — the cold-box case of [§13](#13-startup--operator-initiated-auto-start-deferred) ("open the Supervisor → start all"). It does **not** resurrect a `StoppedByOperator` (deliberately stopped) nor override a `Crashed` / `CrashLoopHalted` latch ([§8](#8-lifecycle-state-machine)): clearing those is a **conscious per-system start**, never a side effect of a blanket "start everything." A fleet-wide start must not relaunch something an operator stopped on purpose or that is crash-looping against the market.
  - **Start gates apply per target.** Each fanned-out start passes the same gates as a single start ([§13](#13-startup--operator-initiated-auto-start-deferred)): a red broker session, or an account whose env file is missing or lacks a session key, refuses that target with the reason in its result; the rest start.

- **Every bulk op** requires an **explicit operator confirmation** before any event is fired (start-all too: starting a whole box is as consequential as stopping one). A fleet-wide stop is the consequential, blast-radius action [§3](#3-principles) names — *"a fleet stop is only the explicit per-system stop commands an operator issues"* — so the UI makes that explicitness a deliberate confirm step, not a single keystroke. Per-system stop / restart stays a single action.

**Execution = the single-system sequence, fanned out.** Each target runs the §11 sequence independently: PID-reuse guard, then (stop / restart) a targeted Ctrl+C via AttachConsole, await clean exit within **T = 60s**, hard-kill + `orphan_suspected` + alert on timeout. The AttachConsole **sends are serialized** (microseconds each); the resulting `graceful_shutdown()`s **run in parallel** — *do not serialize the waiting*. Worst-case "stop all 10" ≈ 60s, not 10 × 60s.

**Partial failure is normal and never aborts the batch.** Outcomes are per-system and independent (separate `clientId` / IPC handle). One target timing out → hard-kill + orphan-flag for *that* system only; the rest proceed. There is **no all-or-nothing / two-phase** semantics — that would mean serializing or aborting the fleet, the exact anti-pattern §11 forbids. The op reports each system's outcome plus an aggregate (n started / skipped / failed).

**Command model — a fan-out, not a new shape.** A bulk op is expressed as **one single-system command per eligible target** (`commands/<id>.json`, the existing `Command{action, system_id}`), enumerated by the client from the published `FleetState`. This reuses the whole single-system path — PID-reuse guard, dedup-by-result, per-system `CommandResult` — and keeps the engine's command loop uniform; the engine moves each target to `Starting` / `Stopping` and confirms the terminal state on subsequent ticks rather than block-waiting per system. The aggregate is the client folding the individual results. (A dedicated bulk command carrying a `scope` selector + a single batch-id is the documented **upgrade** — taken only if the per-target fan-out's lack of one audit / confirm record actually bites.)

## 12. Supervisor restart and child re-attach

Because the Supervisor is itself a long-running process on the live box (and may be restarted for admin reasons — *Supervisor lifecycle ≠ trade lifecycle*, [§3](#3-principles)), it must come back **without disturbing running children** and **without blind-relaunching** them.

- On restart the Supervisor **re-discovers** already-running systems and **re-attaches** — it does **not** relaunch them. Blind relaunch of a system that is already trading = duplicate process = duplicate orders.
- Re-attach is cheap by design: control is a console Ctrl+C to a **PID** ([§9](#9-control--targeted-console-ctrlc)) and observation is by tailing each system's JSONL — neither requires a held handle or a reconnect handshake. The Supervisor reads its **process registry** ([§16](#16-folder-and-ipc-layout)) to map `system_id → PID + start_token`, verifies each PID is alive **and is still this incarnation** (`psutil` create-time / live `status.json` start token, guarding PID reuse), and resumes tailing.
- **Re-attach is distinct from auto-start** ([§13](#13-startup--operator-initiated-auto-start-deferred)). Re-attach = "pick the conversation back up with systems that never stopped." Auto-start = "bring systems up from nothing at boot." The first is required (the Supervisor restarts independently); the second is deferred.
- Registry staleness: a registry entry whose PID is dead, or alive but whose recorded `start_token` does not match, is reconciled to ground truth (`psutil`), never trusted blindly.
- Registry keys are `system_id`s, which carry the account ([§16](#16-folder-and-ipc-layout)). The move to the account layout changes every id, which is why it is a stop-the-fleet cutover: no re-attach spans it.

## 13. Startup — operator-initiated; auto-start deferred

**Automatic application startup at boot is deferred**, because broker-session bring-up needs a human:

- **MT5** needs an interactive desktop session (the terminal is a GUI app). The bring-up pattern — dedicated user + autologon + Task-Scheduler-at-logon + `tscon` to detach RDP while keeping the session alive — is real work and is **not** a Session-0 service. (A Session-0 service is isolated from the interactive session the terminal lives in and fights MT5; see [OPS guide §13](../../signalfoundry-lab/docs/ops/OPS_REFERENCE_GUIDE.md) for the related Win32-binding reality.)
- **IB Gateway / TWS** needs a human login + 2FA and a **forced daily re-auth / restart**. The control plane cannot conjure that.

So in the MVP, **startup is an operator action through the Supervisor**, matching the operator's mental model: open the Supervisor → see each system → start all ([§11.1](#111-bulk-operations--start-all--stop-all--restart-all)) or individually. The one safety gate that survives the deferral:

- **Broker-session precondition check.** Before letting an operator start a system, the Supervisor verifies that system's broker session is up and healthy, and **refuses (or warns) if it is red.** This is a *check*, not *automation* — it does not bring the session up; it prevents the classic "launched the trader before the gateway was ready, errored on first bar." Flow:

> human starts MT5 terminals / IB Gateway and logs in → opens Supervisor → Supervisor shows each broker session's health → operator starts systems whose session is **green** → Supervisor refuses to start a system whose session is
> **red**.

- **Account env pre-check (implemented).** A stopped system has no live session to probe, but its account folder already says which session it will need: `.env.<account>` names the terminal and login before anything starts. The Supervisor refuses a start or restart when that file is missing, or lacks a session key its runners need (`TERMINAL_PATH`, `LOGIN_ID`, `LOGIN_SERVER` for MT5; an IB account is recognised by `IB_HOST`), naming what is missing. The account is also listed as a problem in the fleet view. This turns "the runner crashed on its first line" into a refusal that says why.

Deferring auto-start also defers the *adopt-on-restart-at-boot* problem (distinct from the always-running-Supervisor re-attach of [§12](#12-supervisor-restart-and-child-re-attach)), removing the two thorniest pieces from the MVP.

## 14. Broker-session adapters

What generalizes across brokers and what does not:

- **Generic core (broker-blind):** spawn process, watch, trigger graceful stop, relay logs, restart policy, fleet view. Built once.
- **Per-broker session adapter (NOT generic):** *health-checking* (and, when auto-start is eventually built, *bringing up*) the broker session. Mapping systems → session is no longer adapter work: the account folder gives it (folder → `.env.<account>` → `TERMINAL_PATH` / `LOGIN_ID`), so an adapter can probe the right terminal for a system that is not running yet. MT5 (interactive-desktop terminal) and IB (TWS/Gateway socket + 2FA + daily re-auth, automatable via IBC) have genuinely different session lifecycles that do not unify.

The adapter boundary preserves the *control-plane-never-execution-path* rule: an adapter checks *session health and identity*, it does not place trades. The Supervisor talks to adapters for the precondition check ([§13](#13-startup--operator-initiated-auto-start-deferred)); trade systems talk to the broker for execution. These are different conversations.

**Startup ordering** (operator-followed, not yet automated): broker sessions come up **first** — MT5 terminals, then any others — and only then are dependent systems started. IB Gateway in particular will block this with its human login, which is why auto-start is deferred ([§13](#13-startup--operator-initiated-auto-start-deferred)).

## 15. Live monitoring

With a file-based design (no metrics endpoint), **liveness = freshness of the JSONL; behavior = content of the JSONL.** The Supervisor layers observation and stays out of the systems' internals:

| Layer | Question | Mechanism | Owner |
|---|---|---|---|
| **Liveness** | Is it alive *now*? | Has the JSONL advanced within the last few bar-intervals? Absence → wedged/dead | **Supervisor** (fast check) |
| **Per-event** | Did a trade fire / fail? | `TelegramNotifier` on trade open/close/error | trading system |
| **Lifecycle** | Did it start/stop/crash? state? | Process watch + state machine ([§8](#8-lifecycle-state-machine)) | **Supervisor** |
| **Glance** | Watch it right now | `Get-Content -Wait` tail of today's JSONL, surfaced in the UI | **Supervisor** UI |
| **Behavior** | Is the model still sane? | Posterior / KS / loglik streaming gates | **OPS streaming monitor** (separate, on dev) |

- **Out-of-process is correct.** Monitoring lives outside the thing being monitored — an in-process monitor dies with its subject, useless exactly when needed. The Supervisor watches; it does not embed.
- **Liveness vs behavior are different cadences, kept separate.** The OPS streaming monitor (2×/day, on dev) gates *behavior* — too slow as a *liveness* check (a trader that dies at 00:05 would be unseen until 12:00, up to ~12h dark with possibly open positions). The Supervisor's **fast liveness check** (every few minutes, on the live box) closes that gap. Do not fold liveness into the heavy behavior gate — different cadence, different purpose. The OPS guide should add an explicit fast-liveness/heartbeat note alongside its §8/§10 monitoring sections.
- **Wedged = alive but not advancing.** A `Running` system whose JSONL has not advanced past `N×timeframe + grace` is flagged **wedged** — surfaced in the fleet view but **orthogonal to the [§8](#8-lifecycle-state-machine) lifecycle state** (which stays `Running`; the PID is alive). The flag always shows; whether a wedge *pages* is an **operator-tunable runtime setting** (`settings.json`, [§16](#16-folder-and-ipc-layout)) — `always` (correct for 24/7 instruments), `weekend` (skip the FX weekend window when an FX system legitimately emits no bars), or `surface_only` (never page). `N` and `grace` are tunable too. A real per-instrument market calendar belongs to the [§14](#14-broker-session-adapters) broker adapter; until it exists the weekend mode is the stand-in — wrong for 24/7 instruments, hence operator-selectable.
- **Log relay = two channels, not conflated:**
  - **Structured events → tail the JSONL** (the durable record; survives Supervisor restart by re-tailing). Primary channel for the fleet view.
  - **Raw stdout/stderr → per-process ops-log capture** for crash forensics (the unhandled traceback that fires *before* logging is up). Secondary. The live view depends on the **file**, never on stdout capture.
- **Dashboard is a client of the Supervisor, not the Supervisor itself.** The UI (local web via FastAPI, or a Textual TUI for no-browser) reads the Supervisor's state/API. A dashboard crash never touches supervision.

## 16. Folder and IPC layout

Reuses OPS-guide roots ([OPS §3.1](../../signalfoundry-lab/docs/ops/OPS_REFERENCE_GUIDE.md)); adds a Supervisor-owned state directory (registry + ops capture). There is **no control directory** — control is a console event to a live PID ([§9](#9-control--targeted-console-ctrlc)), not a file.

```
<env_dir>\                                  # machine-local broker env files (OPS §3.4)
├── .env.<account>                          # one per account: .env.fxify.demo, .env.icmarkets.demo, ...
└── templates\                              # non-secret templates created by setup

<supervisor_state>\                         # Supervisor's own state (live box)
├── process_registry.json                   # system_id → {pid, start_token, create_time}
├── settings.json                           # operator-tunable runtime policy (wedge alert gate + thresholds, §15)
├── fleet_state.json                        # the published fleet: systems, accounts, problems
└── commands\                               # TUI → engine command files (one per single-system action)

# Consumed from OPS-guide roots:
<live_base>\<account>\                      # <account> = the env-file stem <broker>.<env>
├── <strategy>\<symbol>\<timeframe>\run.py  # a single-trader
├── <strategy>-multi\run.py                 # a multi-trader (config.json with a non-empty strategies[])
└── <runner>\run.py                         # a runner, e.g. _account_admin (the Account Admin)
<log_base>\<account>\<root>\status.json     # ONE lifecycle file per process: running/stopped + disconnect proof + restart token
<log_base>\<account>\<root>\<symbol>\<timeframe>\inference\inference_<YYYYMMDD>.jsonl   # per-leg bar/breaker stream (liveness + behavior)
```

- **Accounts.** Every top-level folder of `<live_base>` named like `<broker>.<env>` is an account; its env file is `<env_dir>\.env.<account>`. The Supervisor reads only identity keys from it (`LOGIN_ID`, `LOGIN_SERVER`, `BROKER_NAME`, `TERMINAL_PATH`, `IB_HOST`), never secrets. Dot-folders (`.archive`, `.staging`) are the importer's own.
- **`system_id`** — a stable identifier per PID-level unit, always prefixed by its account:
  - a single-trader: `<account>/<strategy>/<symbol>/<timeframe>` (its path);
  - a multi-trader: `<account>/<strategy>-multi` (one PID, N symbols; stopped as a **unit**, per-symbol stop is not offered, by construction);
  - a runner: `<account>/<folder>`, for a `run.py` directly under the account folder that is not a multi-trader. Its folder is its identity and its log root; its liveness legs come from its `status.json`. The Account Admin is one (`<account>/_account_admin`).

  The id distinguishes the **PID-level unit** the Supervisor stops (one Ctrl+C → one PID, [§9](#9-control--targeted-console-ctrlc)) from the **system-level units** tracked for blast radius ([§7](#7-terminal--account-topology-and-blast-radius)).
- **Problems.** A `run.py` outside an account folder (the pre-account flat layout), or at a path that is none of the three shapes, and an account whose env file is missing or incomplete, are published as problems in the fleet view. A problem is never started.
- **The account is passed, not configured.** The Supervisor starts each `run.py` (no arguments, cwd = its folder) with its own environment plus `OKMICH_QUANT_ACCOUNT=<account>`, always overriding any inherited value. The runner loads `<env_dir>\.env.<account>` from it and the framework logs under `<log_base>\<account>\` (LOGGING_CONTRACT §10). A supervised launch and a manual one differ only in who sets the variable.
- **Import** provisions an artefact into an account the operator picks (the TUI lists the accounts that have an env file): a staged copy, the current one archived to `.archive\<account>\...`, then an atomic rename. A runner artefact declares itself with `"runner": "<name>"` in its `config.json`. **Decommission** archives one system the Supervisor currently discovers — never a path built from an id — and refuses while it runs.
- **The Account Admin is the one exception** ([§3](#3-principles)): importing into `_account_admin` carries the installed `directive.json`, `state.json` and `requests\` into the new copy and never takes them from the source; decommissioning it is refused ([ACCOUNT_ADMIN_SPEC §3, §8.3, §9](../../signalfoundry-lab/docs/ops/ACCOUNT_ADMIN_SPEC.md)).
- **`supervisor migrate-layout`** moves a flat box into this layout once (dry run by default, `--apply` to move; refuses while anything runs; proves every move before making the first) — the cutover of [ACCOUNT_LAYOUT_CHANGE_PLAN §5](../../signalfoundry-lab/docs/ops/ACCOUNT_LAYOUT_CHANGE_PLAN.md).
- `process_registry.json` is the re-attach source of truth ([§12](#12-supervisor-restart-and-child-re-attach)), always reconciled against `psutil`, never trusted blindly.
- `<supervisor_state>` is resolved from `OKMICH_QUANT_SUPERVISOR_STATE_DIR`, a required production env var: a missing or unusable value fails fast at Supervisor startup. Any `D:\...` examples in the OPS guide are deployment examples, not source-code defaults.
- `<env_dir>` is resolved from `OKMICH_QUANT_ENV_DIR` and is created/permission-checked by Supervisor setup before any trading system is deployed or started. Env filenames are audit data; secret values are never read or logged.

### 16.1 Supervisor setup / bootstrap responsibilities

The Supervisor utility owns the live-box bootstrap gate. A box is not live-ready until setup has completed these one-time tasks:

1. Verify broker platform prerequisites and ops utilities are installed.
2. Define required machine-level env vars: `OKMICH_QUANT_LIVE_BASE`, `OKMICH_QUANT_LOG_BASE`, `OKMICH_QUANT_ENV_DIR`, and `OKMICH_QUANT_SUPERVISOR_STATE_DIR`. **Never** define `OKMICH_QUANT_ACCOUNT` at machine level: it is per process, set by the Supervisor from the account folder (a machine-wide value would mislead manual runs).
3. Create and permission-check `<live_base>`, `<log_base>`, `<env_dir>`, and `<supervisor_state>`.
4. Install non-secret broker env templates under `<env_dir>\templates\`.
5. Create one account folder `<live_base>\<account>\` per operator-populated `.env.<account>`, and check each env file carries its session keys ([§13](#13-startup--operator-initiated-auto-start-deferred)). The running Supervisor re-checks this every tick and lists any gap as a problem.

Only after this bootstrap succeeds should artefacts be deployed or trading systems be started. Runtime code still fails fast if a required root or broker env file is missing; setup makes that failure an installation-time problem instead of a live startup surprise.

## 17. Office telemetry — Fleet Manager (future)

The **Fleet Manager** aggregates many per-box Supervisors at the office. Pointer only; full design is future work.

- **Outbound push only.** Each box's Supervisor *reports up*; the office does **not** reach *in* to control trading over WAN — not until a real auth model and threat model exist. Inbound remote control of a live-trading box is a network- exposed kill switch and is explicitly out of scope until then. This matches the OPS guide's outbound-push posture for logs/artefacts.
- **Telemetry, not control.** Start with the box reporting state (which systems up, liveness, lifecycle events, broker-session health). Defer remote *control* entirely.
- Naming: the per-box thing stays the **Supervisor**; the cross-box aggregator is the **Fleet Manager** ([§5](#5-vocabulary-supervisor-vs-fleet-manager)).

## 18. Open items

> **Resolved 2026-06-01 — protective-stop residence is out of scope, by principle.** Whether a system holds protective stops broker-side or in-loop is
> **not the Supervisor's concern**: it is a trade property, owned by the analyst, decided at trade-design time. The Supervisor does not store, display, or judge position protection — doing so would be a trade-risk judgment it must not make ([§3](#3-principles), [§4](#4-organizational-boundary--ops-admin-vs-trade-analyst)). Accordingly the earlier `positions_left` / `app_managed_protection` shutdown fields and the amber fleet-view state were **removed** from the design. The Supervisor's only post-stop signal is the connection-orphan flag ([§10](#10-graceful-shutdown-contract-system-owned), [§11](#11-stop--restart-verification-and-sequences)) — orphaned **connection** is ops; open/unprotected **position** is trade.

> **Resolved 2026-06-02 — control granularity is the PID.** With control now a targeted Ctrl+C to a PID (no control file, [§9](#9-control--targeted-console-ctrlc)), a multi-trader is stopped as a **unit**: one PID, one broker session, one stop. `system_id` still distinguishes the PID-level stop unit from the system-level blast-radius units it tracks ([§7](#7-terminal--account-topology-and-blast-radius), [§16](#16-folder-and-ipc-layout)); there is no longer a "how does one command address N systems" question.

> **Resolved 2026-09-25 — which account a system trades.** It was a hardcoded `--env-file` default inside each `run.py`, invisible to the Supervisor until the system ran. It is now the system's account folder ([§16](#16-folder-and-ipc-layout)), passed as `OKMICH_QUANT_ACCOUNT`, grouped in the fleet view and checked at run time ([§7](#7-terminal--account-topology-and-blast-radius)).

Remaining open items:
3. **Restart policy specifics** — backoff curve, crash-loop N/M thresholds, and whether the MVP enables auto-restart on `Crashed` at all or treats it as terminal-until-operator (deferred with auto-start, [§8](#8-lifecycle-state-machine), [§13](#13-startup--operator-initiated-auto-start-deferred)).
4. **UI client form** — Textual TUI vs local FastAPI web dashboard (both are clients of the same Supervisor state; [§15](#15-live-monitoring)).

## 19. Glossary

| Term | Definition |
|---|---|
| **Supervisor** | The per-box process control plane: discover / start / stop / restart / watch trading systems, with a local UI client. The subject of this spec. |
| **Fleet Manager** | The future cross-box office layer that aggregates many Supervisors via outbound telemetry. |
| **Trading system** | One deployed strategy instance run as one OS process; a `trader` (one symbol), a `multi-trader` (N symbols, one PID) or a runner. |
| **Account** | A broker env-file stem `<broker>.<env>` (`fxify.demo`): one env file, one terminal, one login. The first folder level under `<live_base>` and `<log_base>`; the Supervisor passes it to each system as `OKMICH_QUANT_ACCOUNT` ([§16](#16-folder-and-ipc-layout)). |
| **Runner** | A `run.py` directly under an account folder that is not a multi-trader; its folder is its id and its log root. The Account Admin is one ([§16](#16-folder-and-ipc-layout)). |
| **Account check** | The run-time check that a running system is logged into the account its folder claims; identity, not a trade judgment ([§4](#4-organizational-boundary--ops-admin-vs-trade-analyst), [§7](#7-terminal--account-topology-and-blast-radius)). |
| **Logical system** | One OS process = one broker connection = one MT5 terminal IPC slot (`mt5.initialize()` binds per-process); the unit of blast radius and of the ≤10/terminal cap. A `multi-trader` is **one** logical system carrying several **legs**. |
| **Leg** | One symbol inside a logical system. A 4-symbol `multi-trader` is 1 logical system / 4 legs. Legs drive account concentration (shared margin, magic-number namespace), are reported per terminal, and are never capped ([§7](#7-terminal--account-topology-and-blast-radius)). |
| **Control (stop)** | The Supervisor→system stop transport: a targeted `CTRL_C_EVENT` delivered to one child via AttachConsole ([§9](#9-control--targeted-console-ctrlc)), reusing the system's existing Ctrl+C graceful path. No in-system listener. |
| **Bulk operation** | A `start-all` / `stop-all` / `restart-all` over the whole box or one account, confirmed by the operator — a fan-out of single-system commands over the eligible subset, not a new transport; an arbitrary selected set is a deferred extension ([§11.1](#111-bulk-operations--start-all--stop-all--restart-all)). |
| **Shutdown proof** | The `stopped` write to the runner **status file** (`status.json`) in `graceful_shutdown()`, proving a clean broker **connection** disconnect (`broker_disconnected: true`) — a status-file write, not a stream record (v1.1.0; LOGGING_CONTRACT §7.1/§7.4). Carries no position/exposure fields — exposure is a trade concern, invisible to the Supervisor ([§3](#3-principles), [§4](#4-organizational-boundary--ops-admin-vs-trade-analyst), [§10](#10-graceful-shutdown-contract-system-owned)). |
| **Start token** | An identifier a system records at startup (in `status.json`); ops's restart-detection signal and the PID-reuse guard checked before a stop is fired at a PID ([§9](#9-control--targeted-console-ctrlc)). |
| **Graceful shutdown** | System-owned routine: stop new risk, leave the book, disconnect the broker cleanly, write the `stopped` status file, exit 0. |
| **Orphan (connection)** | A broker API connection left half-open by a hard kill (no clean `eDisconnect`/`mt5.shutdown`); the failure mode graceful shutdown exists to prevent. |
| **Blast radius** | The set of logical systems that fail together when a shared broker terminal/account fails; in the fleet view, an account folder's systems. |
| **Re-attach** | A restarted Supervisor re-discovering and resuming control of still-running children, without relaunching them. Distinct from auto-start. |
| **Broker-session adapter** | The per-broker (MT5 / IB) component that health-checks (later: brings up) a broker session; never places trades. |

---

*End of FLEET_SUPERVISOR_SPEC.md*
