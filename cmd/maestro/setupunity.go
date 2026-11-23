package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/conventions"
	"github.com/iandeherdt/Maestro/internal/ghapp"
	"github.com/iandeherdt/Maestro/internal/hostcheck"
	"github.com/iandeherdt/Maestro/internal/setup"
	"github.com/iandeherdt/Maestro/internal/workflow"
)

// setupUnity is the last step of `maestro setup` for a repository that
// holds a Unity project: it switches the unity pack on, and on a Mac with
// Apple silicon it points the checks at the Unity Editor that Unity Hub
// installed and makes sure Hub has a licence. `maestro new` does the same
// for a game it starts; this is for a repository that already exists.
type setupUnity struct {
	home string
	goos string
	arch string
	// version reads the Unity version the repository's project is saved
	// with: "" when the repository holds no Unity project. Replaced by tests.
	version func(ctx context.Context, st *setup.State) (string, error)

	known bool // version was read
	want  string
}

func newSetupUnity() *setupUnity {
	home, _ := os.UserHomeDir()
	return &setupUnity{home: home, goos: runtime.GOOS, arch: runtime.GOARCH, version: repoUnityVersion}
}

func (*setupUnity) Title() string { return "Set up Unity's checks" }

// onMac: Unity's Editor for Linux crashes when it runs emulated, and on a
// Mac with Apple silicon emulated is the only way it runs, so the checks
// run with the Unity on the Mac.
func (s *setupUnity) onMac() bool { return s.goos == "darwin" && s.arch == "arm64" }

func (s *setupUnity) project(ctx context.Context, st *setup.State) (string, error) {
	if !s.known {
		v, err := s.version(ctx, st)
		if err != nil {
			return "", err
		}
		if v == "unknown" {
			// A ProjectVersion.txt without a version: take the one the
			// checks are made for.
			v = unityVersion()
		}
		s.want, s.known = v, true
	}
	return s.want, nil
}

func (s *setupUnity) settings(st *setup.State) (*config.Config, error) {
	def, err := workflow.Load(st.WorkflowFile)
	if err != nil {
		return nil, err
	}
	return config.Build(def, config.Options{Env: os.Getenv, HomeDir: s.home})
}

// editor is the Editor of the version as Unity Hub installed it here.
func (s *setupUnity) editor(version string) string {
	f := &gameFolder{&game{home: s.home, goos: s.goos}}
	return f.localEditor(version)
}

func (s *setupUnity) Check(ctx context.Context, st *setup.State) (setup.Status, error) {
	if st.WorkflowFile == "" {
		return setup.Status{Detail: "The settings file has to be written first."}, nil
	}
	want, err := s.project(ctx, st)
	if err != nil {
		return setup.Status{}, fmt.Errorf("reading the Unity version of %s: %w", st.Repo, err)
	}
	if want == "" {
		return setup.Status{Done: true, Detail: st.Repo + " holds no Unity project, so there is nothing to do."}, nil
	}
	cfg, err := s.settings(st)
	if err != nil {
		return setup.Status{Detail: "The settings file cannot be read: " + err.Error()}, nil
	}
	if !slices.Contains(cfg.Conventions, "unity") {
		return setup.Status{Detail: st.Repo + " is a Unity project (" + want + "), and the unity pack is off. " +
			"With it on, the service runs Unity's checks before it pushes."}, nil
	}
	if !s.onMac() {
		return setup.Status{Done: true, Detail: "The unity pack is on. Unity's checks run in the sandbox; give them a licence with `maestro unity activate`."}, nil
	}
	if e := cfg.Unity.Editor; e == "" || hostcheck.EditorVersion(e) != want {
		return setup.Status{Detail: "On a Mac with Apple silicon, Unity's checks run with Unity " + want + " from Unity Hub, and the settings do not name it yet."}, nil
	} else if st, err := os.Stat(e); err != nil || st.IsDir() {
		return setup.Status{Detail: "The settings name " + e + " for Unity's checks, and it is not there."}, nil
	}
	if !hostcheck.HubLicence(s.home) {
		return setup.Status{Detail: "Unity Hub has no licence on this Mac yet, and Unity's checks need one."}, nil
	}
	return setup.Status{Done: true, Detail: "Unity's checks use Unity " + want + " and Unity Hub's licence on this Mac."}, nil
}

func (s *setupUnity) Run(ctx context.Context, ui *setup.UI, st *setup.State) error {
	want, err := s.project(ctx, st)
	if err != nil || want == "" {
		return err
	}
	cfg, err := s.settings(st)
	if err != nil {
		return err
	}
	if !slices.Contains(cfg.Conventions, "unity") {
		ui.Say("  %s is a Unity project, saved with Unity %s.", st.Repo, want)
		ui.Say("  The unity pack makes the service check every change with Unity before it pushes:")
		ui.Say("  the .meta files, a compile of every script and the project's tests.")
		if img := unityVersion(); img != "" && img != want {
			ui.Say("  Note: the checks are made for Unity %s, and the project is saved with %s.", img, want)
		}
		ok, err := ui.Confirm("  Switch the unity pack on?")
		if err != nil {
			return err
		}
		if !ok {
			return setup.ErrAborted
		}
		p, err := conventions.Get("unity")
		if err != nil {
			return err
		}
		var out strings.Builder
		if code := switchConventions(p, true, []string{"--file", st.WorkflowFile}, &out, &out); code != 0 {
			return errors.New(strings.TrimSpace(out.String()))
		}
		ui.Say("  The unity pack is on.")
	}
	if !s.onMac() {
		ui.Say("  After `maestro image`, give the checks a Unity licence with `maestro unity activate`.")
		return nil
	}
	if cfg, err = s.settings(st); err != nil {
		return err
	}
	if e := cfg.Unity.Editor; e == "" || hostcheck.EditorVersion(e) != want || !isFile(e) {
		editor := s.editor(want)
		for editor == "" {
			ui.Say("  On a Mac with Apple silicon, Unity's Editor for Linux does not run, so the service runs")
			ui.Say("  Unity's checks with the Unity that Unity Hub installs here. Unity %s is not installed yet.", want)
			ui.Say("  In Unity Hub: Installs, Install Editor, then %s (under Archive if it is not listed).", want)
			ui.Say("  Without an administrator's password, first set Hub's Settings, Installs, to a folder in")
			ui.Say("  your home folder, such as %s.", tildePath(s.home+"/Applications/Unity/Hub/Editor", s.home))
			ok, err := ui.Confirm("  Installed?")
			if err != nil {
				return err
			}
			if !ok {
				return setup.ErrAborted
			}
			editor = s.editor(want)
		}
		ui.Say("  The service will run Unity's checks with %s.", tildePath(editor, s.home))
		ui.Say("  They run the agent's code on this Mac, as this user, fenced off by macOS's sandbox: no")
		ui.Say("  network but Unity's hosts, no writes outside their own folder and Unity's, and no reads")
		ui.Say("  of Maestro's keys. That fence is weaker than the container, so run the service as a user")
		ui.Say("  that holds nothing else, and use it for your own projects only.")
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
	}
	for !hostcheck.HubLicence(s.home) {
		ui.Say("  Unity Hub has no licence on this Mac yet. Open Unity Hub, sign in, then Settings, Licenses,")
		ui.Say("  Add, Get a free personal licence. Code the agent writes can read that licence during the")
		ui.Say("  checks, so use a Unity account made for this machine that holds nothing else.")
		ok, err := ui.Confirm("  Added it?")
		if err != nil {
			return err
		}
		if !ok {
			return setup.ErrAborted
		}
	}
	return nil
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

var editorVersionRE = regexp.MustCompile(`(?m)^m_EditorVersion:\s*(\S+)`)

// repoUnityVersion reads ProjectSettings/ProjectVersion.txt from the
// repository's default branch with the App's token.
func repoUnityVersion(ctx context.Context, st *setup.State) (string, error) {
	owner, name, ok := strings.Cut(st.Repo, "/")
	if !ok || owner == "" || name == "" {
		return "", fmt.Errorf("%q is not owner/name", st.Repo)
	}
	app, err := ghapp.New(ghapp.Config{AppID: st.AppID, InstallationID: st.InstallationID,
		KeyFile: st.AppKeyFile, APIURL: gitHub.api})
	if err != nil {
		return "", err
	}
	src, err := app.Source(ghapp.Scope{Repositories: []string{name}, Permissions: map[string]string{"contents": "read"}})
	if err != nil {
		return "", err
	}
	token, err := src.Token(ctx)
	if err != nil {
		return "", err
	}
	return fetchUnityVersion(ctx, gitHub.api, st.Repo, token)
}

func fetchUnityVersion(ctx context.Context, apiURL, repo, token string) (string, error) {
	u := apiURL + "/repos/" + repo + "/contents/" + url.PathEscape("ProjectSettings") + "/" + url.PathEscape("ProjectVersion.txt")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github.raw+json")
	hc := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", nil
	default:
		return "", fmt.Errorf("GitHub answered %d", res.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return "", err
	}
	if m := editorVersionRE.FindSubmatch(raw); m != nil {
		return string(m[1]), nil
	}
	return "unknown", nil
}
