// Package session is the broker-session precondition check (FLEET_SUPERVISOR_SPEC §13/§14): before
// the engine starts a trading system, it asks whether that system's broker session is healthy and
// refuses on a definite "red". The check is broker-specific — MT5's interactive desktop terminal and
// IB's gateway+2FA do not unify — so a real probe is an Adapter per broker. An adapter inspects
// session health/identity ONLY; it never places trades (the control-plane-never-execution-path rule).
//
// No real MT5/IB probe is wired yet. Until one is, the Checker resolves health from an operator
// override file (also a legitimate maintenance lockout) and otherwise reports Unknown — and the gate
// can REFUSE only on a definite Red, so an unprobed deployment behaves exactly as before.
package session

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Health string

const (
	Green   Health = "green"   // session up and healthy
	Red     Health = "red"     // session down/unhealthy — refuse starts (§13)
	Unknown Health = "unknown" // not probed — the gate allows the start
)

// Ref identifies a broker session to probe. For a running system all three come from its status.json;
// broker/account originate in the runner's .env (LOGIN_SERVER/LOGIN_ID) and session_id from the live
// terminal, so a stopped system has no session_id until its terminal is up (a real adapter resolves
// the stopped-system → session mapping; the generic core cannot).
type Ref struct {
	Broker    string
	Account   string
	SessionID string
}

// Status is a session's probed health plus an operator-facing reason.
type Status struct {
	Health Health
	Detail string
}

// Adapter health-checks one broker's sessions (§14): probe-only, never trades. None are implemented
// yet (a real probe needs the broker's own client — MT5 terminal_info, IB gateway). Register one via
// Checker.Register; that is the drop-in point.
type Adapter interface {
	Probe(Ref) Status
}

// Checker is the broker-session precondition oracle (§13). Resolution order per probe:
//  1. the operator override file (session_health.json: broker_session_id → green|red|unknown) — the
//     stand-in until adapters exist, and a manual maintenance lockout;
//  2. the per-broker Adapter, if registered;
//  3. Unknown — unprobed, so the gate ALLOWS the start (absence of a probe never blocks).
type Checker struct {
	overridePath string
	adapters     map[string]Adapter
}

// NewChecker builds a checker whose override file lives in the supervisor state dir.
func NewChecker(stateDir string) *Checker {
	return &Checker{
		overridePath: filepath.Join(stateDir, "session_health.json"),
		adapters:     map[string]Adapter{},
	}
}

// Register wires a real per-broker adapter (e.g. "mt5", "ib"). The drop-in point for real probes.
func (c *Checker) Register(broker string, a Adapter) { c.adapters[broker] = a }

// Probe reports a session's health (override → adapter → Unknown). Nil-safe.
func (c *Checker) Probe(r Ref) Status {
	if c == nil {
		return Status{Health: Unknown, Detail: "no session checker"}
	}
	if h, ok := c.override(r.SessionID); ok {
		return Status{Health: h, Detail: "operator override (session_health.json)"}
	}
	if a := c.adapters[r.Broker]; a != nil {
		return a.Probe(r)
	}
	return Status{Health: Unknown, Detail: unprobedDetail(r.Broker)}
}

// Allowed reports whether a start may proceed: anything but a definite Red. The Status carries the
// operator-facing reason for a refusal.
func (c *Checker) Allowed(r Ref) (bool, Status) {
	st := c.Probe(r)
	return st.Health != Red, st
}

// override reads the per-session health override; missing file / entry / malformed JSON all mean "no
// override" (fall through to the adapter or Unknown). Read per probe — the file is tiny and probes
// are few-per-tick.
func (c *Checker) override(sessionID string) (Health, bool) {
	if sessionID == "" || c.overridePath == "" {
		return "", false
	}
	b, err := os.ReadFile(c.overridePath)
	if err != nil {
		return "", false
	}
	var m map[string]Health
	if json.Unmarshal(b, &m) != nil {
		return "", false
	}
	switch h := m[sessionID]; h {
	case Green, Red, Unknown:
		return h, true
	}
	return "", false
}

func unprobedDetail(broker string) string {
	if broker == "" {
		return "unprobed — no session adapter"
	}
	return "unprobed — no session adapter for " + broker
}
