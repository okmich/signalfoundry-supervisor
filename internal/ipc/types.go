// Package ipc is the file-based contract between the engine (writer of state, reader of
// commands) and the TUI (reader of state, writer of commands). These structs are the single
// source of truth for the on-disk JSON shapes.
package ipc

import "time"

// State is the per-system lifecycle state (FLEET_SUPERVISOR_SPEC §8).
type State string

const (
	StateStopped         State = "Stopped"
	StateStarting        State = "Starting"
	StateRunning         State = "Running"
	StateStopping        State = "Stopping"
	StateRestarting      State = "Restarting"
	StateStoppedByOp     State = "StoppedByOperator"
	StateCrashed         State = "Crashed"
	StateOrphanSuspected State = "OrphanSuspected"
	StateCrashLoopHalted State = "CrashLoopHalted"
)

// FleetState is the snapshot the engine publishes each tick and the TUI renders.
type FleetState struct {
	Engine    EngineInfo `json:"engine"`
	UpdatedAt time.Time  `json:"updated_at"`
	Accounts  []Account  `json:"accounts"`           // one group per account folder, stopped systems included (§7)
	Systems   []System   `json:"systems"`            // ordered by account, then system id
	Problems  []Problem  `json:"problems,omitempty"` // things under LIVE_BASE that cannot run (flat layout, no env file)
}

// Problem is a LIVE_BASE entry the engine will not run, surfaced to the operator.
type Problem struct {
	Path   string `json:"path"` // relative to LIVE_BASE
	Reason string `json:"reason"`
}

type EngineInfo struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Version   string    `json:"version"`
	Alerts    bool      `json:"alerts"` // Telegram alerting enabled (creds present)
}

// Account groups the systems of one account folder, <live_base>/<account>. One env file = one terminal +
// one login, so they share a broker session and die together (§7). Built from the folder, so a group
// exists — with its stopped systems — even when nothing in it runs.
type Account struct {
	Name            string   `json:"account"`                     // the folder: the env-file stem, e.g. fxify.demo
	Login           string   `json:"login,omitempty"`             // LOGIN_ID from .env.<account>
	Server          string   `json:"server,omitempty"`            // LOGIN_SERVER from .env.<account>
	EnvMissing      bool     `json:"env_missing,omitempty"`       // no .env.<account> in ENV_DIR: its systems cannot start
	EnvMissingKeys  []string `json:"env_missing_keys,omitempty"`  // session keys .env.<account> lacks: its systems cannot start
	BrokerSessionID string   `json:"broker_session_id,omitempty"` // from a running system's status.json
	Broker          string   `json:"broker,omitempty"`            // status.json broker (for the session probe)
	SystemIDs       []string `json:"system_ids"`
	LogicalSystems  int      `json:"logical_systems"`  // live PIDs: the ≤10/terminal cap unit: ONE PID = one logical system = one MT5 terminal IPC slot (mt5.initialize binds per-process), whatever its symbol count
	Legs            int      `json:"legs"`             // symbols carried across those PIDs — account concentration (shared margin, magic-number namespace), NOT a cap
	Health          string   `json:"health,omitempty"` // broker-session precondition health (green|red|unknown, §13)
}

// SystemLeg is one logical system's liveness clock inside a multi-trader runner (§16). A single-
// trader has no legs — its own Timeframe/LastBarTS is the clock; a multi-trader carries one leg per
// symbol so the runner is judged wedged if ANY leg is stale past its OWN cadence (§15), which a
// single per-row timeframe cannot express when the basket mixes cadences.
type SystemLeg struct {
	Symbol    string    `json:"symbol"`
	Timeframe string    `json:"timeframe"`
	Inference string    `json:"inference,omitempty"` // this symbol's inference dir (the TUI Glance tail, §15)
	LastBarTS time.Time `json:"last_bar_ts"`
	Wedged    bool      `json:"wedged,omitempty"` // this leg is stale past its own cadence (engine-tagged, §15)
}

// LogPaths let the TUI tail raw logs directly (the engine does not proxy log bytes).
type LogPaths struct {
	Inference string `json:"inference"`
	Status    string `json:"status"`
	Text      string `json:"text,omitempty"`
}

type System struct {
	SystemID   string      `json:"system_id"`
	Strategy   string      `json:"strategy"`
	Symbol     string      `json:"symbol"`
	Timeframe  string      `json:"timeframe"`         // the directory label, e.g. "M5" (not minutes)
	Multi      bool        `json:"multi,omitempty"`   // a multi-trader runner (one PID, N symbols, §16)
	Symbols    []string    `json:"symbols,omitempty"` // multi only: the logical-system symbols it carries
	Legs       []SystemLeg `json:"legs,omitempty"`    // multi only: per-symbol liveness clocks (§15 runner-level liveness)
	State      State       `json:"state"`
	PID        int         `json:"pid,omitempty"`
	StartToken string      `json:"runner_start_token,omitempty"`
	Account    string      `json:"account"`              // the account folder (env-file stem)
	Broker     string      `json:"broker,omitempty"`     // status.json broker
	AccountID  string      `json:"account_id,omitempty"` // status.json account_id: the terminal's login
	SessionID  string      `json:"broker_session_id,omitempty"`
	// AccountMismatch explains why the running system does not match its account folder (status.json
	// account, or the terminal login vs the env file's LOGIN_ID); "" when they agree or cannot be checked.
	AccountMismatch string    `json:"account_mismatch,omitempty"`
	StartedAt       time.Time `json:"started_at"` // runner start time (status.json), for the details view
	LastBarTS       time.Time `json:"last_bar_ts"`
	LastBarAgeS     float64   `json:"last_bar_age_s"`   // liveness: seconds since last bar
	Wedged          bool      `json:"wedged,omitempty"` // alive but JSONL stale past threshold (§15); orthogonal to State
	LogPaths        LogPaths  `json:"log_paths"`
}

// Command is what the TUI drops for the engine: commands/<id>.json.
type Command struct {
	ID       string    `json:"id"`
	Action   string    `json:"action"` // start | stop | restart
	SystemID string    `json:"system_id"`
	IssuedAt time.Time `json:"issued_at"`
}

// CommandResult is what the engine writes back: commands/<id>.result.json (also the dedup marker).
type CommandResult struct {
	ID        string    `json:"id"`
	Accepted  bool      `json:"accepted"`
	Outcome   string    `json:"outcome"` // started | stopping | restarting | failed
	Error     string    `json:"error,omitempty"`
	Completed time.Time `json:"completed_at"`
}
