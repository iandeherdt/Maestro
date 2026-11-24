package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/dockerbox"
	"github.com/iandeherdt/Maestro/internal/launch"
	"github.com/iandeherdt/Maestro/internal/setup"
	"github.com/iandeherdt/Maestro/internal/systemdbox"
)

// The systemd sandbox on this machine (SPEC 11.2, 11.3): what setup
// installs as root, once, after showing it.
const (
	// agentUser runs every step. It has no login, no home and no sudo.
	agentUser = "maestro-agent"
	// sandboxRoot holds, per worker, the workspaces and the agent's Claude
	// state. Root owns it, so no user can put a link where the launcher
	// makes a folder.
	sandboxRoot = "/var/lib/maestro"
	// minStepCeilingMS is the least the launcher allows a step, whatever
	// the settings say today: they can be changed without root, the
	// launcher's file cannot.
	minStepCeilingMS = 4 * 60 * 60 * 1000
)

// systemdPlan is what the systemd sandbox needs, derived from the
// settings.
type systemdPlan struct {
	worker      string
	self        string // this maestro
	launcherSrc string // the maestro-launch that came with it
	cfg         launch.Config
	config      []byte // the launcher's configuration file
	sudoers     string
	claude      string // the Claude Code version the steps run
}

// currentUser is user.Current; tests, which may run as root, replace it.
var currentUser = user.Current

func sudoersPath(worker string) string { return "/etc/sudoers.d/maestro-" + worker }

// planSystemd works out the plan. It touches nothing.
func planSystemd(in *install) (*systemdPlan, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return nil, err
	}
	me, err := currentUser()
	if err != nil {
		return nil, err
	}
	w := in.cfg.Worker.Name
	sb := in.box.(*systemdbox.Box)
	abs := func(p string) string { a, _ := filepath.Abs(p); return a }
	cfg := launch.Config{
		Worker: w, User: agentUser, ServiceUser: me.Username,
		WorkspaceRoot: filepath.Join(sandboxRoot, w, "workspaces"),
		HandoffRoot:   abs(in.handoffDir), Mirror: abs(in.mirrorDir),
		ClaudeState: filepath.Join(sandboxRoot, w, "claude"),
		Skills:      abs(in.skills.SkillsDir()), Agents: abs(in.skills.AgentsDir()),
		ProxySockets: map[string]string{},
		BotName:      in.bot, BotEmail: in.botEmail(),
		MemoryMax: strings.ToUpper(in.cfg.Sandbox.MemoryMax), CPUQuota: in.cfg.Sandbox.CPUQuota,
		TasksMax: int(in.cfg.Sandbox.TasksMax), InnerSandbox: in.cfg.Claude.InnerSandbox,
		MaxTimeoutMS: max(minStepCeilingMS, in.cfg.Claude.TurnTotalTimeoutMS, in.cfg.Verify.TimeoutMS, in.cfg.Hooks.TimeoutMS),
	}
	for _, p := range systemdbox.Profiles {
		cfg.ProxySockets[p] = abs(sb.Socket(p))
	}
	// The steps see no home folder at all (ProtectHome); what lies
	// outside one is hidden by name.
	for _, p := range []string{in.dir, in.cfg.Budget.StateDir, in.cfg.Workspace.Root, filepath.Dir(in.path)} {
		if p = abs(p); p != "" && !underHome(p) {
			cfg.Hidden = append(cfg.Hidden, p)
		}
	}
	if err := cfg.Check(); err != nil {
		return nil, explain("the systemd sandbox cannot be set up: %v\n"+
			"The service runs as an ordinary user, not root, and the launcher takes only plain absolute\n"+
			"paths, without spaces or quotes.", err)
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	l := systemdbox.Launcher
	return &systemdPlan{
		worker: w, self: self, launcherSrc: filepath.Join(filepath.Dir(self), "maestro-launch"),
		cfg: cfg, config: append(raw, '\n'), claude: dockerbox.ClaudeVersion,
		sudoers: fmt.Sprintf("# Written by maestro setup. %s may start the sandbox's root launcher for worker %s.\n"+
			"%s ALL=(root) NOPASSWD: %s %s, %s %s login\n", me.Username, w, me.Username, l, w, l, w),
	}, nil
}

func underHome(p string) bool {
	for _, h := range []string{"/home", "/root", "/run/user"} {
		if p == h || strings.HasPrefix(p, h+"/") {
			return true
		}
	}
	return false
}

// problems lists what is not in place yet. The sudo rule is checked by
// asking sudo, because only root can read it.
func (p *systemdPlan) problems(ctx context.Context) []string {
	var out []string
	if _, err := os.Stat(p.launcherSrc); err != nil {
		out = append(out, "The launcher program maestro-launch is not next to maestro ("+p.launcherSrc+"). "+
			"Install Maestro again from a release, which brings it: curl -fsSL https://raw.githubusercontent.com/iandeherdt/Maestro/main/install.sh | sh")
	} else if !sameFile(p.launcherSrc, systemdbox.Launcher) {
		out = append(out, "The root launcher "+systemdbox.Launcher+" is missing or is not the one of this Maestro.")
	}
	if !sameFile(p.self, filepath.Join(launch.DefaultTools, "maestro")) {
		out = append(out, "The maestro the steps run for the self-test ("+launch.DefaultTools+"/maestro) is missing or older.")
	}
	if !claudeIs(ctx, filepath.Join(launch.DefaultTools, "claude"), p.claude) {
		out = append(out, "Claude Code "+p.claude+" for the steps ("+launch.DefaultTools+"/claude) is not installed.")
	}
	if have, err := os.ReadFile(launch.ConfigPath(p.worker)); err != nil || !bytes.Equal(have, p.config) {
		out = append(out, "The launcher's settings "+launch.ConfigPath(p.worker)+" are missing or out of date.")
	}
	if _, err := user.Lookup(agentUser); err != nil {
		out = append(out, "The sandbox user "+agentUser+" does not exist.")
	}
	for _, d := range []string{p.cfg.WorkspaceRoot, filepath.Dir(p.cfg.ClaudeState)} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			out = append(out, "The folder "+d+" does not exist.")
		}
	}
	if !sudoAllows(ctx, systemdbox.Launcher, p.worker) || !sudoAllows(ctx, systemdbox.Launcher, p.worker, "login") {
		out = append(out, "sudo does not let this user start the launcher without a password ("+sudoersPath(p.worker)+").")
	}
	return out
}

func sameFile(a, b string) bool {
	ha, err1 := fileSum(a)
	hb, err2 := fileSum(b)
	return err1 == nil && err2 == nil && ha == hb
}

func fileSum(p string) ([32]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return [32]byte{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return [32]byte{}, err
	}
	var s [32]byte
	copy(s[:], h.Sum(nil))
	return s, nil
}

func claudeIs(ctx context.Context, path, version string) bool {
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, path, "--version").Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), version)
}

func sudoAllows(ctx context.Context, cmd ...string) bool {
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return exec.CommandContext(c, systemdbox.Sudo, append([]string{"-n", "-l"}, cmd...)...).Run() == nil
}

// script is what root runs. Every path in it passed the launcher's path
// check, so none holds a quote, a space or a shell character; they are
// quoted anyway. configFile and sudoersFile are the prepared files.
func (p *systemdPlan) script(configFile, sudoersFile string) string {
	q := strconv.Quote
	w := p.worker
	claudeDir := "/opt/maestro/claude-" + p.claude
	lines := []string{
		"set -eu",
		"umask 022",
		"# The sandbox user: no login, no home, no sudo.",
		"id -u " + agentUser + " >/dev/null 2>&1 || useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /sbin/nologin " + agentUser,
		"# The programs the steps run: this maestro for the self-test, and Claude Code " + p.claude + ".",
		"install -d -o root -g root -m 0755 /opt/maestro " + launch.DefaultTools,
		"install -o root -g root -m 0755 " + q(p.self) + " " + launch.DefaultTools + "/maestro",
		"if ! " + launch.DefaultTools + "/claude --version 2>/dev/null | grep -q '^" + p.claude + "'; then",
		"  npm install --global --prefix " + claudeDir + " --no-fund --no-audit @anthropic-ai/claude-code@" + p.claude,
		"  ln -sfn " + claudeDir + "/bin/claude " + launch.DefaultTools + "/claude",
		"fi",
		"# The root launcher and its settings, which only root can change.",
		"install -d -o root -g root -m 0755 /usr/local/libexec /etc/maestro",
		"install -o root -g root -m 0755 " + q(p.launcherSrc) + " " + systemdbox.Launcher,
		"install -o root -g root -m 0644 " + q(configFile) + " " + launch.ConfigPath(w),
		"# The workspaces and the agent's Claude login.",
		"install -d -o root -g root -m 0755 " + sandboxRoot + " " + filepath.Join(sandboxRoot, w),
		"install -d -o root -g root -m 0711 " + p.cfg.WorkspaceRoot,
		"install -d -o " + agentUser + " -g " + agentUser + " -m 0700 " + p.cfg.ClaudeState,
		"# The one sudo rule: this user may start the launcher, nothing else.",
		"visudo -cqf " + q(sudoersFile),
		"install -o root -g root -m 0440 " + q(sudoersFile) + " " + sudoersPath(w),
	}
	return strings.Join(lines, "\n") + "\n"
}

// setupSystemd is the setup step for the systemd sandbox. It is done at
// once for a project that uses containers.
type setupSystemd struct{ setupDir string }

func (*setupSystemd) Title() string { return "Install the Linux sandbox" }

func (s *setupSystemd) plan(st *setup.State) (*systemdPlan, bool, error) {
	if st.Sandbox != setup.SandboxSystemd || st.WorkflowFile == "" {
		return nil, false, nil
	}
	in, err := open(s.setupDir, st.WorkflowFile, nil)
	if err != nil {
		return nil, true, err
	}
	if in.dbox != nil {
		return nil, false, nil
	}
	p, err := planSystemd(in)
	return p, true, err
}

func (s *setupSystemd) Check(ctx context.Context, st *setup.State) (setup.Status, error) {
	p, used, err := s.plan(st)
	switch {
	case !used:
		return setup.Status{Done: true, Detail: "Not used: the steps run in containers."}, nil
	case err != nil:
		return setup.Status{Detail: errText(err)}, nil
	}
	if probs := p.problems(ctx); len(probs) > 0 {
		return setup.Status{Detail: strings.Join(probs, " ")}, nil
	}
	return setup.Status{Done: true, Detail: "The launcher, the sandbox user " + agentUser + ", Claude Code " + p.claude + " and the sudo rule are in place."}, nil
}

func (s *setupSystemd) Run(ctx context.Context, ui *setup.UI, st *setup.State) error {
	p, _, err := s.plan(st)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.launcherSrc); err != nil {
		return explain("%s", p.problems(ctx)[0])
	}
	tmp, err := os.MkdirTemp("", "maestro-setup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	configFile, sudoersFile := filepath.Join(tmp, "launch.json"), filepath.Join(tmp, "sudoers")
	if err := os.WriteFile(configFile, p.config, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(sudoersFile, []byte(p.sudoers), 0o644); err != nil {
		return err
	}
	script := p.script(configFile, sudoersFile)
	ui.Say("  Each step runs as the user %s in a systemd unit with no network of its own. A small", agentUser)
	ui.Say("  launcher, which only root can change, starts those units. Setting it up needs root once.")
	ui.Say("  These commands will run with sudo:")
	ui.Say("")
	for _, line := range strings.Split(strings.TrimRight(script, "\n"), "\n") {
		ui.Show("    %s", line)
	}
	ui.Say("")
	ui.Say("  The launcher's settings (%s):", launch.ConfigPath(p.worker))
	for _, line := range strings.Split(strings.TrimRight(string(p.config), "\n"), "\n") {
		ui.Show("    %s", line)
	}
	ui.Say("  The sudo rule (%s):", sudoersPath(p.worker))
	for _, line := range strings.Split(strings.TrimRight(p.sudoers, "\n"), "\n") {
		ui.Show("    %s", line)
	}
	ui.Say("")
	ok, err := ui.Confirm("  Run them with sudo? sudo asks for your password")
	if err != nil {
		return err
	}
	if !ok {
		return setup.ErrAborted
	}
	cmd := exec.CommandContext(ctx, systemdbox.Sudo, "/bin/sh", "-c", script)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, ui.Out, ui.Out
	if err := cmd.Run(); err != nil {
		return explain("the commands stopped: %v. Nothing after the failing line ran; run `maestro setup` again once it is fixed.", err)
	}
	return nil
}
