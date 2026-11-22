package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/setup"
)

// projectsHome makes Maestro's directory in a temporary place and returns
// it. Nothing in the tests is at a terminal.
func projectsHome(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the configuration directory is redirected through XDG_CONFIG_HOME")
	}
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	t.Setenv("MAESTRO_PROJECT", "")
	t.Chdir(t.TempDir())
	old := atTerminal
	atTerminal = func() bool { return false }
	t.Cleanup(func() { atTerminal = old; chosen = selection{} })
	chosen = selection{}
	return filepath.Join(base, "maestro")
}

func writeState(t *testing.T, dir string, st map[string]any) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(dir, "setup.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTakeProject(t *testing.T) {
	for _, c := range []struct {
		in   []string
		rest []string
		name string
	}{
		{[]string{"--file", "x"}, []string{"--file", "x"}, ""},
		{[]string{"--project", "game", "--file", "x"}, []string{"--file", "x"}, "game"},
		{[]string{"why", "--project=web"}, []string{"why"}, "web"},
		{[]string{"-project", "web"}, nil, "web"},
		{[]string{"--", "--project", "x"}, []string{"--", "--project", "x"}, ""},
	} {
		rest, name, err := takeProject(c.in)
		if err != nil || name != c.name || strings.Join(rest, " ") != strings.Join(c.rest, " ") {
			t.Errorf("%v: %v %q %v", c.in, rest, name, err)
		}
	}
	if _, _, err := takeProject([]string{"--project"}); err == nil {
		t.Error("--project without a name was accepted")
	}
}

// An installation from before projects is the one project, under its
// worker's name, and later projects come after it by name.
func TestListProjects(t *testing.T) {
	home := projectsHome(t)
	if ps, err := listProjects(); err != nil || len(ps) != 0 {
		t.Fatalf("fresh: %v %v", ps, err)
	}
	if got, _ := configDir(); got != home {
		t.Fatalf("fresh configDir %q", got)
	}
	writeState(t, home, map[string]any{"repo": "acme/web", "worker": "mac", "workflow_file": filepath.Join(home, "WORKFLOW.md")})
	chosen = selection{}
	if got, _ := configDir(); got != home {
		t.Fatalf("one project: configDir %q", got)
	}
	writeState(t, filepath.Join(home, "projects", "zgame"), map[string]any{"repo": "acme/game", "worker": "mac-zgame"})
	if err := os.MkdirAll(filepath.Join(home, "projects", "art"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "projects", "Not A Name"), 0o700); err != nil {
		t.Fatal(err)
	}
	ps, err := listProjects()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range ps {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "mac,art,zgame" || !ps[0].First || ps[0].Dir != home {
		t.Fatalf("projects %v", names)
	}
	// Several and nobody to ask: say how to name one.
	chosen = selection{}
	if _, err := configDir(); err == nil || !strings.Contains(err.Error(), "--project") {
		t.Fatalf("several projects: %v", err)
	}
	// A name file wins over the worker.
	os.WriteFile(filepath.Join(home, projectNameFile), []byte("web\n"), 0o600)
	if ps, _ := listProjects(); ps[0].Name != "web" {
		t.Fatalf("named first project: %q", ps[0].Name)
	}
}

func TestSelectProject(t *testing.T) {
	home := projectsHome(t)
	writeState(t, home, map[string]any{"repo": "acme/web", "worker": "mac", "workflow_file": filepath.Join(home, "WORKFLOW.md")})
	writeState(t, filepath.Join(home, "projects", "game"), map[string]any{"repo": "acme/game", "worker": "mac-game"})
	var out, errOut bytes.Buffer
	rest, _, done := selectProject("run", []string{"--project", "game", "--file", "f"}, strings.NewReader(""), &out, &errOut)
	if done || chosen.dir != filepath.Join(home, "projects", "game") || !chosen.named || strings.Join(rest, " ") != "--file f" {
		t.Fatalf("by name: %v %+v %s", rest, chosen, errOut.String())
	}
	chosen = selection{}
	t.Setenv("MAESTRO_PROJECT", "mac")
	if _, _, done := selectProject("run", nil, strings.NewReader(""), &out, &errOut); done || chosen.dir != home {
		t.Fatalf("from the environment: %+v", chosen)
	}
	t.Setenv("MAESTRO_PROJECT", "")
	chosen = selection{}
	if _, code, done := selectProject("run", []string{"--project", "nope"}, strings.NewReader(""), &out, &errOut); !done || code != 1 ||
		!strings.Contains(errOut.String(), "no project called nope") {
		t.Fatalf("unknown: %d %q", code, errOut.String())
	}
	errOut.Reset()
	if _, code, done := selectProject("run", []string{"--project", "game", "--dir", "/x"}, strings.NewReader(""), &out, &errOut); !done || code != 2 {
		t.Fatalf("--dir and --project: %d", code)
	}
	// setup with several projects and nobody at the keyboard.
	if _, code, done := selectProject("setup", nil, strings.NewReader(""), &out, &errOut); !done || code != 1 ||
		!strings.Contains(errOut.String(), "maestro setup --project NAME") {
		t.Fatalf("setup: %d %q", code, errOut.String())
	}
	// status shows every project.
	out.Reset()
	if _, code, done := selectProject("status", nil, strings.NewReader(""), &out, &errOut); !done || code != 0 ||
		!strings.Contains(out.String(), "── mac") || !strings.Contains(out.String(), "── game") ||
		!strings.Contains(out.String(), "maestro setup --project game") {
		t.Fatalf("status: %d %q", code, out.String())
	}
}

// A new project gets its own directory and a worker name nobody uses.
func TestAddProjectAndShareTheApp(t *testing.T) {
	home := projectsHome(t)
	key := filepath.Join(home, "github-app.pem")
	writeState(t, home, map[string]any{"repo": "acme/web", "worker": "mac", "owner_type": "Organization",
		"app_id": "7", "app_slug": "maestro-acme-abc", "app_key_file": key, "installation_id": "9",
		"workflow_file": filepath.Join(home, "WORKFLOW.md")})
	if err := os.WriteFile(key, []byte("KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	ps, _ := listProjects()
	var out bytes.Buffer
	ui := setup.NewUI(strings.NewReader("mac\ngame\nacme/game\n1\n"), &out)
	p, err := addProject(ui, "", ps, false)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if p.Name != "game" || p.Dir != filepath.Join(home, "projects", "game") {
		t.Fatalf("project %+v", p)
	}
	if !strings.Contains(out.String(), "already a project called mac") {
		t.Errorf("a taken name was not refused:\n%s", out.String())
	}
	st, err := setup.LoadState(p.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Repo != "acme/game" || st.AppID != "7" || st.InstallationID != "" || st.OwnerType != "Organization" ||
		st.Worker == "" || st.Worker == "mac" || st.AppKeyFile != filepath.Join(p.Dir, "github-app.pem") {
		t.Fatalf("state %+v", st)
	}
	if raw, _ := os.ReadFile(st.AppKeyFile); string(raw) != "KEY" {
		t.Fatalf("key %q", raw)
	}
	if fi, _ := os.Stat(st.AppKeyFile); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", fi.Mode())
	}
	// The first project keeps its name now that there are two.
	if raw, _ := os.ReadFile(filepath.Join(home, projectNameFile)); strings.TrimSpace(string(raw)) != "mac" {
		t.Fatalf("first project's name %q", raw)
	}
	if got := defaultImage(p.Dir); got != "maestro-sandbox-game:latest" {
		t.Fatalf("image %q", got)
	}
	if got := defaultImage(home); got != "" {
		t.Fatalf("first project's image %q", got)
	}
}

func TestClashAndWorkerNames(t *testing.T) {
	home := projectsHome(t)
	writeState(t, home, map[string]any{"repo": "acme/web", "worker": "mac"})
	game := filepath.Join(home, "projects", "game")
	writeState(t, game, map[string]any{"repo": "acme/game", "worker": "mac-game"})
	if err := clash(game, "mac"); err == nil || !strings.Contains(err.Error(), "worker name mac") {
		t.Fatalf("clash: %v", err)
	}
	if err := clash(game, "mac-game"); err != nil {
		t.Fatalf("own name: %v", err)
	}
	machine := setup.DefaultWorker(runtime.GOOS)
	if got := suggestWorker("game", []string{machine + "-game"}); got != machine+"-game-2" {
		t.Fatalf("suggest %q", got)
	}
	if got := suggestWorker(strings.Repeat("a", 24), nil); len(got) > 31 {
		t.Fatalf("too long %q", got)
	}
}

func TestProjectsList(t *testing.T) {
	projectsHome(t)
	var out, errOut bytes.Buffer
	if code := run([]string{"projects"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "no project") {
		t.Fatalf("%d %q", code, out.String())
	}
	if code := run([]string{"projects", "bogus"}, &out, &errOut); code != 2 {
		t.Fatalf("usage: %d", code)
	}
}

// A game's repository is made later, so the App is offered by its owner.
func TestAddGameProjectSharesTheApp(t *testing.T) {
	home := projectsHome(t)
	key := filepath.Join(home, "github-app.pem")
	writeState(t, home, map[string]any{"repo": "acme/web", "worker": "mac",
		"app_id": "7", "app_slug": "maestro-acme-abc", "app_key_file": key, "installation_id": "9",
		"workflow_file": filepath.Join(home, "WORKFLOW.md")})
	os.WriteFile(key, []byte("KEY"), 0o600)
	ps, _ := listProjects()
	var out bytes.Buffer
	p, err := addProject(setup.NewUI(strings.NewReader("1\n"), &out), "game", ps, true)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	st, _ := setup.LoadState(p.Dir)
	if st.Repo != "" || st.AppID != "7" || st.InstallationID != "" {
		t.Fatalf("state %+v", st)
	}
	// And without: a new App.
	ps, _ = listProjects()
	p, err = addProject(setup.NewUI(strings.NewReader("2\n"), &out), "other", ps, true)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := setup.LoadState(p.Dir); st.AppID != "" {
		t.Fatalf("state %+v", st)
	}
}
