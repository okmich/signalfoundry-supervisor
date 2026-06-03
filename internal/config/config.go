// Package config resolves the required ops roots from the environment (fail-fast). There is
// deliberately no hardcoded deployment fallback — deployment paths are examples, not defaults.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Config holds the resolved ops roots the supervisor needs.
type Config struct {
	LiveBase string // OKMICH_QUANT_LIVE_BASE  — <strategy>/<symbol>/<timeframe>/run.py per system
	LogBase  string // OKMICH_QUANT_LOG_BASE   — status.json (runner root) + inference JSONL
	EnvDir   string // OKMICH_QUANT_ENV_DIR    — machine-local broker .env files
	StateDir string // OKMICH_QUANT_SUPERVISOR_STATE_DIR — registry + fleet_state + commands
	Python   string // OKMICH_QUANT_PYTHON (optional) — interpreter used to spawn run.py; default "python"

	// Control timeouts (FLEET_SUPERVISOR_SPEC §11, T=60s). Optional env overrides — primarily for
	// tests that must exercise the timeout paths without a 60s wait; ops may also tune them.
	StopTimeout  time.Duration // OKMICH_QUANT_STOP_TIMEOUT  — graceful-stop window before hard-kill
	StartTimeout time.Duration // OKMICH_QUANT_START_TIMEOUT — window for a spawned system to reach running

	GlobalConfig string // OKMICH_QUANT_GLOBAL_CONFIG (optional) — the .global dir holding notifier.env (alerting)

	// Liveness/wedged thresholds (FLEET_SUPERVISOR_SPEC §15). Wedged when a Running system's last
	// bar is older than WedgeMultiple×timeframe + WedgeGrace. Optional env overrides.
	WedgeMultiple int           // OKMICH_QUANT_WEDGE_MULTIPLE — missed bar intervals before wedged (default 3)
	WedgeGrace    time.Duration // OKMICH_QUANT_WEDGE_GRACE     — slack added to the threshold (default 1m)
}

// MustLoad resolves the config or exits non-zero with a clear error.
func MustLoad() Config {
	cfg, err := Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	return cfg
}

// Load reads the required env vars and validates none are empty.
func Load() (Config, error) {
	c := Config{
		LiveBase: os.Getenv("OKMICH_QUANT_LIVE_BASE"),
		LogBase:  os.Getenv("OKMICH_QUANT_LOG_BASE"),
		EnvDir:   os.Getenv("OKMICH_QUANT_ENV_DIR"),
		StateDir: os.Getenv("OKMICH_QUANT_SUPERVISOR_STATE_DIR"),
	}
	required := map[string]string{
		"OKMICH_QUANT_LIVE_BASE":            c.LiveBase,
		"OKMICH_QUANT_LOG_BASE":             c.LogBase,
		"OKMICH_QUANT_ENV_DIR":              c.EnvDir,
		"OKMICH_QUANT_SUPERVISOR_STATE_DIR": c.StateDir,
	}
	for k, v := range required {
		if v == "" {
			return Config{}, fmt.Errorf("required env var %s is not set", k)
		}
	}
	// Optional: the interpreter used to spawn run.py (a venv/conda python). The OPS guide launches
	// systems as `python run.py`; ops can point this at a specific interpreter via OKMICH_QUANT_PYTHON.
	if c.Python = os.Getenv("OKMICH_QUANT_PYTHON"); c.Python == "" {
		c.Python = "python"
	}
	c.StopTimeout = durationEnv("OKMICH_QUANT_STOP_TIMEOUT", 60*time.Second)
	c.StartTimeout = durationEnv("OKMICH_QUANT_START_TIMEOUT", 60*time.Second)
	c.GlobalConfig = os.Getenv("OKMICH_QUANT_GLOBAL_CONFIG")
	c.WedgeMultiple = intEnv("OKMICH_QUANT_WEDGE_MULTIPLE", 3)
	c.WedgeGrace = durationEnv("OKMICH_QUANT_WEDGE_GRACE", time.Minute)
	return c, nil
}

// intEnv reads a positive int from env, falling back to def if unset or unparseable.
func intEnv(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// NotifierEnvPath is <global_config>/notifier.env (the Telegram/SMTP creds file), or "" when no
// global-config root is configured (alerting then stays disabled).
func (c Config) NotifierEnvPath() string {
	if c.GlobalConfig == "" {
		return ""
	}
	return filepath.Join(c.GlobalConfig, "notifier.env")
}

// durationEnv reads a Go duration string (e.g. "60s", "3s") from env, falling back to def if unset
// or unparseable.
func durationEnv(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// File-IPC locations under StateDir.
func (c Config) FleetStatePath() string { return filepath.Join(c.StateDir, "fleet_state.json") }
func (c Config) CommandsDir() string    { return filepath.Join(c.StateDir, "commands") }
func (c Config) RegistryPath() string   { return filepath.Join(c.StateDir, "process_registry.json") }
func (c Config) SettingsPath() string   { return filepath.Join(c.StateDir, "settings.json") }
