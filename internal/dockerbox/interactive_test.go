package dockerbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDocker is a docker client that writes each command line it gets to
// a log, one per line, and knows of the volumes given.
func fakeDocker(t *testing.T, volumes ...string) (b *Box, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + log + `
if [ "$1" = volume ] && [ "$2" = ls ]; then
  printf '%s\n' ` + strings.Join(append([]string{"''"}, volumes...), " ") + `
fi
exit 0
`
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Box{Docker: bin, Worker: "w1", Image: "maestro-sandbox:1", MirrorDir: "/m", HandoffDir: "/h",
		UnityLicenceDir: "/licence", SkillsDir: "/skills", PIDs: 64}, log
}

func runLine(t *testing.T, log string) string {
	t.Helper()
	raw, _ := os.ReadFile(log)
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(l, "run ") {
			return l
		}
	}
	t.Fatalf("no container was run:\n%s", raw)
	return ""
}

func TestShellInAWorkspaceHasTheChecksView(t *testing.T) {
	b, log := fakeDocker(t, "maestro-w1-ws-GH-12")
	code, err := b.Interactive(ctx, Session{Key: "GH-12", View: ViewChecks}, strings.NewReader(""), new(strings.Builder), new(strings.Builder))
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	run := runLine(t, log)
	for _, want := range []string{"--tty", "--user 10001:10001", "--cap-drop ALL", "--security-opt no-new-privileges",
		"--label maestro.step=shell", "--label maestro.workspace=GH-12", "src=maestro-w1-ws-GH-12,dst=/workspace",
		"--network maestro-w1-hooks", "/licence,dst=/unity-licence,readonly", "--pids-limit 64", "--workdir /workspace/repo", "TERM=xterm-256color"} {
		if !strings.Contains(run, want) {
			t.Errorf("%q lacks %q", run, want)
		}
	}
	for _, not := range []string{"maestro-w1-claude", "/claude", "--network maestro-w1-agent", "/mirror", "/handoff", "--privileged", "CI=1"} {
		if strings.Contains(run, not) {
			t.Errorf("%q has %q", run, not)
		}
	}
}

func TestShellWithTheAgentsView(t *testing.T) {
	b, log := fakeDocker(t, "maestro-w1-ws-GH-12")
	argv := []string{"claude", "--resume", "x"}
	if _, err := b.Interactive(ctx, Session{Key: "GH-12", View: ViewAgent, Argv: argv}, strings.NewReader(""), new(strings.Builder), new(strings.Builder)); err != nil {
		t.Fatal(err)
	}
	run := runLine(t, log)
	for _, want := range []string{"src=maestro-w1-claude,dst=/claude", "--network maestro-w1-agent", "CLAUDE_CONFIG_DIR=/claude",
		"/skills,dst=/claude/skills,readonly", "maestro-sandbox:1 claude --resume x"} {
		if !strings.Contains(run, want) {
			t.Errorf("%q lacks %q", run, want)
		}
	}
	if strings.Contains(run, "unity-licence") {
		t.Errorf("the agent's view has the licence: %q", run)
	}
}

func TestShellWithoutATicketUsesAScratchWorkspace(t *testing.T) {
	b, log := fakeDocker(t)
	if _, err := b.Interactive(ctx, Session{View: ViewChecks}, strings.NewReader(""), new(strings.Builder), new(strings.Builder)); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(log)
	s := string(raw)
	if !strings.Contains(s, "volume create --label maestro.worker=w1 maestro-w1-ws-scratch-") ||
		!strings.Contains(s, "volume rm --force maestro-w1-ws-scratch-") || !strings.Contains(s, "--workdir /workspace ") {
		t.Errorf("log:\n%s", s)
	}
}

func TestShellRefusals(t *testing.T) {
	b, _ := fakeDocker(t, "maestro-w1-ws-GH-12")
	for _, sess := range []Session{
		{Key: "GH-13", View: ViewChecks},
		{Key: "../etc", View: ViewChecks},
		{Key: "GH-12", View: "root"},
		{Key: "GH-12", View: ViewChecks, Argv: []string{"--privileged"}},
		{Key: "GH-12", View: ViewChecks, Env: map[string]string{"GITHUB_TOKEN": "x"}},
	} {
		if _, err := b.Interactive(ctx, sess, strings.NewReader(""), new(strings.Builder), new(strings.Builder)); err == nil {
			t.Errorf("%+v was run", sess)
		}
	}
	if err := b.Join(ctx, "someone-else", strings.NewReader(""), new(strings.Builder), new(strings.Builder)); err == nil {
		t.Error("joined a container of another worker")
	}
}

func TestJoinRunsAsTheSandboxUser(t *testing.T) {
	b, log := fakeDocker(t)
	if err := b.Join(ctx, "maestro-w1-gh-12-agent-abc", strings.NewReader(""), new(strings.Builder), new(strings.Builder)); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(log)
	if !strings.HasPrefix(string(raw), "exec --interactive --tty --user 10001:10001 ") || !strings.Contains(string(raw), "maestro-w1-gh-12-agent-abc /bin/sh") {
		t.Errorf("log: %s", raw)
	}
}

func TestWorkspacesAndRunning(t *testing.T) {
	b, _ := fakeDocker(t, "maestro-w1-ws-GH-12", "maestro-w1-ws-ENG-7", "maestro-w1-claude", "maestro-w1-ws-selftest-abc", "maestro-w1-ws-scratch-abc")
	keys, err := b.Workspaces(ctx)
	if err != nil || strings.Join(keys, ",") != "ENG-7,GH-12" {
		t.Errorf("keys %v err %v", keys, err)
	}
	cs := parseRunning("maestro-w1-gh-12-agent-abc\tagent\nother\tagent\n\n", "maestro-w1-")
	if len(cs) != 1 || cs[0].Step != "agent" {
		t.Errorf("%+v", cs)
	}
}
