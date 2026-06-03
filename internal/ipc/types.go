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
	Terminals []Terminal `json:"terminals"` // blast-radius grouping (§7)
	Systems   []System   `json:"systems"`
}

type EngineInfo struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Version   string    `json:"version"`
	Alerts    bool      `json:"alerts"` // Telegram alerting enabled (creds present)
}

// Terminal groups the logical systems that share one broker session/account (die together).
type Terminal struct {
	BrokerSessionID string   `json:"broker_session_id"`
	Account         string   `json:"account_id"`
	SystemIDs       []string `json:"system_ids"`
}

// LogPaths let the TUI tail raw logs directly (the engine does not proxy log bytes).
type LogPaths struct {
	Inference string `json:"inference"`
	Status    string `json:"status"`
	Text      string `json:"text,omitempty"`
}

type System struct {
	SystemID    string    `json:"system_id"`
	Strategy    string    `json:"strategy"`
	Symbol      string    `json:"symbol"`
	Timeframe   string    `json:"timeframe"` // the directory label, e.g. "M5" (not minutes)
	State       State     `json:"state"`
	PID         int       `json:"pid,omitempty"`
	StartToken  string    `json:"runner_start_token,omitempty"`
	Broker      string    `json:"broker,omitempty"`
	Account     string    `json:"account_id,omitempty"`
	SessionID   string    `json:"broker_session_id,omitempty"`
	LastBarTS   time.Time `json:"last_bar_ts"`
	LastBarAgeS float64   `json:"last_bar_age_s"`   // liveness: seconds since last bar
	Wedged      bool      `json:"wedged,omitempty"` // alive but JSONL stale past threshold (§15); orthogonal to State
	LogPaths    LogPaths  `json:"log_paths"`
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
