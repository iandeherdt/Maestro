package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/gitea"
	"github.com/iandeherdt/Maestro/internal/gitlab"
	"github.com/iandeherdt/Maestro/internal/workflow"
)

const workflowFileName = "WORKFLOW.md"

// noTests is the verify command written when the operator has none.
const noTests = "true"

// WorkflowFile writes the workflow file from a few answers and validates
// it with the same code the service uses at startup.
type WorkflowFile struct {
	GOOS string
	// LookPath finds a program. Default exec.LookPath.
	LookPath func(string) (string, error)
	// PackTests, when set, names the conventions pack's own test command.
	// The service runs that by itself, so there is nothing to ask.
	PackTests func() string
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
	for _, r := range s {
		if unicode.IsControl(r) {
			return errors.New("write the command on one line; you can edit the file later for more")
		}
	}
	return nil
}

func (w *WorkflowFile) Run(_ context.Context, ui *UI, st *State) error {
	m := repoRE.FindStringSubmatch(st.Repo)
	switch {
	case st.UsesLinear():
		if !teamKeyRE.MatchString(st.LinearTeam) || !emailRE.MatchString(st.LinearAuthor) || validStateName(st.LinearHandoff) != nil {
			return errors.New("the earlier steps have to be finished first")
		}
	case st.OwnerType == kindOrg && (!authorRE.MatchString(st.Author) || st.Board <= 0):
		return errors.New("the earlier steps have to be finished first")
	}
	owner := ""
	switch {
	case st.UsesGitLab():
		if !gitlab.ValidPath(st.Repo) || st.GitLabBot == "" || st.Reviewer == "" || !st.UsesLinear() || !workerRE.MatchString(st.Worker) {
			return errors.New("the earlier steps have to be finished first")
		}
	case st.UsesGitea():
		if !gitea.ValidRepo(st.Repo) || st.GiteaBot == "" || st.Reviewer == "" || st.HostURL == "" || !st.UsesLinear() || !workerRE.MatchString(st.Worker) {
			return errors.New("the earlier steps have to be finished first")
		}
	case m == nil || !slugRE.MatchString(st.AppSlug) || !workerRE.MatchString(st.Worker):
		return errors.New("the earlier steps have to be finished first")
	default:
		owner = m[1]
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
	packTests := ""
	if w.PackTests != nil {
		packTests = w.PackTests()
	}
	var verify string
	if packTests != "" {
		verify = noTests // config runs the packs' checks in its place
		ui.Say("  The tests: the service runs %s itself before it publishes anything,", packTests)
		ui.Say("  so there is no test command to give. Your own command can go in verify.command later.")
	} else {
		ui.Say("  One question: how do you test %s?", st.Repo)
		ui.Say("  When the agent says it is done, the service runs your test command itself, in the agent's")
		ui.Say("  copy of the repository. If the command fails, the work is not published and the agent")
		ui.Say("  gets the output to fix it. Type the command you would run yourself, for example:")
		ui.Say("      npm test        go test ./...        make check")
		ui.Say("  If the repository has no tests yet, just press Enter.")
		answer, err := ui.Ask("  Test command", "", validCommand)
		if err != nil {
			return err
		}
		verify = strings.TrimSpace(answer)
		if verify == "" {
			// "true" always succeeds. The file must hold a command, and a
			// first try on an empty repository should not need one.
			verify = noTests
			ui.Say("  No test command: the agent's work will be published without being tested. That is")
			ui.Say("  fine for a first try. Later, change verify.command in the file below.")
		}
	}
	ui.Say("")
	change, ciName := "pull request", "GitHub Actions"
	switch {
	case st.UsesGitLab():
		change, ciName = "merge request", "GitLab CI"
	case st.UsesGitea():
		ciName = "Gitea or Forgejo Actions"
	}
	ui.Say("  One more: does %s run automatic checks on %ss (%s or another CI)?", st.Repo, change, ciName)
	ui.Say("  If it does, the service waits until they pass before it hands a %s to you.", change)
	ui.Say("  If it does not, answer n, or the service would wait for checks that never come.")
	ci, err := ui.Confirm("  Does it run checks on " + change + "s?")
	if err != nil {
		return err
	}
	look := w.LookPath
	if look == nil {
		look = exec.LookPath
	}
	engine := Engine(goos, look)
	switch {
	case st.Sandbox == SandboxSystemd:
		engine = SandboxSystemd
	case engine == "":
		engine = "docker"
	}
	text := render(engine, st, owner, verify, ci)
	ui.Say("  This file will be written to %s:", path)
	ui.Say("")
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		ui.Show("    %s", line)
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
// a narrow character set, except the two paths, which are quoted, and the
// verify command, which goes into a YAML block where no character is
// special.
//
// The container sandbox is the one that exists, with docker or podman. Its
// workspaces are Docker volumes, so the directories here only hold the
// service's own files and belong to the user who runs it.
func render(engine string, st *State, owner, verify string, ci bool) string {
	root := strconv.Quote(filepath.Join(st.Dir(), "workspaces"))
	stateDir := strconv.Quote(filepath.Join(st.Dir(), "state"))
	var b strings.Builder
	b.WriteString("---\n")
	if st.UsesLinear() {
		renderLinear(&b, st)
	} else {
		renderGitHub(&b, st, owner)
	}
	b.WriteString("workspace:\n  root: " + root + "\n  repo: " + st.Repo + "\n")
	if st.UsesGitLab() {
		b.WriteString("  host: gitlab\n")
		if st.HostURL != "" && st.HostURL != gitlab.Default {
			b.WriteString("  host_url: " + strconv.Quote(st.HostURL) + "\n")
		}
	}
	if st.UsesGitea() {
		b.WriteString("  host: gitea\n  host_url: " + strconv.Quote(st.HostURL) + "\n")
	}
	b.WriteString("sandbox:\n  backend: " + engine + "\n")
	b.WriteString("verify:\n  command: |\n    " + verify + "\n")
	if !ci {
		b.WriteString("publish:\n  required_checks: none\n")
	}
	b.WriteString("budget:\n  state_dir: " + stateDir + "\n")
	b.WriteString("worker:\n  name: " + st.Worker + "\n")
	b.WriteString("---\n\n")
	b.WriteString("Implement the issue below in this repository.\n\n")
	b.WriteString("Follow the conventions in CLAUDE.md if the repository has one. Add or update tests for every\n")
	b.WriteString("behaviour you change, and run the test suite before you finish.\n")
	return b.String()
}

// renderLinear is the tracker part for a Linear team. The names come from
// the team; they are quoted.
func renderLinear(b *strings.Builder, st *State) {
	quoted := func(list []string) string {
		q := make([]string, len(list))
		for i, s := range list {
			q[i] = strconv.Quote(s)
		}
		return "[" + strings.Join(q, ", ") + "]"
	}
	terminal := st.LinearTerminal
	if len(terminal) == 0 {
		terminal = []string{"Done"}
	}
	b.WriteString("tracker:\n  kind: linear\n  provider:\n")
	b.WriteString("    team_key: " + st.LinearTeam + "\n")
	b.WriteString("  allowed_authors: " + quoted([]string{st.LinearAuthor}) + "\n")
	b.WriteString("  terminal_states: " + quoted(terminal) + "\n")
	b.WriteString("  handoff_state: " + strconv.Quote(st.LinearHandoff) + "\n")
	if st.OffGitHub() {
		// Reviews and comments there are by that host's users.
		b.WriteString("  allowed_commenters: " + quoted([]string{st.Reviewer}) + "\n")
	}
}

func renderGitHub(b *strings.Builder, st *State, owner string) {
	b.WriteString("tracker:\n  kind: github\n  provider:\n")
	b.WriteString("    repo: " + st.Repo + "\n")
	author := owner
	if st.OwnerType == kindOrg {
		// A ticket's state is its column on the board, and only issues
		// written by the operator count (SPEC 12.5).
		author = st.Author
		b.WriteString("    state_source: project\n")
		b.WriteString("    project:\n      number: " + strconv.Itoa(st.Board) + "\n")
	} else {
		b.WriteString("    state_source: labels\n")
	}
	b.WriteString("    bot_login: \"" + st.AppSlug + "[bot]\"\n")
	b.WriteString("  allowed_authors: [" + author + "]\n")
}
