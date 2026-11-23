package main

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/launch"
	"github.com/iandeherdt/Maestro/internal/setup"
)

func systemdInstall(t *testing.T) *install {
	t.Helper()
	dir := t.TempDir()
	os.Chmod(dir, 0o700)
	path := filepath.Join(dir, "WORKFLOW.md")
	os.WriteFile(path, []byte("---\ntracker:\n  kind: github\n  allowed_authors: [ian]\nworkspace:\n  root: "+dir+"/ws\n  repo: ian/app\n"+
		"sandbox:\n  backend: systemd\n  memory_max: 6G\n  cpu_quota: 300%\nverify:\n  command: 'true'\nbudget:\n  state_dir: "+dir+"/state\nworker:\n  name: linux\n---\n"), 0o600)
	st, _ := setup.LoadState(dir)
	st.Repo, st.AppID, st.AppSlug, st.AppKeyFile, st.InstallationID = "ian/app", "1", "sym", filepath.Join(dir, "k.pem"), "2"
	st.Save()
	in, err := open(dir, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func TestSystemdPlan(t *testing.T) {
	was := currentUser
	currentUser = func() (*user.User, error) { return &user.User{Username: "ian"}, nil }
	defer func() { currentUser = was }()
	in := systemdInstall(t)
	p, err := planSystemd(in)
	if err != nil {
		t.Fatal(err)
	}
	c := p.cfg
	if c.Worker != "linux" || c.User != agentUser || c.WorkspaceRoot != "/var/lib/maestro/linux/workspaces" ||
		c.ClaudeState != "/var/lib/maestro/linux/claude" || c.Mirror != in.mirrorDir || c.HandoffRoot != in.handoffDir ||
		c.ProxySockets["agent"] != filepath.Join(in.cfg.Budget.StateDir, "proxy-sockets", "agent.sock") ||
		c.MemoryMax != "6G" || c.CPUQuota != "300%" || c.BotName != "sym[bot]" || c.MaxTimeoutMS < minStepCeilingMS {
		t.Errorf("config %+v", c)
	}
	// The test folders are not under /home, so they are hidden by name.
	if !slices.Contains(c.Hidden, in.cfg.Budget.StateDir) || !slices.Contains(c.Hidden, in.dir) {
		t.Errorf("hidden %v", c.Hidden)
	}
	var back launch.Config
	if err := json.Unmarshal(p.config, &back); err != nil || back.Check() != nil {
		t.Errorf("the file does not load: %v", err)
	}
	if !strings.Contains(p.sudoers, " ALL=(root) NOPASSWD: /usr/local/libexec/maestro-launch linux, /usr/local/libexec/maestro-launch linux login\n") {
		t.Errorf("sudoers %q", p.sudoers)
	}
	s := p.script("/tmp/x/launch.json", "/tmp/x/sudoers")
	for _, want := range []string{
		"set -eu",
		"useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /sbin/nologin maestro-agent",
		"install -o root -g root -m 0755 \"" + p.launcherSrc + "\" /usr/local/libexec/maestro-launch",
		"install -o root -g root -m 0644 \"/tmp/x/launch.json\" /etc/maestro/launch-linux.json",
		"npm install --global --prefix /opt/maestro/claude-" + p.claude,
		"install -d -o root -g root -m 0711 /var/lib/maestro/linux/workspaces",
		"install -d -o maestro-agent -g maestro-agent -m 0700 /var/lib/maestro/linux/claude",
		"visudo -cqf \"/tmp/x/sudoers\"",
		"install -o root -g root -m 0440 \"/tmp/x/sudoers\" /etc/sudoers.d/maestro-linux",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	// Nothing is installed in this test machine.
	if probs := p.problems(t.Context()); len(probs) < 3 {
		t.Errorf("problems %q", probs)
	}
}

func TestSystemdStepIsDoneForContainers(t *testing.T) {
	s := &setupSystemd{setupDir: t.TempDir()}
	got, err := s.Check(t.Context(), &setup.State{})
	if err != nil || !got.Done || !strings.Contains(got.Detail, "containers") {
		t.Errorf("%+v %v", got, err)
	}
}

func TestUnderHome(t *testing.T) {
	for p, want := range map[string]bool{"/home/ian/x": true, "/home": true, "/homely": false, "/root/.config": true, "/srv/maestro": false} {
		if underHome(p) != want {
			t.Errorf("%s", p)
		}
	}
}
