package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/proxy"
)

// Other users of the machine cannot get to the mirror, the allowlists or
// the hand-off folders: the folders above them are the service's alone.
// The folders a container is given stay open to its user.
func TestDirsAreClosedToOtherUsers(t *testing.T) {
	root := t.TempDir()
	state, work := filepath.Join(root, "state"), filepath.Join(root, "workspaces")
	in := &install{cfg: &config.Config{}}
	in.cfg.Budget.StateDir = state
	in.mirrorDir, in.allowDir, in.auditDir = filepath.Join(state, "mirror"), filepath.Join(state, "proxy"), filepath.Join(state, "audit")
	in.handoffDir = filepath.Join(work, ".handoff")
	// Left open by an earlier version.
	for _, d := range []string{state, in.handoffDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		os.Chmod(d, 0o755)
	}
	if err := in.dirs(); err != nil {
		t.Fatal(err)
	}
	for d, want := range map[string]os.FileMode{state: 0o700, in.handoffDir: 0o700, in.auditDir: 0o700,
		in.mirrorDir: 0o755, in.allowDir: 0o755} {
		st, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm(); got != want {
			t.Errorf("%s has mode %04o, want %04o", d, got, want)
		}
	}
}

func TestAllowlistsTakeWebFetchHosts(t *testing.T) {
	in := &install{allowDir: t.TempDir()}
	cfg := &config.Config{}
	cfg.Sandbox.AllowAgent = []string{"go.dev"}
	cfg.Claude.WebFetch = []string{"go.dev", "*.readthedocs.io"}
	cfg.Sandbox.AllowHooks = []string{"registry.npmjs.org"}
	if err := in.allow(cfg); err != nil {
		t.Fatal(err)
	}
	agent := proxy.ReadAllow(filepath.Join(in.allowDir, "agent.json"))
	want := append(append([]string{}, proxy.DefaultAgentHosts...), "go.dev", "*.readthedocs.io")
	if !slices.Equal(agent, want) {
		t.Errorf("agent allowlist = %v, want %v", agent, want)
	}
	// The hooks never get the agent's reading list.
	if hooks := proxy.ReadAllow(filepath.Join(in.allowDir, "hooks.json")); !slices.Equal(hooks, []string{"registry.npmjs.org"}) {
		t.Errorf("hooks allowlist = %v", hooks)
	}
}
