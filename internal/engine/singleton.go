package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/okmich/signalfoundry-supervisor/internal/proc"
)

// singleton is a coarse one-engine-per-box guard via a pidfile under StateDir.
// TODO: upgrade to a Windows named mutex for a hard cross-session guarantee.
type singleton struct{ path string }

func acquireSingleton(stateDir string) (*singleton, error) {
	path := filepath.Join(stateDir, "engine.pid")
	if b, err := os.ReadFile(path); err == nil {
		if pid, _ := strconv.Atoi(string(b)); pid > 0 && proc.Alive(pid) {
			return nil, fmt.Errorf("an engine is already running (pid %d) — refusing to start a second", pid)
		}
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return nil, err
	}
	return &singleton{path: path}, nil
}

func (s *singleton) release() { _ = os.Remove(s.path) }
