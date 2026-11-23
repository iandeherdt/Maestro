// Package launch is the root launcher of the systemd backend (SPEC 11.2,
// 11.3). It turns a request from the service into one transient systemd
// unit that runs as the sandbox user. The service names a step and a
// workspace; every path, the user and every unit property come from the
// launcher's own configuration, which only root can change.
//
// This is the only code in Maestro that runs as root. It uses the standard
// library and the request types and rules of package sandbox, nothing else.
package launch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/iandeherdt/Maestro/internal/sandbox"
)

// Config is the launcher's configuration, one file per worker, written by
// root at setup.
type Config struct {
	// Worker names the units: maestro-<worker>-...
	Worker string `json:"worker"`
	// User runs every step. It has no sudo rule and no login.
	User string `json:"user"`
	// ServiceUser is the user the service runs as. The export bundle is
	// handed to it.
	ServiceUser string `json:"service_user"`

	// WorkspaceRoot holds one folder per workspace, named by its key.
	WorkspaceRoot string `json:"workspace_root"`
	// HandoffRoot is where the export step leaves its bundle,
	// <key>.bundle, for the service.
	HandoffRoot string `json:"handoff_root"`
	// Mirror is the local copy of the repository, read-only in the unit.
	Mirror string `json:"mirror"`
	// ClaudeState is the agent's Claude Code folder, seen by agent steps
	// only.
	ClaudeState string `json:"claude_state"`
	// Skills and Agents are folders bound read-only into it, when set.
	Skills string `json:"skills,omitempty"`
	Agents string `json:"agents,omitempty"`
	// UnityLicence is bound read-only into hook and verify steps, when
	// set.
	UnityLicence string `json:"unity_licence,omitempty"`
	// Hidden are paths a step must not see at all: the service's state,
	// logs, settings and keys.
	Hidden []string `json:"hidden"`

	// ProxySockets maps a network profile (agent, hooks) to the unix
	// socket its proxy listens on. A step has no network of its own; with
	// a profile, a relay inside the unit forwards one loopback port to
	// that socket.
	ProxySockets map[string]string `json:"proxy_sockets"`

	// BotName and BotEmail are the commit identity of the workspace.
	BotName  string `json:"bot_name"`
	BotEmail string `json:"bot_email"`

	// Limits of every unit.
	MemoryMax string `json:"memory_max"` // such as 4G
	CPUQuota  string `json:"cpu_quota"`  // such as 200%
	TasksMax  int    `json:"tasks_max"`
	// MaxTimeoutMS caps the wall clock of a step; a request may ask for
	// less.
	MaxTimeoutMS int64 `json:"max_timeout_ms"`
	// InnerSandbox leaves namespaces allowed, for Claude Code's own
	// sandbox inside the agent step (claude.inner_sandbox).
	InnerSandbox bool `json:"inner_sandbox,omitempty"`

	// Tools holds the programs the steps may use besides the system's:
	// claude (Claude Code) and maestro, for the self-test's probe. It is
	// first on each step's PATH. Steps cannot see any home folder, so
	// these are installed root-owned outside them. Default /opt/maestro/bin.
	Tools string `json:"tools,omitempty"`

	// Relay is this program's path, run inside a unit with network to
	// forward the proxy port. Default: the launcher's own path.
	Relay string `json:"relay,omitempty"`
}

var (
	userRE  = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	identRE = regexp.MustCompile(`^[^\x00-\x1f"\\]{1,100}$`)
	memRE   = regexp.MustCompile(`^[1-9][0-9]{0,5}[KMG]$`)
	cpuRE   = regexp.MustCompile(`^[1-9][0-9]{0,4}%$`)
)

// pathOK accepts an absolute, clean path that systemd can take as a unit
// property value.
func pathOK(p string) bool {
	return filepath.IsAbs(p) && filepath.Clean(p) == p && p != "/" && !strings.ContainsAny(p, " \t\n\r\\\"'%$:;,=") &&
		!strings.ContainsRune(p, 0)
}

// Check validates the configuration without looking at the machine.
func (c *Config) Check() error {
	switch {
	case !sandbox.ValidWorker(c.Worker):
		return errors.New("worker: not a valid name")
	case !userRE.MatchString(c.User) || c.User == "root":
		return errors.New("user: not a valid user name")
	case !userRE.MatchString(c.ServiceUser) || c.ServiceUser == "root" || c.ServiceUser == c.User:
		return errors.New("service_user: must be another ordinary user")
	case !identRE.MatchString(c.BotName) || !identRE.MatchString(c.BotEmail):
		return errors.New("bot_name and bot_email are required")
	case c.MemoryMax != "" && !memRE.MatchString(c.MemoryMax):
		return errors.New("memory_max: such as 4G")
	case c.CPUQuota != "" && !cpuRE.MatchString(c.CPUQuota):
		return errors.New("cpu_quota: such as 200%")
	case c.TasksMax < 0 || c.TasksMax > 1<<20:
		return errors.New("tasks_max: out of range")
	case c.MaxTimeoutMS <= 0:
		return errors.New("max_timeout_ms is required")
	}
	paths := map[string]string{"workspace_root": c.WorkspaceRoot, "handoff_root": c.HandoffRoot,
		"mirror": c.Mirror, "claude_state": c.ClaudeState}
	for k, p := range map[string]string{"skills": c.Skills, "agents": c.Agents, "unity_licence": c.UnityLicence, "relay": c.Relay, "tools": c.Tools} {
		if p != "" {
			paths[k] = p
		}
	}
	for i, h := range c.Hidden {
		paths[fmt.Sprintf("hidden[%d]", i)] = h
	}
	for k, s := range c.ProxySockets {
		if k != sandbox.NetAgent && k != sandbox.NetHooks {
			return fmt.Errorf("proxy_sockets: unknown profile %q", k)
		}
		paths["proxy_sockets."+k] = s
	}
	for k, p := range paths {
		if !pathOK(p) {
			return fmt.Errorf("%s: %q is not an absolute, plain path", k, p)
		}
	}
	return nil
}

// Load reads the configuration from path. The file and every folder above
// it must belong to root and be writable by root only: whoever can change
// it decides what root runs.
func Load(path string) (*Config, error) {
	if err := ownerCheck(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var c Config
	dec := json.NewDecoder(io.LimitReader(f, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	if err := c.Check(); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	return &c, nil
}

// ownerCheck is rootOwned; tests, which do not run as root, replace it.
var ownerCheck = rootOwned

// rootOwned checks path and every folder above it.
func rootOwned(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("the configuration path must be absolute")
	}
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		switch {
		case !ok:
			return fmt.Errorf("%s: cannot read its owner", p)
		case fi.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("%s is a symbolic link", p)
		case st.Uid != 0:
			return fmt.Errorf("%s does not belong to root", p)
		case fi.Mode().Perm()&0o022 != 0:
			return fmt.Errorf("%s can be changed by users other than root", p)
		}
		if p == "/" {
			return nil
		}
	}
}

// DefaultTools is where setup installs the steps' own programs.
const DefaultTools = "/opt/maestro/bin"

func (c *Config) tools() string {
	if c.Tools != "" {
		return c.Tools
	}
	return DefaultTools
}

// ConfigPath is where the configuration of a worker lives.
func ConfigPath(worker string) string { return "/etc/maestro/launch-" + worker + ".json" }
