// Package contract is the read-side of LOGGING_CONTRACT v1.1.0 — the supervisor's ONLY
// structural dependency on the trading systems. It reads the runner-root status.json and the
// inference JSONL; it never imports the Python framework.
package contract

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RunnerStatus mirrors logging/schema/runner_status.json (the fields the supervisor uses).
type RunnerStatus struct {
	LogSchemaVersion   string          `json:"log_schema_version"`
	State              string          `json:"state"` // "running" | "stopped"
	RunnerID           string          `json:"runner_id"`
	RunnerStartToken   string          `json:"runner_start_token"`
	PID                int             `json:"pid"`
	Broker             string          `json:"broker"`
	AccountID          string          `json:"account_id"`
	BrokerSessionID    string          `json:"broker_session_id"`
	LogicalSystems     []LogicalSystem `json:"logical_systems"`
	StartedAt          time.Time       `json:"started_at"`
	StoppedAt          *time.Time      `json:"stopped_at"`
	BrokerDisconnected *bool           `json:"broker_disconnected"`
	Clean              *bool           `json:"clean"`
	Reason             *string         `json:"reason"`
}

type LogicalSystem struct {
	LogicalSystemID string `json:"logical_system_id"`
	Symbol          string `json:"symbol"`
	Timeframe       int    `json:"timeframe"`
}

// StatusPath is the runner-root status file: <logBase>/<runnerStrategy>/status.json, where
// runnerStrategy is <strategy> (single trader) or <strategy>-multi (multi-trader).
func StatusPath(logBase, runnerStrategy string) string {
	return filepath.Join(logBase, runnerStrategy, "status.json")
}

// InferenceDir is <logBase>/<runnerStrategy>/<symbol>/<timeframe>/inference.
func InferenceDir(logBase, runnerStrategy, symbol, timeframe string) string {
	return filepath.Join(logBase, runnerStrategy, symbol, timeframe, "inference")
}

// ReadStatus loads a runner-root status.json.
func ReadStatus(path string) (RunnerStatus, error) {
	var s RunnerStatus
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// LastBarTS returns the most recent `bar` record's asof_bar_ts in today's inference file (the
// liveness clock). It scans the file's TAIL backward for the latest line that parses as a bar — so
// it is unaffected by a non-bar event (breaker/trade) or a partial mid-write line being last, and
// its cost is constant regardless of how large the day's JSONL has grown (it is polled every tick).
// Returns false if there is no file / no bar in the tail window. TODO: handle the UTC-midnight
// rollover by also checking yesterday's file near the boundary.
func LastBarTS(inferenceDir string) (time.Time, bool) {
	name := "inference_" + time.Now().UTC().Format("20060102") + ".jsonl"
	lines, ok := tailLines(filepath.Join(inferenceDir, name))
	if !ok {
		return time.Time{}, false
	}
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var rec struct {
			Event     string    `json:"event"`
			AsofBarTS time.Time `json:"asof_bar_ts"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Event == "bar" {
			return rec.AsofBarTS, true
		}
	}
	return time.Time{}, false
}

// tailLines returns the lines of a file's last tailWindow bytes. A JSONL bar record recurs far more
// often than tailWindow, so the latest bar is always within the window (a partial first line in the
// window is harmless — callers scan from the end and skip unparseable lines).
func tailLines(path string) ([]string, bool) {
	const tailWindow = 64 * 1024
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return nil, false
	}
	start := int64(0)
	if fi.Size() > tailWindow {
		start = fi.Size() - tailWindow
	}
	buf := make([]byte, fi.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return nil, false
	}
	return strings.Split(string(buf), "\n"), true
}
