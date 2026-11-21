package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"

	"github.com/iandeherdt/Symphony-beads/internal/config"
	"github.com/iandeherdt/Symphony-beads/internal/workflow"
)

const workflowFileName = "WORKFLOW.md"

// WorkflowFile writes the workflow file from a few answers and validates
// it with the same code the service uses at startup.
type WorkflowFile struct {
	GOOS string
}

func (*WorkflowFile) Title() string { return "Write the workflow file" }

// validate loads a workflow file exactly as the service would.
func validate(path string) error {
	def, err := workflow.Load(path)
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	_, err = config.Build(def, config.Options{Env: os.Getenv, HomeDir: home})
	return err
}

func (w *WorkflowFile) Check(_ context.Context, st *State) (Status, error) {
	if st.WorkflowFile == "" {
		return Status{Detail: "The workflow file holds the settings and the standing instructions for the agent."}, nil
	}
	if err := validate(st.WorkflowFile); err != nil {
		return Status{Detail: st.WorkflowFile + " is not valid: " + err.Error()}, nil
	}
	return Status{Done: true, Detail: st.WorkflowFile + " is valid."}, nil
}

func validCommand(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("a verify command is required: it is how the service checks the agent's work")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return errors.New("write the command on one line; you can edit the file later for more")
		}
	}
	return nil
}

func (w *WorkflowFile) Run(_ context.Context, ui *UI, st *State) error {
	m := repoRE.FindStringSubmatch(st.Repo)
	if m == nil || !slugRE.MatchString(st.AppSlug) || !workerRE.MatchString(st.Worker) {
		return errors.New("the earlier steps have to be finished first")
	}
	path := filepath.Join(st.Dir(), workflowFileName)
	if _, err := os.Lstat(path); err == nil {
		// Never overwrite a file the operator may have edited.
		if err := validate(path); err != nil {
			return fmt.Errorf("%s exists but is not valid: %w. Fix it or move it away, then run setup again", path, err)
		}
		st.WorkflowFile = path
		return nil
	}
	goos := w.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	ui.Say("  After the agent finishes, the service runs a command of yours to decide whether the work")
	ui.Say("  is acceptable, for example: npm test   or: go test ./...   or: make check")
	verify, err := ui.Ask("  Verify command", "", validCommand)
	if err != nil {
		return err
	}
	verify = strings.TrimSpace(verify)
	text := render(goos, st, m[1], verify)
	ui.Say("  This file will be written to %s:", path)
	ui.Say("")
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		ui.Say("    %s", line)
	}
	ui.Say("")
	ok, err := ui.Confirm("  Write it?")
	if err != nil {
		return err
	}
	if !ok {
		return ErrAborted
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(text)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	st.WorkflowFile = path
	return nil
}

// render builds the file. Every value placed in it has been validated to
// a narrow character set, except the verify command, which goes into a
// YAML block where no character is special.
func render(goos string, st *State, owner, verify string) string {
	backend, root := "systemd", "/var/lib/symphony-agent/workspaces"
	stateDir := "/var/lib/symphony/state"
	if goos == "darwin" {
		backend, root = "macos-vm", "/Users/symphony-agent/workspaces"
		stateDir = filepath.Join(st.Dir(), "state")
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("tracker:\n  kind: github\n  provider:\n")
	b.WriteString("    repo: " + st.Repo + "\n")
	b.WriteString("    state_source: labels\n")
	b.WriteString("    bot_login: \"" + st.AppSlug + "[bot]\"\n")
	b.WriteString("  allowed_authors: [" + owner + "]\n")
	b.WriteString("workspace:\n  root: " + root + "\n  repo: " + st.Repo + "\n")
	b.WriteString("sandbox:\n  backend: " + backend + "\n  network:\n    proxy: 127.0.0.1:3128\n")
	b.WriteString("verify:\n  command: |\n    " + verify + "\n")
	b.WriteString("budget:\n  state_dir: " + stateDir + "\n")
	b.WriteString("worker:\n  name: " + st.Worker + "\n")
	b.WriteString("---\n\n")
	b.WriteString("Implement the issue below in this repository.\n\n")
	b.WriteString("Follow the conventions in CLAUDE.md if the repository has one. Add or update tests for every\n")
	b.WriteString("behaviour you change, and run the test suite before you finish.\n")
	return b.String()
}
