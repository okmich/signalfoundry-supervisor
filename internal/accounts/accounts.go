// Package accounts knows the box's broker accounts. An account is named by its broker env-file stem,
// <broker>.<env> (fxify.demo, deriv.live): one env file = one terminal + one login = one account folder,
// the first level under both LIVE_BASE and LOG_BASE (ACCOUNT_LAYOUT_CHANGE_PLAN, LOGGING_CONTRACT §10).
//
// The env files hold credentials. This package reads ONLY the identity keys the supervisor needs to
// label and check an account (LOGIN_ID, LOGIN_SERVER, BROKER_NAME, TERMINAL_PATH) and never retains or
// exposes anything else.
package accounts

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// EnvVar is the per-process variable the engine injects at spawn so a runner knows its account.
const EnvVar = "OKMICH_QUANT_ACCOUNT"

// AdminFolder is the Account Admin's folder inside an account, <live_base>/<account>/_account_admin: its code
// and config, and the governance files it writes there (ACCOUNT_ADMIN_SPEC §3). It is an ordinary runner to
// the supervisor, except that its governance must never be undone as a side effect: see AdminRuntime.
const AdminFolder = "_account_admin"

// AdminRuntime are the files the Admin writes into its own folder at run time. An import never takes them
// from the source and always carries the current ones into the new copy; losing them would leave the
// account ungoverned (no directive) or forget tripped latches (no state).
var AdminRuntime = []string{"directive.json", "state.json", "writer.lock", "requests"}

// namePattern mirrors okmich_quant_core.account._ACCOUNT_PATTERN: lower-case snake tokens, one dot.
var namePattern = regexp.MustCompile(`^[a-z0-9_]+\.[a-z0-9_]+$`)

// Valid reports whether name is an account name (<broker>.<env>). Anything else under LIVE_BASE is not
// an account folder.
func Valid(name string) bool { return namePattern.MatchString(name) }

// Info is what the supervisor knows about one account from its env file.
type Info struct {
	Name         string
	EnvFile      string // <env_dir>/.env.<name>
	EnvFound     bool
	Login        string // LOGIN_ID
	Server       string // LOGIN_SERVER
	Broker       string // BROKER_NAME
	TerminalPath string // TERMINAL_PATH
}

// EnvPath is <envDir>/.env.<name>.
func EnvPath(envDir, name string) string { return filepath.Join(envDir, ".env."+name) }

// Load reads an account's identity keys from its env file. A missing or unreadable file yields
// EnvFound=false and empty identity; it is never an error, so a stray account folder degrades to a
// flagged row instead of breaking discovery.
func Load(envDir, name string) Info {
	info := Info{Name: name, EnvFile: EnvPath(envDir, name)}
	f, err := os.Open(info.EnvFile)
	if err != nil {
		return info
	}
	defer f.Close()
	info.EnvFound = true
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := parseLine(sc.Text())
		if !ok {
			continue
		}
		switch key {
		case "LOGIN_ID":
			info.Login = val
		case "LOGIN_SERVER":
			info.Server = val
		case "BROKER_NAME":
			info.Broker = val
		case "TERMINAL_PATH":
			info.TerminalPath = val
		}
	}
	return info
}

// List returns the account names that have an env file in envDir (.env.<broker>.<env>), sorted.
func List(envDir string) []string {
	entries, err := os.ReadDir(envDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), ".env.") {
			continue
		}
		if name := strings.TrimPrefix(e.Name(), ".env."); Valid(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Cache memoizes Load for one reconcile pass, so an account's env file is read once per tick however
// many systems it holds.
type Cache struct {
	envDir string
	m      map[string]Info
}

func NewCache(envDir string) *Cache { return &Cache{envDir: envDir, m: map[string]Info{}} }

func (c *Cache) Get(name string) Info {
	if info, ok := c.m[name]; ok {
		return info
	}
	info := Load(c.envDir, name)
	c.m[name] = info
	return info
}

// parseLine parses one dotenv line (KEY=VALUE, optional `export `, optional matching quotes, # comments).
func parseLine(line string) (key, val string, ok bool) {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, "#") {
		return "", "", false
	}
	s = strings.TrimPrefix(s, "export ")
	key, val, ok = strings.Cut(s, "=")
	if !ok {
		return "", "", false
	}
	key, val = strings.TrimSpace(key), strings.TrimSpace(val)
	if n := len(val); n >= 2 && (val[0] == '"' || val[0] == '\'') && val[n-1] == val[0] {
		val = val[1 : n-1]
	} else if i := strings.Index(val, " #"); i >= 0 {
		val = strings.TrimSpace(val[:i])
	}
	return key, val, true
}
