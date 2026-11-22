package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/conventions"
	"github.com/iandeherdt/Maestro/internal/hostcheck"
	"github.com/iandeherdt/Maestro/internal/setup"
	"github.com/iandeherdt/Maestro/internal/workflow"
	"golang.org/x/term"
)

const newUsage = `usage: maestro new [--another] [--dir DIR]

A guided start of a game project that the agents work on: it finds or makes
the project, adds the rule packs, puts it on GitHub, points Maestro at it,
imports the Unity licence and builds the sandbox image. Every step says what
it will do and waits for you; run it again to continue where you stopped.`

// game is what the steps of `maestro new` find out and share.
type game struct {
	setupDir string
	st       *setup.State
	stdin    io.Reader // the wizard's reader, for commands it runs
	rawIn    io.Reader // the terminal itself, for a password typed without echo
	stdout   io.Writer
	stderr   io.Writer

	engine string   // "unity" or "godot"
	art    []string // "blender", "klad"
	dir    string   // the project
	repo   string   // owner/name on GitHub

	// home and look are the user's home directory and exec.LookPath,
	// replaced by tests.
	home string
	look func(string) (string, error)
	goos string
	arch string
}

func (g *game) packs() []string { return append([]string{g.engine}, g.art...) }

// saved is what `maestro new` remembers between runs, so a second run
// continues with the same game.
type saved struct {
	Engine string   `json:"engine"`
	Art    []string `json:"art"`
	Dir    string   `json:"dir"`
}

func (g *game) savePath() string { return filepath.Join(g.setupDir, "new-game.json") }

func (g *game) load() {
	raw, err := os.ReadFile(g.savePath())
	if err != nil {
		return
	}
	var s saved
	if json.Unmarshal(raw, &s) == nil && (s.Engine == "unity" || s.Engine == "godot") {
		g.engine, g.art, g.dir = s.Engine, s.Art, s.Dir
	}
}

func (g *game) save() error {
	raw, err := json.MarshalIndent(saved{g.engine, g.art, g.dir}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(g.savePath(), append(raw, '\n'), 0o600)
}

// git runs git without a terminal: a question for a password fails at
// once instead of waiting where nobody sees it.
func (g *game) git(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", g.dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if os.Getenv("GIT_SSH_COMMAND") == "" {
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes -o ConnectTimeout=15 -o StrictHostKeyChecking=accept-new")
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// interactive runs a command whose output and questions go to the person.
// It reads the terminal itself, never the wizard's input: a command that
// reads a pipe would take the answers meant for the next questions.
func (g *game) interactive(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = g.dir, g.stdout, g.stderr
	if f, ok := g.rawIn.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		cmd.Stdin = f
	}
	return cmd.Run()
}

func runNew(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "where setup keeps its state (default: the user's configuration directory)")
	again := fs.Bool("another", false, "start another game instead of continuing the last one")
	fs.Usage = func() { fmt.Fprintln(stderr, newUsage) }
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, newUsage)
		return 2
	}
	if *dir == "" {
		found, err := configDir()
		if err != nil {
			return report(stderr, explain("no configuration directory was found for this user, pass --dir: %v", err))
		}
		*dir = found
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		return report(stderr, err)
	}
	st, err := setup.LoadState(abs)
	if err != nil {
		return report(stderr, err)
	}
	if ps, err := listProjects(); err == nil {
		st.TakenWorkers = workersOf(ps, abs)
	}
	home, _ := os.UserHomeDir()
	g := &game{setupDir: abs, st: st, stdin: stdin, rawIn: stdin, stdout: stdout, stderr: stderr,
		home: home, look: exec.LookPath, goos: runtime.GOOS, arch: runtime.GOARCH}
	if !*again {
		g.load()
	}
	ui := setup.Terminal(stdin, stdout)
	g.stdin = ui.In // one reader, so no typed answer is lost between the wizard and a command
	ui.Banner("Maestro new game", "Set up a game project for the agents.",
		"Every step says what it will do and waits for you.",
		"You can stop with Ctrl-C and run `maestro new` again; finished steps are skipped.")
	code := runGame(context.Background(), ui, g)
	if code != 0 {
		return code
	}
	ui.Say("")
	ui.Headline(true, fmt.Sprintf("%s is set up: the agents can work on it.", g.repo))
	ui.Say("What is left:")
	ui.Commands([][2]string{
		{"maestro login", "signs Claude Code in inside the sandbox (once per machine)"},
		{"maestro run", "starts the service; or maestro service install, to run it in the background"},
	})
	ui.Say("")
	ui.Say("Then give it a first card, such as: \"Add a Health component with TakeDamage and Heal, kept between 0 and a maximum, with tests.\" Approve it (Todo) and follow it with `maestro status`.")
	return 0
}

func runGame(ctx context.Context, ui *setup.UI, g *game) int {
	steps := []setup.Step{
		&gameKind{g}, &gameFolder{g}, &gameGit{g}, &gamePacks{g}, &gameCommit{g}, &gameGitHub{g},
		&gameRepo{g},
		&setup.Machine{}, &setup.CreateApp{Port: setup.DefaultPort}, &setup.InstallApp{}, &setup.Tickets{},
		&setup.WorkflowFile{PackTests: func() string { return "sh conventions/" + g.engine + "/verify.sh" }},
		&gameEnable{g}, &gameHostUnity{g}, &gameImage{g}, &gameLicence{g},
	}
	if err := setup.Run(ctx, ui, g.st, steps); err != nil {
		if errors.Is(err, setup.ErrAborted) {
			ui.Fail(g.stderr, errors.New("stopped; run `maestro new` again to continue where you left off"), true)
			return 3
		}
		ui.Fail(g.stderr, err, false)
		return 1
	}
	return 0
}

// ---- 1. the kind of game

type gameKind struct{ *game }

func (*gameKind) Title() string { return "Choose the engine and the art" }

func (s *gameKind) Check(context.Context, *setup.State) (setup.Status, error) {
	if s.engine == "" {
		return setup.Status{Detail: "Maestro has rules, checks and a skill for Unity and for Godot, and for art made by Blender scripts or painted with klad."}, nil
	}
	return setup.Status{Done: true, Detail: "Packs: " + strings.Join(s.packs(), ", ") + ". (For a different game: maestro new --another.)"}, nil
}

func (s *gameKind) Run(_ context.Context, ui *setup.UI, _ *setup.State) error {
	i, err := ui.Choose("Which engine is the game made with?", []string{
		"Unity (C#), with Unity's tests; needs a Unity licence, a free Personal one will do",
		"Godot 4 (GDScript), with GUT tests; needs no licence",
	})
	if err != nil {
		return err
	}
	s.engine = []string{"unity", "godot"}[i]
	s.art, s.dir = nil, ""
	if ok, err := ui.Confirm("Will the agents make 3D models with Python scripts that Blender runs?"); err != nil {
		return err
	} else if ok {
		s.art = append(s.art, "blender")
	}
	if ok, err := ui.Confirm("Will the agents paint 2D art (textures, sprites) with klad?"); err != nil {
		return err
	} else if ok {
		s.art = append(s.art, "klad")
	}
	return s.save()
}

// ---- 2. the project folder

type gameFolder struct{ *game }

func (*gameFolder) Title() string { return "Find the project" }

// unityVersion is the Editor version of the unity pack's image.
func unityVersion() string {
	p, err := conventions.Get("unity")
	if err != nil {
		return ""
	}
	if m := regexp.MustCompile(`Editor ([0-9]+\.[0-9]+\.[0-9]+[abfp][0-9]+)`).FindStringSubmatch(p.ImageNote); m != nil {
		return m[1]
	}
	return ""
}

// projectVersion is the Unity version a project is saved with, empty when
// the folder is not a Unity project.
func projectVersion(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "ProjectSettings", "ProjectVersion.txt"))
	if err != nil {
		return ""
	}
	if m := regexp.MustCompile(`(?m)^m_EditorVersion:\s*(\S+)`).FindSubmatch(raw); m != nil {
		return string(m[1])
	}
	return "unknown"
}

func (s *gameFolder) problem() string {
	if s.dir == "" {
		return "no folder chosen yet"
	}
	if s.engine == "unity" {
		switch v, want := projectVersion(s.dir), unityVersion(); {
		case v == "":
			return s.dir + " is not a Unity project (it has no ProjectSettings/ProjectVersion.txt)"
		case v != want:
			return fmt.Sprintf("%s is saved with Unity %s; the checks use Unity %s", s.dir, v, want)
		}
		return ""
	}
	if st, err := os.Stat(s.dir); err != nil || !st.IsDir() {
		return s.dir + " is not a folder"
	}
	return ""
}

func (s *gameFolder) Check(context.Context, *setup.State) (setup.Status, error) {
	if p := s.problem(); p != "" {
		return setup.Status{Detail: "The game's own folder: " + p + "."}, nil
	}
	return setup.Status{Done: true, Detail: s.dir}, nil
}

// hubProjects are the projects Unity Hub knows, newest first where Hub
// says. Hub keeps them in a JSON file of its own; anything unexpected in it
// is skipped.
func (s *gameFolder) hubProjects() []string {
	var files []string
	switch s.goos {
	case "darwin":
		files = append(files, filepath.Join(s.home, "Library", "Application Support", "UnityHub", "projects-v1.json"))
	case "windows":
		files = append(files, filepath.Join(os.Getenv("APPDATA"), "UnityHub", "projects-v1.json"))
	default:
		files = append(files, filepath.Join(s.home, ".config", "UnityHub", "projects-v1.json"))
	}
	var out []string
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var doc struct {
			Data map[string]struct {
				Path string `json:"path"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &doc) != nil {
			continue
		}
		for k, p := range doc.Data {
			path := p.Path
			if path == "" {
				path = k
			}
			if projectVersion(path) != "" && !slices.Contains(out, path) {
				out = append(out, path)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (s *gameFolder) Run(_ context.Context, ui *setup.UI, _ *setup.State) error {
	cwd, _ := os.Getwd()
	if s.engine == "godot" {
		def := cwd
		ans, err := ui.Ask("  In which folder is (or will be) the game? An empty folder is fine; it is made if it is not there.", def, nil)
		if err != nil {
			return err
		}
		dir, err := filepath.Abs(expandHome(ans, s.home))
		if err != nil {
			return err
		}
		if _, err := os.Stat(dir); err != nil {
			ok, err := ui.Confirm("  " + dir + " does not exist. Make it?")
			if err != nil {
				return err
			}
			if !ok {
				return setup.ErrAborted
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		s.dir = dir
		return s.save()
	}

	want := unityVersion()
	for {
		var options, paths []string
		if projectVersion(cwd) != "" {
			paths = append(paths, cwd)
		}
		for _, p := range s.hubProjects() {
			if !slices.Contains(paths, p) {
				paths = append(paths, p)
			}
		}
		for _, p := range paths {
			v := projectVersion(p)
			note := "Unity " + v
			if v != want {
				note += ", not " + want
			}
			options = append(options, fmt.Sprintf("%s   (%s)", tildePath(p, s.home), note))
		}
		other := len(options)
		options = append(options, "Another folder: I will type its path")
		none := len(options)
		if s.localEditor(want) != "" {
			options = append(options, "None yet: make a new one with Unity "+want)
		} else {
			options = append(options, "None yet: tell me how to make one in Unity Hub")
		}
		i, err := ui.Choose("Which Unity project is the game?", options)
		if err != nil {
			return err
		}
		switch i {
		case none:
			if editor := s.localEditor(want); editor != "" {
				ans, err := ui.Ask("  Where should the new project be? A folder that does not exist yet, or an empty one", filepath.Join(cwd, "my-game"), nil)
				if err != nil {
					return err
				}
				dir, err := filepath.Abs(expandHome(ans, s.home))
				if err != nil {
					return err
				}
				if err := s.create(ui, editor, want, dir); err != nil {
					if errors.Is(err, setup.ErrAborted) {
						return err
					}
					ui.Say("  %v", err)
					continue
				}
				s.dir = dir
				break
			}
			ui.Say("  Unity %s is not installed in Unity Hub on this machine, so make the project in Hub:", want)
			ui.Say("   1. If Hub's Installs page has no Unity %s: Install Editor, the Archive tab, and pick %s.", want, want)
			ui.Say("   2. Projects, New project, Editor version %s, a template such as Universal 3D.", want)
			ui.Say("   3. Give it a name and a location, press Create project, and close Unity when it has opened.")
			if _, err := ui.Ask("  Press Enter when the project is made", "", nil); err != nil {
				return err
			}
			continue
		case other:
			ans, err := ui.Ask("  The project's folder (the one with Assets, Packages and ProjectSettings in it)", "", nil)
			if err != nil {
				return err
			}
			s.dir, err = filepath.Abs(expandHome(ans, s.home))
			if err != nil {
				return err
			}
		default:
			s.dir = paths[i]
		}
		if projectVersion(s.dir) == "" {
			// Hub makes the project in a folder of its own inside the one
			// chosen as its location.
			if sub := onlyProject(s.dir); sub != "" {
				ui.Say("  The Unity project is in the folder inside it: %s", tildePath(sub, s.home))
				s.dir = sub
			} else if emptyDir(s.dir) {
				if editor := s.localEditor(want); editor != "" {
					if err := s.create(ui, editor, want, s.dir); err != nil {
						if errors.Is(err, setup.ErrAborted) {
							return err
						}
						ui.Say("  %v", err)
						s.dir = ""
						continue
					}
				}
			}
		}
		if p := s.problem(); p != "" {
			ui.Say("  %s.", p)
			if projectVersion(s.dir) == "" {
				if emptyDir(s.dir) {
					ui.Say("  Unity %s is not installed in Unity Hub, so the project cannot be made here. Install it in", want)
					ui.Say("  Hub (Installs, Install Editor, Archive), or choose None yet for the steps in Hub.")
				} else {
					ui.Say("  The folder holds no Unity project. Choose one, or a new or empty folder to make one in.")
				}
			}
			if strings.Contains(p, "the checks use") {
				ui.Say("  Open it in Unity Hub with Editor %s (Hub asks whether to change the version: yes), let Unity", want)
				ui.Say("  finish, close it, and choose it here again. The agents' checks run that exact version.")
			}
			s.dir = ""
			continue
		}
		return s.save()
	}
}

// emptyDir reports whether a folder does not exist or holds nothing but
// Git's and the Finder's own files.
func emptyDir(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return os.IsNotExist(err)
	}
	for _, e := range entries {
		if e.Name() != ".git" && e.Name() != ".DS_Store" {
			return false
		}
	}
	return true
}

// onlyProject is the one Unity project directly inside dir, empty when
// there is none or more than one.
func onlyProject(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	found := ""
	for _, e := range entries {
		if p := filepath.Join(dir, e.Name()); e.IsDir() && projectVersion(p) != "" {
			if found != "" {
				return ""
			}
			found = p
		}
	}
	return found
}

// localEditor is the Unity Editor of a version as Unity Hub installs it on
// this machine, empty when there is none.
func (s *gameFolder) localEditor(version string) string {
	var roots []string
	var rel string
	switch s.goos {
	case "darwin":
		roots = []string{"/Applications/Unity/Hub/Editor"}
		rel = "Unity.app/Contents/MacOS/Unity"
	case "windows":
		roots = []string{`C:\Program Files\Unity\Hub\Editor`}
		rel = `Editor\Unity.exe`
	default:
		roots = []string{filepath.Join(s.home, "Unity", "Hub", "Editor")}
		rel = "Editor/Unity"
	}
	// Hub can be told to install editors elsewhere; it keeps that place as
	// a JSON string.
	for _, f := range []string{
		filepath.Join(s.home, "Library", "Application Support", "UnityHub", "secondaryInstallPath.json"),
		filepath.Join(s.home, ".config", "UnityHub", "secondaryInstallPath.json"),
	} {
		var other string
		if raw, err := os.ReadFile(f); err == nil && json.Unmarshal(raw, &other) == nil && other != "" {
			roots = append(roots, other)
		}
	}
	for _, r := range roots {
		p := filepath.Join(r, version, rel)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// create makes a new Unity project with the Editor on this machine, which
// uses the licence Unity Hub activated here.
func (s *gameFolder) create(ui *setup.UI, editor, version, dir string) error {
	if !emptyDir(dir) {
		return fmt.Errorf("%s holds other files; a new project needs a new or empty folder", tildePath(dir, s.home))
	}
	ok, err := ui.Confirm(fmt.Sprintf("  Make a new Unity project in %s with Unity %s? It takes a minute or two.", tildePath(dir, s.home), version))
	if err != nil {
		return err
	}
	if !ok {
		return setup.ErrAborted
	}
	logFile := filepath.Join(os.TempDir(), "maestro-unity-create.log")
	stop := ui.Waiting("Unity is making the project")
	err = exec.Command(editor, "-batchmode", "-nographics", "-quit", "-createProject", dir, "-logFile", logFile).Run()
	stop()
	if err != nil || projectVersion(dir) == "" {
		return fmt.Errorf("unity could not make the project (%v). Its log is %s; the last lines often say why, such as no licence: sign in to Unity Hub and add one", err, logFile)
	}
	ui.Say("  The project is made with Unity %s.", projectVersion(dir))
	return nil
}

func expandHome(p, home string) string {
	p = strings.TrimSpace(strings.Trim(p, `"'`))
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

func tildePath(p, home string) string {
	if home != "" && strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + p[len(home):]
	}
	return p
}

// ---- 3. git

type gameGit struct{ *game }

func (*gameGit) Title() string { return "Make it a Git repository" }

func (s *gameGit) Check(context.Context, *setup.State) (setup.Status, error) {
	if _, err := os.Stat(filepath.Join(s.dir, ".git")); err != nil {
		return setup.Status{Detail: "The agents get the project through Git, so the folder has to be a repository."}, nil
	}
	return setup.Status{Done: true, Detail: s.dir + " is a Git repository."}, nil
}

func (s *gameGit) Run(_ context.Context, ui *setup.UI, _ *setup.State) error {
	if _, err := s.look("git"); err != nil {
		return explain("git is not installed. On a Mac: xcode-select --install, then run `maestro new` again")
	}
	ok, err := ui.Confirm("  Run `git init` in " + tildePath(s.dir, s.home) + "?")
	if err != nil {
		return err
	}
	if !ok {
		return setup.ErrAborted
	}
	if out, err := s.git("init", "--initial-branch=main"); err != nil {
		return fmt.Errorf("git init: %v: %s", err, out)
	}
	return nil
}

// ---- 4. the packs

type gamePacks struct{ *game }

func (*gamePacks) Title() string { return "Add the rules, the checks and the skills" }

func (s *gamePacks) missing() []string {
	var out []string
	for _, n := range s.packs() {
		p, err := conventions.Get(n)
		if err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.dir, filepath.FromSlash(p.GuidePath()))); err != nil {
			out = append(out, n)
		}
	}
	if s.engine == "godot" {
		if _, err := os.Stat(filepath.Join(s.dir, "addons", "gut")); err != nil {
			out = append(out, "GUT")
		}
	}
	return out
}

// outdated are the files of packs already in the project that this
// version of Maestro has changed, or added. The project's own settings
// of a pack (settings.sh) are the owner's and are left alone.
func (s *gamePacks) outdated() map[string][]string {
	out := map[string][]string{}
	for _, n := range s.packs() {
		p, err := conventions.Get(n)
		if err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.dir, filepath.FromSlash(p.GuidePath()))); err != nil {
			continue // not in the project: missing, not outdated
		}
		files, err := p.Files()
		if err != nil {
			continue
		}
		for name, raw := range files {
			if path.Base(name) == "settings.sh" {
				continue
			}
			if have, err := os.ReadFile(filepath.Join(s.dir, filepath.FromSlash(name))); err != nil || !bytes.Equal(have, raw) {
				out[n] = append(out[n], name)
			}
		}
		sort.Strings(out[n])
	}
	return out
}

// update writes this version's files of the packs into the project.
func (s *gamePacks) update(old map[string][]string) error {
	for n, names := range old {
		p, err := conventions.Get(n)
		if err != nil {
			return err
		}
		files, err := p.Files()
		if err != nil {
			return err
		}
		for _, name := range names {
			target := filepath.Join(s.dir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if strings.HasSuffix(name, ".sh") {
				mode = 0o755
			}
			if err := os.WriteFile(target, files[name], mode); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *gamePacks) Check(context.Context, *setup.State) (setup.Status, error) {
	if m := s.missing(); len(m) > 0 {
		return setup.Status{Detail: "Not in the project yet: " + strings.Join(m, ", ") + "."}, nil
	}
	if old := s.outdated(); len(old) > 0 {
		var names []string
		for n := range old {
			names = append(names, n)
		}
		sort.Strings(names)
		return setup.Status{Detail: "This version of Maestro has newer files for " + strings.Join(names, ", ") + "; they are updated."}, nil
	}
	return setup.Status{Done: true, Detail: "In the project: " + strings.Join(s.packs(), ", ") + "."}, nil
}

func (s *gamePacks) Run(_ context.Context, ui *setup.UI, _ *setup.State) error {
	if len(s.missing()) == 0 {
		old := s.outdated()
		for _, n := range s.packs() {
			for _, f := range old[n] {
				ui.Show("    %s", f)
			}
		}
		ok, err := ui.Confirm("  Update these files of the packs to this version of Maestro? Then they are committed and pushed.")
		if err != nil {
			return err
		}
		if !ok {
			return setup.ErrAborted
		}
		return s.update(old)
	}
	var list []*conventions.Pack
	for _, n := range s.packs() {
		p, err := conventions.Get(n)
		if err != nil {
			return err
		}
		list = append(list, p)
	}
	ui.Say("  These packs are written into the project, each with the scripts of its checks, a CI workflow")
	ui.Say("  and a skill for the agents. The agents cannot change them.")
	for _, p := range list {
		ui.Say("   - %s: %s", p.Name, p.Title)
	}
	if s.engine == "godot" {
		ui.Say("  A new Godot project also gets project.godot and a first test, and GUT 9.7.1 is downloaded into addons/gut.")
	}
	if s.engine == "unity" {
		ui.Say("  The Unity project also gets a first EditMode test, so its checks pass from the start.")
	}
	ok, err := ui.Confirm("  Write them?")
	if err != nil {
		return err
	}
	if !ok {
		return setup.ErrAborted
	}
	for _, p := range list {
		rep, err := p.Apply(s.dir, false)
		if err != nil {
			return err
		}
		if len(rep.Conflicts) > 0 {
			ui.Say("  The %s pack would replace these files, which hold something else:", p.Name)
			for _, c := range rep.Conflicts {
				ui.Show("    %s", c)
			}
			ok, err := ui.Confirm("  Replace them with the pack's version? Git keeps nothing of them unless they are committed.")
			if err != nil {
				return err
			}
			if !ok {
				return setup.ErrAborted
			}
			if rep, err = p.Apply(s.dir, true); err != nil {
				return err
			}
		}
		ui.Say("  %s: %d file(s) written, %d new for a new project.", p.Name, len(rep.Written), len(rep.Started))
	}
	if s.engine == "godot" {
		if _, err := os.Stat(filepath.Join(s.dir, "addons", "gut")); err != nil {
			ui.Say("  Downloading GUT:")
			if err := s.interactive("sh", "conventions/godot/get-gut.sh"); err != nil {
				return fmt.Errorf("GUT could not be downloaded (%v). Check the network and run `maestro new` again", err)
			}
		}
	}
	return nil
}

// ---- 5. commit

type gameCommit struct{ *game }

func (*gameCommit) Title() string { return "Commit the project" }

func (s *gameCommit) Check(context.Context, *setup.State) (setup.Status, error) {
	if _, err := s.git("rev-parse", "--verify", "HEAD"); err != nil {
		return setup.Status{Detail: "Nothing is committed yet."}, nil
	}
	out, err := s.git("status", "--porcelain")
	if err != nil {
		return setup.Status{}, fmt.Errorf("git status: %v: %s", err, out)
	}
	if out != "" {
		return setup.Status{Detail: fmt.Sprintf("%d file(s) are not committed.", len(strings.Split(out, "\n")))}, nil
	}
	return setup.Status{Done: true, Detail: "Everything is committed."}, nil
}

func (s *gameCommit) Run(_ context.Context, ui *setup.UI, _ *setup.State) error {
	ok, err := ui.Confirm("  Commit everything in the folder, with the message \"Set up the game for Maestro\"?")
	if err != nil {
		return err
	}
	if !ok {
		return setup.ErrAborted
	}
	if out, err := s.git("add", "-A"); err != nil {
		return fmt.Errorf("git add: %v: %s", err, out)
	}
	if out, err := s.git("commit", "-q", "-m", "Set up the game for Maestro"); err != nil {
		if strings.Contains(out, "user.email") || strings.Contains(out, "identity") {
			return explain("Git does not know who you are yet. Tell it once, then run `maestro new` again:\n" +
				"  git config --global user.name \"Your Name\"\n  git config --global user.email you@example.com")
		}
		return fmt.Errorf("git commit: %v: %s", err, out)
	}
	return nil
}

// ---- 6. GitHub

type gameGitHub struct{ *game }

func (*gameGitHub) Title() string { return "Put it on GitHub" }

var remoteRE = regexp.MustCompile(`github\.com(?::443)?[:/]([A-Za-z0-9-]+)/([A-Za-z0-9._-]+?)(\.git)?/?$`)

// origin is the GitHub repository the project is pushed to.
func (s *gameGitHub) origin() string {
	url, err := s.git("config", "--get", "remote.origin.url")
	if err != nil {
		return ""
	}
	if m := remoteRE.FindStringSubmatch(url); m != nil {
		return m[1] + "/" + m[2]
	}
	return ""
}

func (s *gameGitHub) Check(context.Context, *setup.State) (setup.Status, error) {
	repo := s.origin()
	if repo == "" {
		return setup.Status{Detail: "The agents fetch the project from GitHub and open their pull requests there."}, nil
	}
	head, err := s.git("rev-parse", "HEAD")
	if err != nil {
		return setup.Status{Detail: "Nothing is committed yet."}, nil
	}
	remote, err := s.git("ls-remote", "--heads", "origin")
	if err != nil {
		return setup.Status{Detail: "Git cannot reach " + repo + " on GitHub without asking for a password; the next part sets that up."}, nil
	}
	if !strings.Contains(remote, head) {
		return setup.Status{Detail: "The project is not pushed to " + repo + " yet."}, nil
	}
	s.repo = repo
	return setup.Status{Done: true, Detail: "On GitHub as " + repo + "."}, nil
}

func (s *gameGitHub) Run(_ context.Context, ui *setup.UI, st *setup.State) error {
	if s.origin() == "" {
		owner := ""
		if m := remoteRE.FindStringSubmatch("github.com/" + st.Repo); st.Repo != "" && m != nil {
			owner = m[1]
		}
		name := filepath.Base(s.dir)
		def := ""
		if owner != "" {
			def = owner + "/" + strings.ToLower(regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(name, "-"))
		}
		repo, err := ui.Ask("  The repository on GitHub (owner/name)", def, func(v string) error {
			if !regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`).MatchString(v) {
				return errors.New("write it as owner/name, such as you/my-game")
			}
			return nil
		})
		if err != nil {
			return err
		}
		if _, err := s.look("gh"); err == nil && exec.Command("gh", "auth", "status").Run() == nil {
			ok, err := ui.Confirm("  Make the private repository " + repo + " on GitHub with the gh command, and push?")
			if err != nil {
				return err
			}
			if !ok {
				return setup.ErrAborted
			}
			return s.interactive("gh", "repo", "create", repo, "--private", "--source", ".", "--remote", "origin", "--push")
		}
		ui.Say("  Make an empty repository on GitHub: open https://github.com/new, name it %s,", repo)
		ui.Say("  choose Private, and add no README, .gitignore or licence. Then come back here.")
		if _, err := ui.Ask("  Press Enter when it exists", "", nil); err != nil {
			return err
		}
		verb := "add"
		if _, err := s.git("config", "--get", "remote.origin.url"); err == nil {
			verb = "set-url" // an origin the guide cannot read
		}
		if out, err := s.git("remote", verb, "origin", "https://github.com/"+repo+".git"); err != nil {
			return fmt.Errorf("git remote %s: %v: %s", verb, err, out)
		}
	}
	if err := s.access(ui); err != nil {
		return err
	}
	if err := s.mergeRemote(ui); err != nil {
		return err
	}
	ui.Say("  Pushing to %s:", s.origin())
	if err := s.interactive("git", "push", "-u", "origin", "HEAD"); err != nil {
		return explain("the push did not succeed. Fix what Git said above and run `maestro new` again")
	}
	return nil
}

// mergeRemote takes in what the repository on GitHub already holds, such
// as the README GitHub adds when it makes a repository, so the push is
// not refused.
func (s *gameGitHub) mergeRemote(ui *setup.UI) error {
	branch, err := s.git("symbolic-ref", "--short", "HEAD")
	if err != nil {
		return nil
	}
	if out, err := s.git("fetch", "origin", branch); err != nil {
		if strings.Contains(out, "couldn't find remote ref") {
			return nil // an empty repository
		}
		return fmt.Errorf("git fetch: %s", out)
	}
	if _, err := s.git("merge-base", "--is-ancestor", "FETCH_HEAD", "HEAD"); err == nil {
		return nil
	}
	files, _ := s.git("ls-tree", "-r", "--name-only", "FETCH_HEAD")
	ui.Say("  The repository on GitHub already holds commits, with these files:")
	for i, f := range strings.Split(files, "\n") {
		if i == 8 {
			ui.Say("    and more")
			break
		}
		ui.Show("    %s", f)
	}
	ok, err := ui.Confirm("  Merge them into the project, keeping the project's own version of a file both have?")
	if err != nil {
		return err
	}
	if !ok {
		return setup.ErrAborted
	}
	if out, err := s.git("merge", "--allow-unrelated-histories", "--no-edit", "-X", "ours", "FETCH_HEAD"); err != nil {
		s.git("merge", "--abort")
		return explain("the merge did not succeed: %s\nMerge by hand (git pull --allow-unrelated-histories origin %s) and run `maestro new` again", out, branch)
	}
	return nil
}

// port443 moves an SSH remote to GitHub's SSH service on port 443 when the
// network lets nothing through on port 22, and reports whether git can
// reach the repository then.
func (s *gameGitHub) port443(ui *setup.UI, failure string) bool {
	url, _ := s.git("config", "--get", "remote.origin.url")
	if !strings.HasPrefix(url, "git@github.com:") && !strings.HasPrefix(url, "ssh://git@github.com/") {
		return false
	}
	if !strings.Contains(failure, "port 22") && !strings.Contains(failure, "Connection closed") && !strings.Contains(failure, "timed out") {
		return false
	}
	alt := "ssh://git@ssh.github.com:443/" + s.origin() + ".git"
	if _, err := s.git("remote", "set-url", "origin", alt); err != nil {
		return false
	}
	if _, err := s.git("ls-remote", "--heads", "origin"); err != nil {
		s.git("remote", "set-url", "origin", url)
		return false
	}
	ui.Say("  This network lets no SSH through on port 22, so Git now uses GitHub's SSH on port 443: %s", alt)
	return true
}

// access makes sure git can reach the repository without a password,
// which GitHub no longer takes: with the gh command's login, or with an
// SSH key.
func (s *gameGitHub) access(ui *setup.UI) error {
	out, err := s.git("ls-remote", "--heads", "origin")
	if err == nil {
		return nil
	}
	repo := s.origin()
	if s.port443(ui, out) {
		return nil
	}
	url, _ := s.git("config", "--get", "remote.origin.url")
	ui.Say("  Git cannot sign in to GitHub by itself yet. GitHub takes no passwords for Git, only a login")
	ui.Say("  through the gh command or an SSH key.")
	var options []string
	var acts []func() error
	if _, err := s.look("gh"); err == nil {
		options = append(options, "Use my GitHub login through gh (gh auth login, then gh auth setup-git)")
		acts = append(acts, func() error {
			if exec.Command("gh", "auth", "status").Run() != nil {
				if err := s.interactive("gh", "auth", "login", "--hostname", "github.com", "--git-protocol", "https"); err != nil {
					return err
				}
			}
			if err := s.interactive("gh", "auth", "setup-git"); err != nil {
				return err
			}
			if !strings.HasPrefix(url, "https://") {
				_, err := s.git("remote", "set-url", "origin", "https://github.com/"+repo+".git")
				return err
			}
			return nil
		})
	}
	keys, _ := filepath.Glob(filepath.Join(s.home, ".ssh", "id_*.pub"))
	if len(keys) > 0 {
		options = append(options, "Use my SSH key ("+tildePath(strings.TrimSuffix(keys[0], ".pub"), s.home)+")")
		acts = append(acts, func() error {
			if _, err := s.git("remote", "set-url", "origin", "git@github.com:"+repo+".git"); err != nil {
				return err
			}
			if out, err := s.git("ls-remote", "--heads", "origin"); err != nil {
				s.port443(ui, out)
			}
			return nil
		})
	}
	options = append(options, "I will set it up myself, then run maestro new again")
	i, err := ui.Choose("How should Git sign in to GitHub?", options)
	if err != nil {
		return err
	}
	if i == len(acts) {
		ui.Say("  The simplest way: install gh (on a Mac: brew install gh), then run")
		ui.Say("    gh auth login && gh auth setup-git")
		ui.Say("  Or add an SSH key to your GitHub account: https://docs.github.com/en/authentication/connecting-to-github-with-ssh")
		return setup.ErrAborted
	}
	if err := acts[i](); err != nil {
		return explain("that did not work (%v). Run `maestro new` again to try another way", err)
	}
	if out, err := s.git("ls-remote", "--heads", "origin"); err != nil {
		return explain("Git still cannot reach %s: %s\nIf the repository does not exist yet, make it on github.com first; then run `maestro new` again", repo, out)
	}
	return nil
}

// ---- 7. Maestro on this repository

type gameRepo struct{ *game }

func (*gameRepo) Title() string { return "Point Maestro at the repository" }

func (s *gameRepo) Check(_ context.Context, st *setup.State) (setup.Status, error) {
	if st.Repo != "" && strings.EqualFold(st.Repo, s.repo) {
		return setup.Status{Done: true, Detail: "Maestro on this machine works on " + st.Repo + "."}, nil
	}
	if st.Repo == "" {
		return setup.Status{Detail: "Maestro is not set up on this machine yet; the next steps do that, for " + s.repo + "."}, nil
	}
	return setup.Status{Detail: "Maestro on this machine works on " + st.Repo + ", not on " + s.repo + "."}, nil
}

func (s *gameRepo) Run(_ context.Context, ui *setup.UI, st *setup.State) error {
	if st.Repo == "" {
		st.Repo = s.repo
		return st.Save()
	}
	ui.Say("  This project runs agents for %s. To keep it as it is and give the game a project of its own,", st.Repo)
	ui.Say("  answer n and run `maestro new` again: it asks whether the game is a new project.")
	ui.Say("  Switching keeps the GitHub App: the next step gives it access to %s too. The settings file", s.repo)
	ui.Say("  of %s is kept under another name.", st.Repo)
	ok, err := ui.Confirm("  Switch this project to " + s.repo + "?")
	if err != nil {
		return err
	}
	if !ok {
		return setup.ErrAborted
	}
	kept, err := setup.SwitchRepo(st, s.repo)
	if err != nil {
		return explain("%v", err)
	}
	if kept != "" {
		ui.Say("  The old settings file is now %s.", kept)
	}
	return nil
}

// ---- 8. the packs on

type gameEnable struct{ *game }

func (*gameEnable) Title() string { return "Switch the packs on" }

func (s *gameEnable) conventions(st *setup.State) ([]string, error) {
	def, err := workflow.Load(st.WorkflowFile)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Build(def, config.Options{Env: os.Getenv, HomeDir: s.home})
	if err != nil {
		return nil, err
	}
	return cfg.Conventions, nil
}

func (s *gameEnable) Check(_ context.Context, st *setup.State) (setup.Status, error) {
	have, err := s.conventions(st)
	if err != nil {
		return setup.Status{Detail: "The settings file cannot be read: " + err.Error()}, nil
	}
	var off []string
	for _, n := range s.packs() {
		if !slices.Contains(have, n) {
			off = append(off, n)
		}
	}
	if len(off) > 0 {
		return setup.Status{Detail: "Off in the settings file: " + strings.Join(off, ", ") + ". Then the service runs the checks before it pushes, and protects the rule files."}, nil
	}
	return setup.Status{Done: true, Detail: "On in " + st.WorkflowFile + "."}, nil
}

func (s *gameEnable) Run(_ context.Context, ui *setup.UI, st *setup.State) error {
	for _, n := range s.packs() {
		p, err := conventions.Get(n)
		if err != nil {
			return err
		}
		var out strings.Builder
		if code := switchConventions(p, true, []string{"--file", st.WorkflowFile}, &out, &out); code != 0 {
			return errors.New(strings.TrimSpace(out.String()))
		}
		ui.Say("  %s is on.", n)
	}
	return nil
}

// ---- 8b. on a Mac with Apple silicon, Unity's checks run with its own Unity

type gameHostUnity struct{ *game }

func (*gameHostUnity) Title() string { return "Run Unity's checks with the Unity on this Mac" }

// needed: Unity's Editor for Linux crashes when it runs emulated, and on a
// Mac with Apple silicon emulated is the only way it runs.
func (s *gameHostUnity) needed() bool {
	return s.engine == "unity" && s.goos == "darwin" && s.arch == "arm64"
}

func (s *gameHostUnity) Check(_ context.Context, st *setup.State) (setup.Status, error) {
	if !s.needed() {
		return setup.Status{Done: true, Detail: "The checks run in the sandbox."}, nil
	}
	def, err := workflow.Load(st.WorkflowFile)
	if err != nil {
		return setup.Status{Detail: "The settings file cannot be read: " + err.Error()}, nil
	}
	cfg, err := config.Build(def, config.Options{Env: os.Getenv, HomeDir: s.home})
	if err != nil {
		return setup.Status{Detail: "The settings file cannot be read: " + err.Error()}, nil
	}
	want := projectVersion(s.dir)
	if e := cfg.Unity.Editor; e != "" && hostcheck.EditorVersion(e) == want {
		if _, err := os.Stat(e); err == nil {
			return setup.Status{Done: true, Detail: "The checks use Unity " + want + " from Unity Hub on this Mac."}, nil
		}
	}
	return setup.Status{Detail: "Unity's Editor for Linux does not run emulated on a Mac with Apple silicon, " +
		"so the service runs Unity's checks with the Unity that Unity Hub installed here."}, nil
}

func (s *gameHostUnity) Run(_ context.Context, ui *setup.UI, st *setup.State) error {
	want := projectVersion(s.dir)
	editor := (&gameFolder{s.game}).localEditor(want)
	for editor == "" {
		ui.Say("  Unity %s is not installed with Unity Hub on this Mac. In Unity Hub: Installs, Install Editor,", want)
		ui.Say("  then %s (under Archive if it is not listed).", want)
		ok, err := ui.Confirm("  Installed?")
		if err != nil {
			return err
		}
		if !ok {
			return setup.ErrAborted
		}
		editor = (&gameFolder{s.game}).localEditor(want)
	}
	ui.Say("  The service will run Unity's checks with %s.", tildePath(editor, s.home))
	ui.Say("  They run the agent's code on this Mac, fenced off by macOS's sandbox: no network")
	ui.Say("  but Unity's hosts, no writes outside their own folder and Unity's, and no reads")
	ui.Say("  of Maestro's keys. That fence is weaker than the container, so use it for your")
	ui.Say("  own projects only.")
	ok, err := ui.Confirm("  Write this into the settings file?")
	if err != nil {
		return err
	}
	if !ok {
		return setup.ErrAborted
	}
	if err := setUnityEditor(st.WorkflowFile, editor, s.home); err != nil {
		return err
	}
	if !hostcheck.HubLicence(s.home) {
		ui.Say("  Unity Hub has no licence on this Mac yet. Open Unity Hub, sign in, and add one (Preferences,")
		ui.Say("  Licenses, Add, Get a free personal licence). The checks need it.")
	}
	return nil
}

var unityBlockRE = regexp.MustCompile(`(?m)^unity:[^\n]*\n(?:[ \t]+[^\n]*\n|[ \t]*\n)*`)

// withUnityEditor sets unity.editor in the settings block of a settings file.
func withUnityEditor(text, editor string) (string, error) {
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return "", errors.New("the settings file does not start with a settings block (a line of three dashes)")
	}
	block := "unity:\n  editor: " + strconv.Quote(editor) + "\n"
	nl := strings.Index(text, "\n") + 1
	end := strings.Index(text[nl:], "\n---")
	if end < 0 {
		return "", errors.New("the settings block of the settings file does not end")
	}
	head := text[nl : nl+end+1]
	if loc := unityBlockRE.FindStringIndex(head); loc != nil {
		return text[:nl] + head[:loc[0]] + block + head[loc[1]:] + text[nl+end+1:], nil
	}
	return text[:nl] + block + text[nl:], nil
}

// setUnityEditor writes unity.editor into the settings file, and keeps the
// old file when the new one does not read as intended.
func setUnityEditor(path, editor, home string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text, err := withUnityEditor(string(raw), editor)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(text), info.Mode().Perm()); err != nil {
		return err
	}
	defer os.Remove(tmp)
	def, err := workflow.Load(tmp)
	if err == nil {
		var cfg *config.Config
		if cfg, err = config.Build(def, config.Options{Env: os.Getenv, HomeDir: home}); err == nil && cfg.Unity.Editor != editor {
			err = errors.New("the change did not take effect")
		}
	}
	if err != nil {
		return fmt.Errorf("the settings file was left as it was, because the changed one is not valid: %w", err)
	}
	return os.Rename(tmp, path)
}

// ---- 10. the Unity licence (after the image: Unity activates inside it)

type gameLicence struct{ *game }

func (*gameLicence) Title() string { return "Give the checks a Unity licence" }

func (s *gameLicence) Check(context.Context, *setup.State) (setup.Status, error) {
	if s.engine != "unity" {
		return setup.Status{Done: true, Detail: "Godot needs no licence."}, nil
	}
	if (&gameHostUnity{s.game}).needed() {
		if hostcheck.HubLicence(s.home) {
			return setup.Status{Done: true, Detail: "The checks use Unity Hub's licence on this Mac."}, nil
		}
		return setup.Status{Detail: "The checks use Unity Hub's licence on this Mac, and there is none yet."}, nil
	}
	if unityStore(s.setupDir).Has() {
		return setup.Status{Done: true, Detail: "This machine has a Unity licence for the checks."}, nil
	}
	return setup.Status{Detail: "Unity's tests need a licence. It goes to the checks only, never to the agent."}, nil
}

// licenceFiles are the places Unity Hub leaves an activated licence file.
func (s *gameLicence) licenceFiles() []string {
	var places []string
	switch s.goos {
	case "darwin":
		places = []string{"/Library/Application Support/Unity/Unity_lic.ulf",
			filepath.Join(s.home, "Library", "Application Support", "Unity", "Unity_lic.ulf")}
	case "windows":
		places = []string{filepath.Join(os.Getenv("PROGRAMDATA"), "Unity", "Unity_lic.ulf")}
	default:
		places = []string{filepath.Join(s.home, ".local", "share", "unity3d", "Unity", "Unity_lic.ulf")}
	}
	var out []string
	for _, p := range places {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			out = append(out, p)
		}
	}
	return out
}

func (s *gameLicence) Run(_ context.Context, ui *setup.UI, st *setup.State) error {
	if (&gameHostUnity{s.game}).needed() {
		for !hostcheck.HubLicence(s.home) {
			ui.Say("  Open Unity Hub, sign in, and add a licence: Preferences, Licenses, Add, Get a free personal licence.")
			ok, err := ui.Confirm("  Done?")
			if err != nil {
				return err
			}
			if !ok {
				return setup.ErrAborted
			}
		}
		return nil
	}
	store := unityStore(s.setupDir)
	for {
		if files := s.licenceFiles(); len(files) > 0 {
			ui.Say("  Unity Hub left a licence file on this machine: %s", files[0])
			ui.Say("  Use an account that holds nothing else: the checks run the agent's code, which can read it.")
			ok, err := ui.Confirm("  Give it to the checks?")
			if err != nil {
				return err
			}
			if ok {
				if err := store.Import(files[0]); err != nil {
					return err
				}
				return nil
			}
		}
		// Unity 6's Hub keeps an entitlement licence bound to this machine,
		// which a Linux container cannot use, so Hub is no way in.
		i, err := ui.Choose("How should the checks get a licence?", []string{
			"Sign in to Unity in my browser (two-step verification works)",
			"With my Unity account's email and password (no two-step verification)",
			"From a licence file I have (Unity_lic.ulf)",
		})
		if err != nil {
			return err
		}
		switch i {
		case 0, 1:
			mode := activateBrowser
			if i == 1 {
				mode = activatePassword
			}
			if code := unityActivate(s.setupDir, st.WorkflowFile, store, mode, s.rawIn, s.stdout, s.stderr); code != 0 {
				ui.Say("  The activation did not succeed; the reason is above. Choose again, or stop with Ctrl-C.")
				continue
			}
			return nil
		case 2:
			ans, err := ui.Ask("  The licence file", "", nil)
			if err != nil {
				return err
			}
			if err := store.Import(expandHome(ans, s.home)); err != nil {
				ui.Say("  %v", err)
				continue
			}
			return nil
		}
	}
}

// ---- 9. the image

type gameImage struct{ *game }

func (*gameImage) Title() string { return "Build the sandbox image" }

func (s *gameImage) Check(ctx context.Context, st *setup.State) (setup.Status, error) {
	in, err := open(s.setupDir, st.WorkflowFile, nil)
	if err != nil {
		return setup.Status{Detail: "The settings cannot be read: " + err.Error()}, nil
	}
	have, ok := in.box.ImageLabel(ctx, imagePacksLabel)
	if !ok {
		return setup.Status{Detail: "The agents work in a container made from this image; it is not built yet."}, nil
	}
	got := strings.Split(have, ",")
	for _, n := range s.packs() {
		if !slices.Contains(got, n) {
			return setup.Status{Detail: "The image has no tools for " + n + " yet; it is built again with them."}, nil
		}
	}
	if packs, err := conventions.Resolve(in.cfg.Conventions); err == nil {
		extra := imageExtra(packs, len(in.cfg.Screenshots.Pages) > 0 || in.cfg.Tracker.DesignState != "")
		if lines, _ := in.box.ImageLabel(ctx, imageLinesLabel); lines != linesHash(extra) {
			return setup.Status{Detail: "This version of Maestro puts more in the image; it is built again."}, nil
		}
	}
	return setup.Status{Done: true, Detail: "The image has the tools of " + strings.Join(s.packs(), ", ") + "."}, nil
}

func (s *gameImage) Run(_ context.Context, ui *setup.UI, st *setup.State) error {
	if s.engine == "unity" {
		ui.Say("  The Unity Editor makes the image about 8 GB, so the first build takes a while.")
		if s.arch == "arm64" {
			ui.Say("  Unity's Editor for Linux exists for Intel only, so on this Mac the agent's container runs")
			ui.Say("  emulated, which is slower. Unity's own checks run with the Unity on this Mac.")
		}
	}
	ok, err := ui.Confirm("  Build it now?")
	if err != nil {
		return err
	}
	if !ok {
		return setup.ErrAborted
	}
	if code := runImage([]string{"--dir", s.setupDir, "--file", st.WorkflowFile}, s.stdout, s.stderr); code != 0 {
		return errors.New("the image was not built; the reason is above")
	}
	return nil
}
