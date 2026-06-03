package ipc

import (
	"encoding/json"
	"os"
)

// WedgeAlertMode controls WHEN a wedged system raises a Telegram alert. The wedged flag itself is
// always surfaced in the fleet view; this only gates the page (FLEET_SUPERVISOR_SPEC §15).
type WedgeAlertMode string

const (
	WedgeAlertAlways  WedgeAlertMode = "always"       // page regardless of clock (correct for 24/7 instruments)
	WedgeAlertWeekend WedgeAlertMode = "weekend"      // page except the FX weekend window (Fri 21:00–Sun 21:00 UTC)
	WedgeAlertSurface WedgeAlertMode = "surface_only" // never page; only show [WEDGED] in the view
)

// Settings is the operator-tunable runtime policy, persisted at StateDir/settings.json. The engine
// live-reads it each tick (changes apply with no restart); the TUI settings screen edits it. When
// the file is absent the engine seeds it from its env-derived defaults.
type Settings struct {
	WedgeAlert    WedgeAlertMode `json:"wedge_alert"`    // when a wedge pages
	WedgeMultiple int            `json:"wedge_multiple"` // missed bar intervals before wedged
	WedgeGraceS   int            `json:"wedge_grace_s"`  // slack seconds added to N×timeframe
}

// DefaultSettings is the baseline used when no file exists and no other default is supplied.
func DefaultSettings() Settings {
	return Settings{WedgeAlert: WedgeAlertWeekend, WedgeMultiple: 3, WedgeGraceS: 60}
}

// Normalized fills invalid/zero fields from def and coerces an unknown mode, so a hand-edited or
// partial settings.json can never put the engine into a nonsense state.
func (s Settings) Normalized(def Settings) Settings {
	switch s.WedgeAlert {
	case WedgeAlertAlways, WedgeAlertWeekend, WedgeAlertSurface:
	default:
		s.WedgeAlert = def.WedgeAlert
	}
	if s.WedgeMultiple <= 0 {
		s.WedgeMultiple = def.WedgeMultiple
	}
	if s.WedgeGraceS < 0 {
		s.WedgeGraceS = def.WedgeGraceS
	}
	return s
}

// ReadSettings loads StateDir/settings.json.
func ReadSettings(path string) (Settings, error) {
	var s Settings
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// WriteSettings atomically persists settings (tmp -> rename), so a reader never sees a torn file.
func WriteSettings(path string, s Settings) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
