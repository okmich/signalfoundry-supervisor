// Command supervisor is the per-box process control plane: one binary, three faces
// (engine | tui | ctrlc), plus the one-off migrate-layout cutover. See FLEET_SUPERVISOR_SPEC.md.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/okmich/signalfoundry-supervisor/internal/config"
	"github.com/okmich/signalfoundry-supervisor/internal/engine"
	"github.com/okmich/signalfoundry-supervisor/internal/migrate"
	"github.com/okmich/signalfoundry-supervisor/internal/proc"
	"github.com/okmich/signalfoundry-supervisor/internal/tui"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "engine":
		if err := engine.Run(config.MustLoad()); err != nil {
			fmt.Fprintln(os.Stderr, "engine:", err)
			os.Exit(1)
		}
	case "tui":
		if err := tui.Run(config.MustLoad()); err != nil {
			fmt.Fprintln(os.Stderr, "tui:", err)
			os.Exit(1)
		}
	case "ctrlc": // internal helper, invoked by the engine per stop
		os.Exit(proc.CtrlCMain(os.Args[2:]))
	case "migrate-layout": // one-off: flat LIVE_BASE/LOG_BASE -> <base>/<account>/... (dry run unless --apply)
		os.Exit(migrateLayout(os.Args[2:]))
	case "version":
		fmt.Println("supervisor", engine.Version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: supervisor <engine|tui|ctrlc|migrate-layout|version>")
}

// mapFlag collects repeated --map name=account pairs.
type mapFlag map[string]string

func (m mapFlag) String() string { return fmt.Sprint(map[string]string(m)) }
func (m mapFlag) Set(v string) error {
	name, acct, ok := strings.Cut(v, "=")
	if !ok || name == "" || acct == "" {
		return fmt.Errorf("want name=account, got %q", v)
	}
	m[name] = acct
	return nil
}

// migrateLayout prints the migration plan and, with --apply, performs it.
func migrateLayout(args []string) int {
	fs := flag.NewFlagSet("migrate-layout", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "perform the moves (default: print the plan only)")
	overrides := mapFlag{}
	fs.Var(overrides, "map", "name=account: place a flat folder in an account (repeatable; overrides run.py)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := config.MustLoad()
	plan, err := migrate.BuildPlan(cfg, overrides)
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate-layout:", err)
		return 1
	}
	plan.Print(func(format string, a ...any) { fmt.Printf(format, a...) })
	if !*apply {
		fmt.Println("\nDry run: nothing moved. Re-run with --apply to perform these moves.")
		return 0
	}
	if err := plan.Apply(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "\nmigrate-layout:", err)
		return 1
	}
	fmt.Printf("\nDone: %d move(s) applied.\n", len(plan.Moves))
	return 0
}
