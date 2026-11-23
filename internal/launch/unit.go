package launch

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/iandeherdt/Maestro/internal/sandbox"
)

// Where things are inside a unit. The populate and export scripts expect
// these paths.
const (
	workDir    = "/workspace"
	repoDir    = "/workspace/repo"
	mirrorDir  = "/mirror"
	handoffDir = "/handoff"
	claudeDir  = "/claude"
	licenceDir = "/unity-licence"
	// proxySocket is where the profile's proxy socket appears in a unit;
	// proxyAddr is the loopback address the relay serves it on.
	proxySocket = "/run/maestro-proxy.sock"
	proxyAddr   = "127.0.0.1:3128"
)

// alwaysHidden are sockets of container engines and of other users'
// sessions. The sandbox user has no right to use them anyway; hiding them
// keeps it that way if a permission is ever wrong.
var alwaysHidden = []string{"/run/docker.sock", "/var/run/docker.sock", "/run/podman", "/run/containerd", "/run/user"}

// unit is one step as systemd will run it.
type unit struct {
	name    string
	props   []string // Key=Value, in order
	env     map[string]string
	argv    []string // run inside the unit by the "inside" helper
	network string   // "", agent or hooks
	pty     bool     // a terminal instead of pipes, for the login
}

func unitToken() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// spec is what a step needs besides its command.
type spec struct {
	step    sandbox.Step
	key     string
	network string
	mirror  bool // the mirror, read-only
	claude  bool // the agent's Claude state
	handoff bool // the hand-off folder of the workspace
	licence bool // the Unity licence, read-only
	workdir string
	timeout int64 // milliseconds
	// noWorkspace: the step has no workspace (the login and its status).
	noWorkspace bool
	pty         bool
}

// workspace is the host folder of a workspace.
func (c *Config) workspace(key string) string { return filepath.Join(c.WorkspaceRoot, key) }

// handoff is the folder the export step writes in, and bundle the file the
// service picks up.
func (c *Config) handoff(key string) string { return filepath.Join(c.HandoffRoot, key) }
func (c *Config) bundle(key string) string  { return filepath.Join(c.HandoffRoot, key+".bundle") }

// baseEnv is the whole environment of a step. Nothing is inherited.
func baseEnv(network, tools string) map[string]string {
	env := map[string]string{
		"HOME": "/tmp", "PATH": tools + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG": "C.UTF-8", "TZ": "UTC", "CI": "1", "TERM": "dumb", "NO_PROXY": "",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
	}
	if network != "" {
		proxy := "http://" + proxyAddr
		env["HTTPS_PROXY"], env["HTTP_PROXY"] = proxy, proxy
		env["https_proxy"], env["http_proxy"] = proxy, proxy
	}
	return env
}

// build turns a spec into a unit. Every path and property comes from the
// configuration; the request contributed only checked names.
func (c *Config) build(s spec, argv []string, extra map[string]string) (*unit, error) {
	name := "maestro-" + c.Worker + "-"
	switch {
	case s.noWorkspace && s.key == "":
	case !s.noWorkspace && sandbox.ValidKey(s.key):
		name += strings.ToLower(s.key) + "-"
	default:
		return nil, errors.New("invalid workspace key")
	}
	u := &unit{
		name:    name + string(s.step) + "-" + unitToken(),
		env:     baseEnv(s.network, c.tools()),
		argv:    argv,
		network: s.network,
		pty:     s.pty,
	}
	for k, v := range extra {
		if !sandbox.AllowedEnv(k, v) {
			return nil, errors.New("environment variable " + strconv.Quote(k) + " is not allowed in the sandbox")
		}
		if _, fixed := u.env[k]; fixed {
			return nil, errors.New("environment variable " + strconv.Quote(k) + " is set by the sandbox")
		}
		u.env[k] = v
	}
	p := func(kv ...string) { u.props = append(u.props, kv...) }
	p("User="+c.User, "Group="+c.User,
		"NoNewPrivileges=yes", "CapabilityBoundingSet=", "AmbientCapabilities=",
		"ProtectSystem=strict", "ProtectHome=tmpfs",
		"PrivateTmp=yes", "PrivateDevices=yes", "PrivateUsers=no",
		// No network of its own: only loopback, inside the unit.
		"PrivateNetwork=yes", "IPAddressDeny=any",
		"ProtectKernelTunables=yes", "ProtectKernelModules=yes", "ProtectKernelLogs=yes",
		"ProtectControlGroups=yes", "ProtectClock=yes", "ProtectHostname=yes",
		"RestrictSUIDSGID=yes", "LockPersonality=yes", "RestrictRealtime=yes",
		"RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
		"SystemCallFilter=@system-service", "SystemCallArchitectures=native",
		"UMask=0077", "KillMode=control-group", "OOMPolicy=kill",
		"MemorySwapMax=0",
	)
	if !(s.claude && c.InnerSandbox) {
		p("RestrictNamespaces=yes")
	}
	if c.MemoryMax != "" {
		p("MemoryMax=" + c.MemoryMax)
	}
	if c.CPUQuota != "" {
		p("CPUQuota=" + c.CPUQuota)
	}
	if c.TasksMax > 0 {
		p("TasksMax=" + strconv.Itoa(c.TasksMax))
	}
	timeout := c.MaxTimeoutMS
	if s.timeout > 0 && s.timeout < timeout {
		timeout = s.timeout
	}
	// Whole seconds, rounded up, so a short limit is never zero.
	p("RuntimeMaxSec=" + strconv.FormatInt((timeout+999)/1000, 10))

	if !s.noWorkspace {
		p("BindPaths=" + c.workspace(s.key) + ":" + workDir)
	}
	if s.mirror {
		p("BindReadOnlyPaths=" + c.Mirror + ":" + mirrorDir)
	}
	if s.handoff {
		p("BindPaths=" + c.handoff(s.key) + ":" + handoffDir)
	}
	if s.claude {
		p("BindPaths=" + c.ClaudeState + ":" + claudeDir)
		if c.Skills != "" {
			p("BindReadOnlyPaths=-" + c.Skills + ":" + claudeDir + "/skills")
		}
		if c.Agents != "" {
			p("BindReadOnlyPaths=-" + c.Agents + ":" + claudeDir + "/agents")
		}
		u.env["CLAUDE_CONFIG_DIR"] = claudeDir
	}
	if s.licence && c.UnityLicence != "" {
		p("BindReadOnlyPaths=-" + c.UnityLicence + ":" + licenceDir)
	}
	if s.network != "" {
		sock := c.ProxySockets[s.network]
		if sock == "" {
			return nil, errors.New("no proxy is configured for the network profile " + s.network)
		}
		p("BindReadOnlyPaths=" + sock + ":" + proxySocket)
	}
	hidden := append(append([]string{}, c.Hidden...), alwaysHidden...)
	sort.Strings(hidden)
	for _, h := range hidden {
		// "-": a path that does not exist is not an error.
		p("InaccessiblePaths=-" + h)
	}
	p("WorkingDirectory=" + s.workdir)
	return u, nil
}

// args is the systemd-run command line for the unit. The command inside
// is always this program's "inside" helper, which starts the relay when
// the step has network, then runs argv.
func (u *unit) args(self string) []string {
	stdio := "--pipe"
	if u.pty {
		stdio = "--pty"
	}
	a := []string{"--wait", "--collect", stdio, "--quiet", "--service-type=exec", "--unit=" + u.name}
	for _, kv := range u.props {
		a = append(a, "--property="+kv)
	}
	keys := make([]string, 0, len(u.env))
	for k := range u.env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a = append(a, "--setenv="+k+"="+u.env[k])
	}
	a = append(a, "--", self, "inside")
	if u.network != "" {
		a = append(a, "--proxy", proxySocket)
	}
	a = append(a, "--")
	return append(a, u.argv...)
}
