// Package setup is the guided installation behind `maestro setup` (SPEC
// section 16.8).
//
// The wizard is a list of steps. Each step can tell whether it is already
// done, so the wizard can be stopped and run again at any point: finished
// steps are confirmed in one line and skipped. A step explains what it is
// about to change and waits for the operator before it changes anything.
package setup

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// UI is how the wizard talks to the operator.
type UI struct {
	In  *bufio.Reader
	Out io.Writer

	// Styled switches on colour and drawing. Width is the width of the
	// terminal, and Raw switches the terminal to single key presses and
	// returns how to switch back. Both are only used when Styled is set,
	// and both may be nil.
	Styled bool
	Width  func() int
	Raw    func() (restore func(), err error)

	inStep bool
	hang   int // indent of the text of the list item being printed
}

// NewUI wraps a terminal's input and output.
func NewUI(in io.Reader, out io.Writer) *UI { return &UI{In: bufio.NewReader(in), Out: out} }

// Say prints one line.
func (u *UI) Say(format string, a ...any) {
	if u.Styled {
		u.say(fmt.Sprintf(format, a...))
		return
	}
	fmt.Fprintf(u.Out, format+"\n", a...)
}

// ErrAborted is returned when the operator declines or the input ends.
var ErrAborted = errors.New("setup stopped; run `maestro setup` again to continue where you left off")

func (u *UI) line() (string, error) {
	s, err := u.In.ReadString('\n')
	if err != nil && s == "" {
		return "", ErrAborted
	}
	return strings.TrimSpace(s), nil
}

// Ask asks for a value. An empty answer takes the default. The answer is
// checked by valid, and the question is repeated until it passes.
func (u *UI) Ask(question, def string, valid func(string) error) (string, error) {
	if u.Styled {
		hint := ""
		if def != "" {
			hint = "Enter for " + def
		}
		u.question(question, hint)
		fmt.Fprintln(u.Out)
	}
	for {
		switch {
		case u.Styled:
			fmt.Fprint(u.Out, prompt)
		case def != "":
			fmt.Fprintf(u.Out, "%s [%s]: ", question, def)
		default:
			fmt.Fprintf(u.Out, "%s: ", question)
		}
		ans, err := u.line()
		if err != nil {
			return "", err
		}
		if ans == "" {
			ans = def
			if u.Styled && def != "" && u.Raw != nil {
				// Show what Enter stood for, on the line it was pressed on.
				fmt.Fprintf(u.Out, "\x1b[1A%s%s\n", prompt, def)
			}
		}
		if valid != nil {
			if err := valid(ans); err != nil {
				if u.Styled {
					fmt.Fprintf(u.Out, "  %s✖%s  %v\n", red, reset, err)
				} else {
					u.Say("  %v", err)
				}
				continue
			}
		}
		return ans, nil
	}
}

// Confirm asks a yes or no question. Only an explicit yes continues.
func (u *UI) Confirm(question string) (bool, error) {
	if u.Styled && u.Raw != nil {
		return u.confirmKey(question)
	}
	if u.Styled {
		u.question(question, "y / n")
		fmt.Fprint(u.Out, "\n"+prompt)
		return u.confirmLine()
	}
	for {
		fmt.Fprintf(u.Out, "%s [y/n]: ", question)
		ans, err := u.line()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(ans) {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
	}
}

// State is what the wizard remembers between runs. It holds no secret: the
// App's key is a separate file, and this file only says where it is.
type State struct {
	Repo string `json:"repo,omitempty"`
	// OwnerType is "Organization" or "User". An organization gets a board,
	// a personal account labels. Empty in a state written before boards.
	OwnerType      string `json:"owner_type,omitempty"`
	AppID          string `json:"app_id,omitempty"`
	AppSlug        string `json:"app_slug,omitempty"`
	AppKeyFile     string `json:"app_key_file,omitempty"`
	InstallationID string `json:"installation_id,omitempty"`
	Worker         string `json:"worker,omitempty"`
	WorkflowFile   string `json:"workflow_file,omitempty"`
	// Author is the GitHub login whose issues are worked on, and Board the
	// number of the board. Both are only set for an organization.
	Author string `json:"author,omitempty"`
	Board  int    `json:"board,omitempty"`

	// Tracker is where the tickets are kept: TrackerLinear, or GitHub when
	// empty or TrackerGitHub. With Linear, LinearTeam is the team's key,
	// LinearAuthor the e-mail address of the person who approves, and the
	// last two are the team's names for the hand-off and closed states.
	Tracker        string   `json:"tracker,omitempty"`
	LinearTeam     string   `json:"linear_team,omitempty"`
	LinearAuthor   string   `json:"linear_author,omitempty"`
	LinearHandoff  string   `json:"linear_handoff,omitempty"`
	LinearTerminal []string `json:"linear_terminal,omitempty"`
	// Sandbox is SandboxSystemd when the steps run in systemd units
	// instead of containers (`maestro setup --sandbox systemd`, Linux only).
	Sandbox string `json:"sandbox,omitempty"`

	// Host is HostGitLab when the code is on GitLab (`maestro setup --host
	// gitlab`); empty is GitHub. HostURL is the GitLab instance, GitLabBot
	// the user of the project access token, GitLabProject the project's
	// number, and Reviewer the GitLab user whose reviews and comments count.
	Host          string `json:"host,omitempty"`
	HostURL       string `json:"host_url,omitempty"`
	GitLabBot     string `json:"gitlab_bot,omitempty"`
	GitLabProject int64  `json:"gitlab_project,omitempty"`
	Reviewer      string `json:"reviewer,omitempty"`

	// TakenWorkers are the worker names of the other projects on this
	// machine. Every worker has its own containers, volumes and service, so
	// two projects may not share one. They are not saved.
	TakenWorkers []string `json:"-"`

	path string
}

// checkWorker is validWorker that also refuses the names of the other
// projects on this machine.
func (s *State) checkWorker(name string) error {
	if err := validWorker(name); err != nil {
		return err
	}
	for _, t := range s.TakenWorkers {
		if t == name {
			return fmt.Errorf("another project on this machine already uses %s; each project needs its own name", name)
		}
	}
	return nil
}

// LoadState reads the state from dir, which is created if needed. A
// directory that other users can write to is refused: it will hold the key.
func LoadState(dir string) (*State, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("the setup directory must be an absolute path")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%s is writable by other users (mode %04o); run: chmod 700 %s", dir, fi.Mode().Perm(), dir)
	}
	s := &State{path: filepath.Join(dir, "setup.json")}
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, s); err != nil {
		return nil, fmt.Errorf("%s is damaged: %v. Move it away to start over", s.path, err)
	}
	return s, nil
}

// OnBoard reports whether tickets are approved on a board instead of with
// labels.
func (s *State) OnBoard() bool { return s.OwnerType == kindOrg }

// Dir is the directory the state lives in.
func (s *State) Dir() string { return filepath.Dir(s.path) }

// Save writes the state atomically.
func (s *State) Save() error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Status is what a step reports about itself.
type Status struct {
	Done   bool
	Detail string // one line: what was found
}

// Step is one stage of the installation.
type Step interface {
	Title() string
	// Check reports whether the step is already complete. It changes nothing.
	Check(ctx context.Context, st *State) (Status, error)
	// Run performs the step, asking before each change.
	Run(ctx context.Context, ui *UI, st *State) error
}

// Run walks the steps in order. A step that is done is confirmed and
// skipped. After a step runs, it is checked again, and the wizard stops if
// it still is not done.
func Run(ctx context.Context, ui *UI, st *State, steps []Step) error {
	if ui.Styled {
		return runStyled(ctx, ui, st, steps)
	}
	for i, step := range steps {
		ui.Say("")
		ui.Say("Step %d of %d: %s", i+1, len(steps), step.Title())
		status, err := step.Check(ctx, st)
		if err != nil {
			return fmt.Errorf("%s: %w", step.Title(), err)
		}
		if status.Done {
			ui.Say("  Already done. %s", status.Detail)
			continue
		}
		if status.Detail != "" {
			ui.Say("  %s", status.Detail)
		}
		if err := step.Run(ctx, ui, st); err != nil {
			return err
		}
		if err := st.Save(); err != nil {
			return err
		}
		status, err = step.Check(ctx, st)
		if err != nil {
			return fmt.Errorf("%s: %w", step.Title(), err)
		}
		if !status.Done {
			ui.Say("  Not finished yet. %s", status.Detail)
			return ErrAborted
		}
		ui.Say("  Done. %s", status.Detail)
	}
	ui.Say("")
	ui.Say("All %d steps are complete.", len(steps))
	return nil
}

// runStyled is Run as it looks in a terminal. A step that is already done
// takes one line; the others open up.
func runStyled(ctx context.Context, ui *UI, st *State, steps []Step) error {
	check := func(step Step) (Status, error) {
		stop := ui.Waiting("Checking: " + step.Title())
		defer stop()
		status, err := step.Check(ctx, st)
		if err != nil {
			return status, fmt.Errorf("%s: %w", step.Title(), err)
		}
		return status, nil
	}
	fmt.Fprintln(ui.Out)
	for i, step := range steps {
		status, err := check(step)
		if err != nil {
			return err
		}
		if status.Done {
			ui.stepEnd(green, "✔", step.Title()+dim+"  ·  "+status.Detail+reset)
			continue
		}
		ui.stepHead(i+1, len(steps), step.Title())
		if status.Detail != "" {
			ui.Say("  %s", status.Detail)
		}
		err = step.Run(ctx, ui, st)
		if err == nil {
			err = st.Save()
		}
		if err != nil {
			ui.inStep = false
			return err
		}
		if status, err = check(step); err != nil {
			ui.inStep = false
			return err
		}
		if !status.Done {
			ui.stepEnd(yellow, "▲", "Not finished yet. "+status.Detail)
			return ErrAborted
		}
		ui.stepEnd(green, "✔", "Done. "+status.Detail)
	}
	return nil
}

// SwitchRepo points the installation at another repository. The App stays:
// it is installed on that repository too in the next step. The settings
// file of the old repository is kept under another name, and a new one is
// written for the new repository. Only a repository of the same owner can
// be switched to, because the App belongs to that owner.
func SwitchRepo(st *State, repo string) (kept string, err error) {
	if err := validRepo(repo); err != nil {
		return "", err
	}
	if st.Repo == repo {
		return "", nil
	}
	if st.Repo != "" {
		was, now := repoRE.FindStringSubmatch(st.Repo), repoRE.FindStringSubmatch(repo)
		if !strings.EqualFold(was[1], now[1]) {
			return "", fmt.Errorf("the App on this machine belongs to %s, and %s belongs to %s. "+
				"Put the repository under %s, or give it a project of its own: maestro projects add", was[1], repo, now[1], was[1])
		}
	}
	if st.WorkflowFile != "" {
		if _, err := os.Stat(st.WorkflowFile); err == nil {
			old := repoRE.FindStringSubmatch(st.Repo)
			kept = strings.TrimSuffix(st.WorkflowFile, ".md") + "." + old[2] + ".md"
			if _, err := os.Lstat(kept); err == nil {
				return "", fmt.Errorf("%s already exists; move it away first", kept)
			}
			if err := os.Rename(st.WorkflowFile, kept); err != nil {
				return "", err
			}
		}
	}
	st.Repo, st.InstallationID, st.WorkflowFile = repo, "", ""
	return kept, st.Save()
}
