package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckerOverride(t *testing.T) {
	dir := t.TempDir()
	c := NewChecker(dir)
	ref := Ref{Broker: "deriv", Account: "DEMO-123", SessionID: "mt5-deriv-1"}

	// No override file: Unknown, and the gate ALLOWS (absence of a probe never blocks).
	if ok, st := c.Allowed(ref); !ok || st.Health != Unknown {
		t.Errorf("no override: got (allowed=%v, %s), want (true, unknown)", ok, st.Health)
	}

	// Red override: refused.
	write(t, dir, `{"mt5-deriv-1":"red"}`)
	if ok, st := c.Allowed(ref); ok || st.Health != Red {
		t.Errorf("red override: got (allowed=%v, %s), want (false, red)", ok, st.Health)
	}

	// Green override: allowed.
	write(t, dir, `{"mt5-deriv-1":"green"}`)
	if ok, st := c.Allowed(ref); !ok || st.Health != Green {
		t.Errorf("green override: got (allowed=%v, %s), want (true, green)", ok, st.Health)
	}

	// An entry for a different session does not affect this one.
	write(t, dir, `{"other":"red"}`)
	if ok, _ := c.Allowed(ref); !ok {
		t.Errorf("an override for another session should not gate this one")
	}
}

func TestCheckerEmptyAndNil(t *testing.T) {
	// Empty session id never matches an override (a stopped system has none) → Unknown/allowed.
	c := NewChecker(t.TempDir())
	write(t, c.overrideDir(), `{"":"red"}`) // even a bogus empty-key entry must not gate
	if ok, _ := c.Allowed(Ref{SessionID: ""}); !ok {
		t.Errorf("empty session id should never be gated")
	}
	// Nil checker is safe and Unknown.
	var nilC *Checker
	if ok, st := nilC.Allowed(Ref{SessionID: "x"}); !ok || st.Health != Unknown {
		t.Errorf("nil checker: got (allowed=%v, %s), want (true, unknown)", ok, st.Health)
	}
}

func TestCheckerAdapterFallback(t *testing.T) {
	c := NewChecker(t.TempDir()) // no override file
	c.Register("ib", probeFunc(func(Ref) Status { return Status{Health: Red, Detail: "gateway down"} }))
	if ok, _ := c.Allowed(Ref{Broker: "ib", SessionID: "ib-1"}); ok {
		t.Errorf("a registered adapter reporting red should refuse")
	}
	// A broker with no adapter and no override stays Unknown → allowed.
	if ok, st := c.Allowed(Ref{Broker: "mt5", SessionID: "mt5-1"}); !ok || st.Health != Unknown {
		t.Errorf("unadaptered broker: got (allowed=%v, %s), want (true, unknown)", ok, st.Health)
	}
}

// helpers

type probeFunc func(Ref) Status

func (f probeFunc) Probe(r Ref) Status { return f(r) }

func (c *Checker) overrideDir() string { return filepath.Dir(c.overridePath) }

func write(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "session_health.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
