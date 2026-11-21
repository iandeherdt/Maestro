package reload

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iandeherdt/Symphony-beads/internal/config"
	"github.com/iandeherdt/Symphony-beads/internal/issue"
	"github.com/iandeherdt/Symphony-beads/internal/prompt"
	"github.com/iandeherdt/Symphony-beads/internal/workflow"
)

type fixture struct {
	t    *testing.T
	path string
	base string
	src  *Source
	logs *bytes.Buffer
}

func setup(t *testing.T) *fixture {
	t.Helper()
	base, err := os.ReadFile(filepath.Join("..", "..", "examples", "WORKFLOW.example.md"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, path: filepath.Join(t.TempDir(), "WORKFLOW.md"), base: string(base), logs: &bytes.Buffer{}}
	f.write(f.base)
	env := func(name string) string { return "value-of-" + name }
	src, err := Open(f.path, config.Options{Env: env, HomeDir: "/home/x"}, slog.New(slog.NewTextHandler(f.logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	f.src = src
	return f
}

func (f *fixture) write(content string) {
	f.t.Helper()
	if err := os.WriteFile(f.path, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// edit replaces old with new in the base file and writes the result.
func (f *fixture) edit(old, new string) {
	f.t.Helper()
	if !strings.Contains(f.base, old) {
		f.t.Fatalf("fixture has no %q", old)
	}
	f.write(strings.Replace(f.base, old, new, 1))
}

func TestUnchangedFileIsNotReapplied(t *testing.T) {
	f := setup(t)
	for i := 0; i < 3; i++ {
		if l, err := f.src.Check(); l != nil || err != nil {
			t.Fatalf("check %d: %v %v", i, l, err)
		}
	}
}

func TestValidChangeIsApplied(t *testing.T) {
	f := setup(t)
	f.edit("budget:\n", "budget:\n  max_failure_attempts: 7\n")
	l, err := f.src.Check()
	if err != nil || l == nil || l.Config.Budget.MaxFailureAttempts != 7 {
		t.Fatalf("loaded %+v, err %v", l, err)
	}
	if f.src.Current() != l {
		t.Fatal("current view not replaced")
	}
	cfg, err := f.src.Preflight()
	if cfg != nil || err != nil {
		t.Fatalf("second check: %v %v", cfg, err)
	}
}

func TestPromptChangeIsApplied(t *testing.T) {
	f := setup(t)
	f.write(f.base + "\nAlways run `make lint`.\n")
	l, err := f.src.Check()
	if err != nil || l == nil {
		t.Fatalf("loaded %v, err %v", l, err)
	}
	out, err := l.Prompt.First(prompt.Input{Issue: issue.Issue{Title: "x"}})
	if err != nil || !strings.Contains(out, "Always run `make lint`.") {
		t.Fatalf("prompt not replaced: %v", err)
	}
}

func TestInvalidReloadKeepsLastGood(t *testing.T) {
	good := setup(t).src.Current().Config.Budget.MaxFailureAttempts
	for name, change := range map[string]func(f *fixture){
		"bad yaml":            func(f *fixture) { f.edit("tracker:", "tracker: [") },
		"invalid config":      func(f *fixture) { f.edit("kind: github", "kind: jira") },
		"unknown sandbox key": func(f *fixture) { f.edit("sandbox:\n", "sandbox:\n  privileged: true\n") },
		"bad template":        func(f *fixture) { f.write(f.base + "\n{{ issue.title | exec }}\n") },
		"file removed":        func(f *fixture) { os.Remove(f.path) },
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			before := f.src.Current()
			change(f)
			for i := 0; i < 3; i++ {
				l, err := f.src.Check()
				if l != nil || err == nil {
					t.Fatalf("check %d: loaded %v, err %v", i, l, err)
				}
			}
			if f.src.Current() != before || before.Config.Budget.MaxFailureAttempts != good {
				t.Fatal("last good configuration was replaced")
			}
			if n := strings.Count(f.logs.String(), "workflow reload rejected"); n != 1 {
				t.Errorf("rejection logged %d times, want once", n)
			}
			// Fixing the file recovers without a restart.
			f.write(f.base + "\n")
			if l, err := f.src.Check(); l == nil || err != nil {
				t.Fatalf("after fix: %v %v", l, err)
			}
		})
	}
}

func TestMissingFileAtStartupFails(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "WORKFLOW.md"), config.Options{}, nil)
	if workflow.ClassOf(err) != workflow.ClassMissingFile {
		t.Fatalf("err = %v", err)
	}
}

func TestRestartRequiredKeysAreNotApplied(t *testing.T) {
	f := setup(t)
	old := *f.src.Current().Config
	content := f.base
	for _, r := range [][2]string{
		{"root: /var/lib/symphony-agent/workspaces", "root: /tmp/elsewhere"},
		{"repo: your-account/your-repo\nhooks:", "repo: other-account/other-repo\nhooks:"},
		{"backend: systemd", "backend: macos-user"},
		{"state_dir: /var/lib/symphony/state", "state_dir: /tmp/state"},
		{"name: linux", "name: mac"},
		{"budget:\n", "server:\n  port: 8080\nbudget:\n  max_failure_attempts: 9\n"},
	} {
		if !strings.Contains(content, r[0]) {
			t.Fatalf("fixture has no %q", r[0])
		}
		content = strings.Replace(content, r[0], r[1], 1)
	}
	f.write(content)
	l, err := f.src.Check()
	if err != nil || l == nil {
		t.Fatalf("loaded %v, err %v", l, err)
	}
	c := l.Config
	if c.Workspace.Root != old.Workspace.Root || c.Workspace.Repo != old.Workspace.Repo ||
		c.Sandbox.Backend != old.Sandbox.Backend || c.Budget.StateDir != old.Budget.StateDir ||
		c.Worker.Name != "linux" || c.Server.Port != nil {
		t.Fatalf("restart-required key applied: %+v", c)
	}
	// The worker must keep taking its own issues, not the other worker's.
	if got := strings.Join(c.Tracker.RequiredLabels, ","); got != "run:linux" {
		t.Fatalf("required labels = %s", got)
	}
	// Everything else in the same edit still applies.
	if c.Budget.MaxFailureAttempts != 9 {
		t.Fatal("ordinary key in the same edit was not applied")
	}
	for _, key := range []string{"workspace.root", "workspace.repo", "sandbox.backend", "budget.state_dir", "worker.name", "server.port"} {
		if !strings.Contains(f.logs.String(), "key="+key) {
			t.Errorf("no log line for ignored key %s", key)
		}
	}
}

func TestTrackerKindIsRestartRequired(t *testing.T) {
	old := &config.Config{Tracker: config.Tracker{Kind: "github", Provider: map[string]any{"repo": "a/b"}}}
	next := &config.Config{Tracker: config.Tracker{Kind: "linear", Provider: map[string]any{"team": "X"}}}
	if got := keepRestartKeys(old, next); len(got) != 1 || got[0] != "tracker.kind" {
		t.Fatalf("ignored = %v", got)
	}
	if next.Tracker.Kind != "github" || next.Tracker.Provider["repo"] != "a/b" {
		t.Fatalf("tracker = %+v", next.Tracker)
	}
}
