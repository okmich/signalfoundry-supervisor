// Command supervisor is the per-box process control plane: one binary, three faces
// (engine | tui | ctrlc). See FLEET_SUPERVISOR_SPEC.md.
package main

import (
	"fmt"
	"os"

	"github.com/okmich/signalfoundry-supervisor/internal/config"
	"github.com/okmich/signalfoundry-supervisor/internal/engine"
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
	case "version":
		fmt.Println("supervisor", engine.Version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: supervisor <engine|tui|ctrlc|version>")
}
