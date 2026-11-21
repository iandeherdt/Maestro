// Package setup is the guided installation behind `symphony setup` (SPEC
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
}

// NewUI wraps a terminal's input and output.
func NewUI(in io.Reader, out io.Writer) *UI { return &UI{In: bufio.NewReader(in), Out: out} }

// Say prints one line.
func (u *UI) Say(format string, a ...any) { fmt.Fprintf(u.Out, format+"\n", a...) }

// ErrAborted is returned when the operator declines or the input ends.
var ErrAborted = errors.New("setup stopped; run `symphony setup` again to continue where you left off")

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
	for {
		if def != "" {
			fmt.Fprintf(u.Out, "%s [%s]: ", question, def)
		} else {
			fmt.Fprintf(u.Out, "%s: ", question)
		}
		ans, err := u.line()
		if err != nil {
			return "", err
		}
		if ans == "" {
			ans = def
		}
		if valid != nil {
			if err := valid(ans); err != nil {
				u.Say("  %v", err)
				continue
			}
		}
		return ans, nil
	}
}

// Confirm asks a yes or no question. Only an explicit yes continues.
func (u *UI) Confirm(question string) (bool, error) {
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
	Repo           string `json:"repo,omitempty"`
	AppID          string `json:"app_id,omitempty"`
	AppSlug        string `json:"app_slug,omitempty"`
	AppKeyFile     string `json:"app_key_file,omitempty"`
	InstallationID string `json:"installation_id,omitempty"`

	path string
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
