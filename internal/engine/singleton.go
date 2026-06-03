package engine

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/okmich/signalfoundry-supervisor/internal/proc"
)

// singleton enforces one engine per deployment. The hard guarantee is a named mutex (Windows), which
// the OS frees on process exit so it never goes stale and which closes the pidfile's check-then-write
// TOCTOU race. The pidfile carries the holder's PID for observability and is the best-effort guard
// where the mutex is a no-op (non-Windows dev).
type singleton struct {
	path   string
	handle uintptr
}

func acquireSingleton(stateDir string) (*singleton, error) {
	h, err := acquireMutex(mutexName(stateDir))
	if err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, "engine.pid")
	if h == 0 { // no mutex (non-Windows): fall back to a pidfile staleness check
		if b, rerr := os.ReadFile(path); rerr == nil {
			if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 0 && proc.Alive(pid) {
				return nil, fmt.Errorf("an engine is already running (pid %d) — refusing to start a second", pid)
			}
		}
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		releaseMutex(h)
		return nil, err
	}
	return &singleton{path: path, handle: h}, nil
}

func (s *singleton) release() {
	releaseMutex(s.handle)
	_ = os.Remove(s.path)
}

// mutexName is deployment-specific (derived from the state dir) so independent deployments
// (paper / live / experimental) on one box never block each other.
func mutexName(stateDir string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(filepath.Clean(stateDir)))
	return fmt.Sprintf("okmich-quant-supervisor-engine-%08x", h.Sum32())
}
