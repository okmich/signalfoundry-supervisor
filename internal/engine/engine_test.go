package engine

import (
	"os"
	"testing"
	"time"

	"github.com/okmich/signalfoundry-supervisor/internal/config"
	"github.com/okmich/signalfoundry-supervisor/internal/ipc"
	"github.com/okmich/signalfoundry-supervisor/internal/notify"
	"github.com/okmich/signalfoundry-supervisor/internal/proc"
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

func testEngine() *engine {
	return &engine{
		cfg:         config.Config{WedgeMultiple: 3, WedgeGrace: time.Minute}, // M5 threshold = 16m
		transitions: map[string]*transition{},
		wedged:      map[string]bool{},
		identities:  map[string]identity{},
		notifier:    notify.FromEnvFile(""), // no creds -> alert is a no-op
	}
}

func TestCheckLivenessWedged(t *testing.T) {
	e := testEngine()
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
	e := testEngine()
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

// confirmIdentities downgrades a Running row whose PID's create-time no longer matches the recorded
// baseline (the PID was recycled). Uses the test process's own PID, whose create-time is real and
// stable; skips where CreateTime is unavailable (non-Windows).
func TestConfirmIdentitiesDetectsReuse(t *testing.T) {
	e := testEngine()
	pid := os.Getpid()
	if _, ok := proc.CreateTime(pid); !ok {
		t.Skip("proc.CreateTime unsupported on this platform")
	}
	e.identities["sys/X/M5"] = identity{pid: pid, ctime: time.Unix(1, 0)} // stale baseline
	systems := []ipc.System{{SystemID: "sys/X/M5", State: ipc.StateRunning, PID: pid}}
	e.confirmIdentities(systems)
	if systems[0].State != ipc.StateOrphanSuspected {
		t.Errorf("reused PID should be downgraded to OrphanSuspected, got %s", systems[0].State)
	}
}

func TestConfirmIdentitiesConsistent(t *testing.T) {
	e := testEngine()
	pid := os.Getpid()
	ct, ok := proc.CreateTime(pid)
	if !ok {
		t.Skip("proc.CreateTime unsupported on this platform")
	}
	e.identities["sys/X/M5"] = identity{pid: pid, ctime: ct} // correct baseline
	systems := []ipc.System{{SystemID: "sys/X/M5", State: ipc.StateRunning, PID: pid}}
	e.confirmIdentities(systems)
	if systems[0].State != ipc.StateRunning {
		t.Errorf("matching create-time should stay Running, got %s", systems[0].State)
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
