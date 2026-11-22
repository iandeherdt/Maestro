package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/setup"
)

// A new Godot game from an empty folder, as far as the steps that need
// GitHub: the packs are written, the starter is there, and it is committed.
// A second run continues where the first stopped.
func TestNewGodotGame(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	game := filepath.Join(root, "game")
	os.MkdirAll(filepath.Join(game, "addons", "gut"), 0o755) // GUT is fetched from the network otherwise
	os.WriteFile(filepath.Join(game, "addons", "gut", "plugin.cfg"), []byte("[plugin]\n"), 0o644)
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "gitconfig"))
	t.Setenv("GIT_AUTHOR_NAME", "T")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "T")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	t.Chdir(game)
	setupDir := filepath.Join(root, "setup")

	var out, errOut strings.Builder
	in := strings.NewReader("2\nn\ny\n\ny\ny\ny\n") // Godot, no Blender, klad, this folder, git init, write, commit
	if code := runNew([]string{"--dir", setupDir}, in, &out, &errOut); code != 3 {
		t.Fatalf("exit %d, want 3 at the GitHub step\n%s\n%s", code, out.String(), errOut.String())
	}
	for _, f := range []string{"project.godot", "test/test_project.gd", "conventions/godot/RULES.md", "conventions/klad/RULES.md"} {
		if _, err := os.Stat(filepath.Join(game, f)); err != nil {
			t.Errorf("%s is missing", f)
		}
	}
	if log, err := exec.Command("git", "-C", game, "log", "--format=%s").Output(); err != nil || strings.TrimSpace(string(log)) != "Set up the game for Maestro" {
		t.Errorf("commit: %q %v", log, err)
	}
	if !strings.Contains(out.String(), "The repository on GitHub") {
		t.Errorf("did not reach the GitHub step:\n%s", out.String())
	}

	// The second run asks nothing that was answered.
	out.Reset()
	if code := runNew([]string{"--dir", setupDir}, strings.NewReader(""), &out, &errOut); code != 3 {
		t.Fatalf("second run: exit %d\n%s", code, out.String())
	}
	if strings.Contains(out.String(), "Which engine") || strings.Count(out.String(), "Already done") < 5 {
		t.Errorf("second run asked again:\n%s", out.String())
	}
}

func TestUnityProjectVersion(t *testing.T) {
	dir := t.TempDir()
	if projectVersion(dir) != "" {
		t.Error("an empty folder is a Unity project")
	}
	os.MkdirAll(filepath.Join(dir, "ProjectSettings"), 0o755)
	os.WriteFile(filepath.Join(dir, "ProjectSettings", "ProjectVersion.txt"),
		[]byte("m_EditorVersion: 6000.5.8f1\nm_EditorVersionWithRevision: 6000.5.8f1 (abc)\n"), 0o644)
	if v := projectVersion(dir); v != "6000.5.8f1" || v != unityVersion() {
		t.Errorf("version %q, the pack's %q", v, unityVersion())
	}
	f := &gameFolder{&game{engine: "unity", dir: dir}}
	if p := f.problem(); p != "" {
		t.Errorf("a project on the right version: %s", p)
	}
	os.WriteFile(filepath.Join(dir, "ProjectSettings", "ProjectVersion.txt"), []byte("m_EditorVersion: 2022.3.1f1\n"), 0o644)
	if p := f.problem(); !strings.Contains(p, "the checks use Unity 6000.5.8f1") {
		t.Errorf("a project on another version: %q", p)
	}
}

func TestHubProjects(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, "Games", "Cave")
	os.MkdirAll(filepath.Join(proj, "ProjectSettings"), 0o755)
	os.WriteFile(filepath.Join(proj, "ProjectSettings", "ProjectVersion.txt"), []byte("m_EditorVersion: 6000.5.8f1\n"), 0o644)
	hub := filepath.Join(home, "Library", "Application Support", "UnityHub")
	os.MkdirAll(hub, 0o755)
	os.WriteFile(filepath.Join(hub, "projects-v1.json"), []byte(`{"schema_version":"v1","data":{"`+proj+`":{"title":"Cave","path":"`+proj+`","version":"6000.5.8f1"},"/gone":{"path":"/gone"}}}`), 0o644)
	f := &gameFolder{&game{engine: "unity", home: home, goos: "darwin"}}
	if got := f.hubProjects(); len(got) != 1 || got[0] != proj {
		t.Errorf("hub projects: %v", got)
	}
}

// Hub puts a new project in a folder of its own inside the location; the
// guide finds it there. An empty folder gets a project made by the Editor
// Hub installed.
func TestUnityFolderInsideAndNewProject(t *testing.T) {
	home := t.TempDir()
	want := unityVersion()
	loc := filepath.Join(home, "projects", "test")
	proj := filepath.Join(loc, "My project")
	os.MkdirAll(filepath.Join(proj, "ProjectSettings"), 0o755)
	os.WriteFile(filepath.Join(proj, "ProjectSettings", "ProjectVersion.txt"), []byte("m_EditorVersion: "+want+"\n"), 0o644)
	if got := onlyProject(loc); got != proj {
		t.Errorf("onlyProject: %q", got)
	}
	if emptyDir(loc) || !emptyDir(filepath.Join(home, "nothing")) {
		t.Error("emptyDir")
	}

	// A stand-in for the Editor: it makes ProjectSettings in the folder after -createProject.
	editor := filepath.Join(home, "Unity", "Hub", "Editor", want, "Editor", "Unity")
	os.MkdirAll(filepath.Dir(editor), 0o755)
	os.WriteFile(editor, []byte("#!/bin/sh\nwhile [ $# -gt 0 ]; do if [ \"$1\" = -createProject ]; then d=$2; fi; shift; done\n"+
		"mkdir -p \"$d/ProjectSettings\" && echo 'm_EditorVersion: "+want+"' > \"$d/ProjectSettings/ProjectVersion.txt\"\n"), 0o755)
	cwd := t.TempDir()
	t.Chdir(cwd)
	g := &game{engine: "unity", home: home, goos: "linux", setupDir: t.TempDir()}
	f := &gameFolder{g}
	if f.localEditor(want) != editor {
		t.Fatalf("editor not found: %q", f.localEditor(want))
	}
	// Choose "None yet" (the last of: another folder, none yet), take the
	// offered folder, and say yes.
	var out strings.Builder
	ui := setup.NewUI(strings.NewReader("2\n\ny\n"), &out)
	if err := f.Run(t.Context(), ui, nil); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if g.dir != filepath.Join(cwd, "my-game") || projectVersion(g.dir) != want {
		t.Errorf("project %q:\n%s", g.dir, out.String())
	}
}
