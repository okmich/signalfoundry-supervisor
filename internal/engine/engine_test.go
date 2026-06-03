package engine

import (
	"os"
	"testing"
	"time"

	"github.com/okmich/signalfoundry-supervisor/internal/config"
	"github.com/okmich/signalfoundry-supervisor/internal/ipc"
	"github.com/okmich/signalfoundry-supervisor/internal/notify"
	"github.com/okmich/signalfoundry-supervisor/internal/proc"
	"github.com/okmich/signalfoundry-supervisor/internal/registry"
)

func TestParseTimeframe(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"M5", 5 * time.Minute, true},
		{"M15", 15 * time.Minute, true},
		{"H1", time.Hour, true},
		{"H4", 4 * time.Hour, true},
		{"D1", 24 * time.Hour, true},
		{"5", 5 * time.Minute, true}, // bare integer => minutes
		{"MN1", 0, false},            // monthly: not sized
		{"", 0, false},
		{"X9", 0, false},
	}
	for _, c := range cases {
		got, ok := parseTimeframe(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseTimeframe(%q) = (%v,%v), want (%v,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestIsFXWeekend(t *testing.T) {
	// 2026-01-01 is a Thursday, so 01-09 is Friday, 01-10 Saturday, 01-11 Sunday, 01-07 Wednesday.
	mk := func(day, hour int) time.Time { return time.Date(2026, 1, day, hour, 0, 0, 0, time.UTC) }
	cases := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"Wed noon", mk(7, 12), false},
		{"Fri 20:00", mk(9, 20), false},
		{"Fri 21:00", mk(9, 21), true},
		{"Sat 03:00", mk(10, 3), true},
		{"Sun 20:00", mk(11, 20), true},
		{"Sun 21:00", mk(11, 21), false},
	}
	for _, c := range cases {
		if got := isFXWeekend(c.t); got != c.want {
			t.Errorf("isFXWeekend(%s = %s) = %v, want %v", c.name, c.t.Weekday(), got, c.want)
		}
	}
}

func testEngine(t *testing.T) *engine {
	return &engine{
		cfg:         config.Config{WedgeMultiple: 3, WedgeGrace: time.Minute, StateDir: t.TempDir()}, // M5 threshold = 16m
		transitions: map[string]*transition{},
		wedged:      map[string]bool{},
		identities:  map[string]identity{},
		notifier:    notify.FromEnvFile(""), // no creds -> alert is a no-op
	}
}

func TestCheckLivenessWedged(t *testing.T) {
	e := testEngine(t)
	now := time.Date(2026, 1, 7, 12, 0, 0, 0, time.UTC) // Wednesday — weekend gate off
	set := ipc.Settings{WedgeAlert: ipc.WedgeAlertAlways, WedgeMultiple: 3, WedgeGraceS: 60}
	systems := []ipc.System{
		{SystemID: "stale/X/M5", State: ipc.StateRunning, PID: 1, Timeframe: "M5", LastBarTS: now.Add(-1 * time.Hour)},
		{SystemID: "fresh/Y/M5", State: ipc.StateRunning, PID: 2, Timeframe: "M5", LastBarTS: now.Add(-2 * time.Minute)},
		{SystemID: "stopped/Z/M5", State: ipc.StateStopped, Timeframe: "M5"},
		{SystemID: "nobar/W/M5", State: ipc.StateRunning, PID: 4, Timeframe: "M5"}, // LastBarTS zero -> not assessable
	}
	e.checkLiveness(systems, now, set)
	if !systems[0].Wedged {
		t.Errorf("stale system (60m > 16m threshold) should be wedged")
	}
	if systems[1].Wedged || systems[2].Wedged || systems[3].Wedged {
		t.Errorf("only the stale system should be wedged, got %+v", systems)
	}
	if !e.wedged["stale/X/M5"] {
		t.Errorf("engine should track stale/X/M5 as wedged")
	}

	// Recovery: a fresh bar clears the flag.
	systems[0].LastBarTS = now.Add(-1 * time.Minute)
	e.checkLiveness(systems, now, set)
	if systems[0].Wedged || e.wedged["stale/X/M5"] {
		t.Errorf("system should have recovered once its bar went fresh")
	}
}

// surface_only still SETS the wedged flag (the flag is always surfaced; only the page is gated).
func TestCheckLivenessSurfaceOnlyStillFlags(t *testing.T) {
	e := testEngine(t)
	now := time.Date(2026, 1, 7, 12, 0, 0, 0, time.UTC)
	set := ipc.Settings{WedgeAlert: ipc.WedgeAlertSurface, WedgeMultiple: 3, WedgeGraceS: 60}
	systems := []ipc.System{
		{SystemID: "stale/X/M5", State: ipc.StateRunning, PID: 1, Timeframe: "M5", LastBarTS: now.Add(-1 * time.Hour)},
	}
	e.checkLiveness(systems, now, set)
	if !systems[0].Wedged {
		t.Errorf("surface_only should still flag wedged (only the alert is suppressed)")
	}
}

// reconcileIdentities downgrades a Running row whose PID's create-time no longer matches the recorded
// baseline (the PID was recycled). Uses the test process's own PID, whose create-time is real and
// stable; skips where CreateTime is unavailable (non-Windows).
func TestReconcileIdentitiesDetectsReuse(t *testing.T) {
	e := testEngine(t)
	pid := os.Getpid()
	if _, ok := proc.CreateTime(pid); !ok {
		t.Skip("proc.CreateTime unsupported on this platform")
	}
	e.identities["sys/X/M5"] = identity{pid: pid, ctime: time.Unix(1, 0)} // stale (non-zero) baseline
	systems := []ipc.System{{SystemID: "sys/X/M5", State: ipc.StateRunning, PID: pid}}
	e.reconcileIdentities(systems)
	if systems[0].State != ipc.StateOrphanSuspected {
		t.Errorf("reused PID should be downgraded to OrphanSuspected, got %s", systems[0].State)
	}
}

func TestReconcileIdentitiesConsistent(t *testing.T) {
	e := testEngine(t)
	pid := os.Getpid()
	ct, ok := proc.CreateTime(pid)
	if !ok {
		t.Skip("proc.CreateTime unsupported on this platform")
	}
	e.identities["sys/X/M5"] = identity{pid: pid, ctime: ct} // correct baseline
	systems := []ipc.System{{SystemID: "sys/X/M5", State: ipc.StateRunning, PID: pid}}
	e.reconcileIdentities(systems)
	if systems[0].State != ipc.StateRunning {
		t.Errorf("matching create-time should stay Running, got %s", systems[0].State)
	}
}

// The identity baseline is persisted to the registry and re-loaded on (engine) restart, so a
// re-attached system stays Running rather than tripping a false orphan (§12).
func TestIdentityPersistsAndReattaches(t *testing.T) {
	pid := os.Getpid()
	if _, ok := proc.CreateTime(pid); !ok {
		t.Skip("proc.CreateTime unsupported on this platform")
	}
	dir := t.TempDir()
	newEngine := func() *engine {
		return &engine{
			cfg:         config.Config{StateDir: dir},
			transitions: map[string]*transition{},
			wedged:      map[string]bool{},
			identities:  map[string]identity{},
			notifier:    notify.FromEnvFile(""),
		}
	}
	systems := []ipc.System{{SystemID: "sys/X/M5", State: ipc.StateRunning, PID: pid, StartToken: "tok-1"}}

	e1 := newEngine()
	e1.reconcileIdentities(systems) // records + persists
	if reg, _ := registry.Load(e1.cfg.RegistryPath()); reg.Entries["sys/X/M5"].PID != pid {
		t.Fatalf("identity not persisted to the registry")
	}

	// "restart": a fresh engine loads the registry and re-attaches without a false orphan.
	e2 := newEngine()
	e2.loadIdentities()
	if e2.identities["sys/X/M5"].pid != pid {
		t.Fatalf("identity not re-loaded on restart")
	}
	systems[0].State = ipc.StateRunning // reset (snapshot is fresh each tick)
	e2.reconcileIdentities(systems)
	if systems[0].State != ipc.StateRunning {
		t.Errorf("re-attached system should stay Running (consistent create-time), got %s", systems[0].State)
	}
}

func TestGroupTerminals(t *testing.T) {
	systems := []ipc.System{
		{SystemID: "a/X/5", State: ipc.StateRunning, SessionID: "mt5-1", Account: "A1"},
		{SystemID: "b-multi", State: ipc.StateRunning, SessionID: "mt5-1", Account: "A1", Multi: true, Symbols: []string{"s1", "s2", "s3"}},
		{SystemID: "c/Y/5", State: ipc.StateRunning, SessionID: "mt5-2", Account: "A2"},
		{SystemID: "d/Z/5", State: ipc.StateStopped}, // no session -> not grouped, sorts last
	}
	terms := groupTerminals(systems)
	byID := map[string]ipc.Terminal{}
	for _, tm := range terms {
		byID[tm.BrokerSessionID] = tm
	}
	if len(terms) != 2 {
		t.Fatalf("got %d terminals, want 2 (the stopped system has no session)", len(terms))
	}
	if got := byID["mt5-1"].LogicalSystems; got != 4 { // 1 single + a 3-symbol multi
		t.Errorf("mt5-1 logical systems = %d, want 4 (a multi counts as len(symbols))", got)
	}
	if got := byID["mt5-2"].LogicalSystems; got != 1 {
		t.Errorf("mt5-2 logical systems = %d, want 1", got)
	}
	if systems[len(systems)-1].SystemID != "d/Z/5" {
		t.Errorf("the idle (no-session) system should sort last, ended with %s", systems[len(systems)-1].SystemID)
	}
}

func TestSettingsNormalized(t *testing.T) {
	def := ipc.DefaultSettings()
	got := ipc.Settings{WedgeAlert: "bogus", WedgeMultiple: 0, WedgeGraceS: -5}.Normalized(def)
	if got.WedgeAlert != def.WedgeAlert || got.WedgeMultiple != def.WedgeMultiple || got.WedgeGraceS != def.WedgeGraceS {
		t.Errorf("Normalized(%+v) did not fall back to defaults: %+v", def, got)
	}
	keep := ipc.Settings{WedgeAlert: ipc.WedgeAlertAlways, WedgeMultiple: 5, WedgeGraceS: 30}.Normalized(def)
	if keep.WedgeAlert != ipc.WedgeAlertAlways || keep.WedgeMultiple != 5 || keep.WedgeGraceS != 30 {
		t.Errorf("Normalized clobbered valid values: %+v", keep)
	}
}
