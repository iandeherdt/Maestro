// Command maestro runs the orchestrator described in docs/SPEC.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/dockerbox"
	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/prompt"
	"github.com/iandeherdt/Maestro/internal/proxy"
	"github.com/iandeherdt/Maestro/internal/setup"
	"github.com/iandeherdt/Maestro/internal/workflow"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// version is set at build time for releases.
var version = "dev"

const usage = `usage:
  maestro setup [--dir DIR] [--port N] [--board github|linear]
                                        guided installation, safe to run again
  maestro new                            guided start of a Unity or Godot game for the agents
  maestro check [path-to-WORKFLOW.md]    validate a workflow file
  maestro image                          build the sandbox image (needs Docker)
  maestro login                          sign Claude Code in inside the sandbox
  maestro run                            start the service, stop with Ctrl-C
  maestro status                         what the running service is doing
  maestro pause [reason]                 start no new tickets until resume
  maestro resume                         continue after a pause
  maestro unblock ISSUE                  give a blocked ticket a fresh start
  maestro doctor                         check this machine, the sandbox and GitHub
  maestro service install                run in the background, start at login
  maestro service uninstall              stop running in the background
  maestro conventions                    rule packs per kind of project, enforced by tools
  maestro skills                         give the agents extra skills and agent files
  maestro unity                          the Unity licence the checks of a Unity project use
  maestro linear                         the Linear API key, when the board is Linear
  maestro assistant                      let Claude Code on this machine ask and pause the service
  maestro projects                       the projects on this machine; add and remove them
  maestro version                        print the version

Several projects on one machine: name one with --project NAME on any command.
With several and no name, maestro asks which one.

The usual order the first time: setup, image, login, doctor, run.`

func run(args []string, stdout, stderr io.Writer) int {
	chosen = selection{}
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintln(stdout, "maestro", version)
		return 0
	}
	if len(args) > 0 && args[0] == "proxy" {
		return runProxy(args[1:], stderr)
	}
	// The self-test runs this inside the sandbox (SPEC 11.6).
	if len(args) > 0 && args[0] == "probe" {
		return dockerbox.Probe(args[1:], stdout)
	}
	if len(args) > 0 && (args[0] == "projects" || args[0] == "project") {
		return runProjects(args[1:], os.Stdin, stdout, stderr)
	}
	if len(args) > 0 {
		rest, code, done := selectProject(args[0], args[1:], os.Stdin, stdout, stderr)
		if done {
			return code
		}
		args = append([]string{args[0]}, rest...)
	}
	if len(args) > 0 {
		switch args[0] {
		case "run":
			return runService(args[1:], stdout, stderr)
		case "image":
			return runImage(args[1:], stdout, stderr)
		case "status", "pause", "resume", "unblock":
			return runControl(args[0], args[1:], stdout, stderr)
		case "doctor":
			return runDoctor(args[1:], stdout, stderr)
		case "service":
			return runBackground(args[1:], os.Stdin, stdout, stderr)
		case "login":
			return runLogin(args[1:], os.Stdin, stdout, stderr)
		case "conventions":
			return runConventions(args[1:], stdout, stderr)
		case "skills":
			return runSkills(args[1:], os.Stdin, stdout, stderr)
		case "unity":
			return runUnity(args[1:], os.Stdin, stdout, stderr)
		case "linear":
			return runLinear(args[1:], os.Stdin, stdout, stderr)
		case "mcp":
			return runMCP(args[1:], os.Stdin, stdout, stderr)
		case "assistant":
			return runAssistant(args[1:], stdout, stderr)
		}
	}
	if len(args) > 0 && args[0] == "setup" {
		return runSetup(args[1:], os.Stdin, stdout, stderr)
	}
	if len(args) > 0 && args[0] == "new" {
		return runNew(args[1:], os.Stdin, stdout, stderr)
	}
	if len(args) == 0 || args[0] != "check" || len(args) > 2 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	explicit := ""
	if len(args) == 2 {
		explicit = args[1]
	}
	path := workflowPath(explicit)
	def, err := workflow.Load(path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	home, _ := os.UserHomeDir()
	cfg, err := config.Build(def, config.Options{Env: os.Getenv, HomeDir: home})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	// Parse the prompt template and render it once with sample data, so that
	// an unknown variable, filter or tag is reported here and not at the
	// first dispatch.
	pb, err := prompt.New(def.PromptTemplate)
	if err == nil {
		_, err = pb.First(prompt.Input{
			Issue:      issue.Issue{ID: "sample", Identifier: "SAMPLE-1", Title: "Sample", State: "Todo"},
			WorkerName: cfg.Worker.Name, RepoName: cfg.Workspace.Repo,
			BaseBranch: cfg.Workspace.BaseBranch, Branch: cfg.Publish.BranchPrefix + "SAMPLE-1",
		})
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prompt.RenderError, err)
		return 1
	}
	for _, w := range cfg.Warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	fmt.Fprintf(stdout, "checked %s\n", path)
	fmt.Fprintf(stdout, "workflow ok: tracker=%s repo=%s worker=%s required_labels=%v\n",
		cfg.Tracker.Kind, cfg.Workspace.Repo, cfg.Worker.Name, cfg.Tracker.RequiredLabels)
	return 0
}

// workflowPath applies SPEC 5.1: an explicit path, else WORKFLOW.md in the
// current directory, else the file the guided setup wrote.
func workflowPath(explicit string) string {
	path := workflow.ResolvePath(explicit)
	if explicit != "" {
		return path
	}
	// A project named on the command line or picked from the list uses
	// its own settings, never a WORKFLOW.md that happens to be in the
	// current directory.
	if chosen.named {
		return filepath.Join(chosen.dir, workflow.DefaultFileName)
	}
	if _, err := os.Stat(path); err == nil {
		return path
	}
	if dir, err := configDir(); err == nil {
		fromSetup := filepath.Join(dir, workflow.DefaultFileName)
		if _, err := os.Stat(fromSetup); err == nil || chosen.named {
			return fromSetup
		}
	}
	return path
}

// workflowIn is workflowPath for the project in dir: its own settings file
// when neither the command line nor the current directory names one.
func workflowIn(dir, explicit string) string {
	path := workflow.ResolvePath(explicit)
	if explicit != "" {
		return path
	}
	if _, err := os.Stat(path); err == nil && !chosen.named {
		return path
	}
	return filepath.Join(dir, workflow.DefaultFileName)
}

// maestroHome is where Maestro keeps its files: "maestro" in the user's
// configuration directory. An installation made when the command was called
// symphony keeps its directory, so its App key and settings are found.
//
// It holds the first project's setup itself and the other projects under
// projects/ (see projects.go).
func maestroHome() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "maestro")
	if _, err := os.Stat(dir); err != nil {
		if old := filepath.Join(base, "symphony"); isDir(old) {
			return old, nil
		}
	}
	return dir, nil
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// runProxy is the egress proxy. It is started by the service inside the
// proxy container, not by a person, so it is not in the usage text.
func runProxy(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "", "address to listen on")
	profile := fs.String("profile", "", "network profile, for the log")
	allowFile := fs.String("allow-file", "", "JSON file with the allowed host names")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *listen == "" || *allowFile == "" {
		fmt.Fprintln(stderr, "usage: maestro proxy --listen ADDR --profile NAME --allow-file FILE")
		return 2
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	p := &proxy.Proxy{Profile: *profile, Log: slog.New(slog.NewTextHandler(stderr, nil)),
		Allow: func() []string { return dockerbox.ReadAllow(*allowFile) }}
	if err := p.Serve(ln); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func runSetup(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "where setup keeps its state and the App key (default: the user's configuration directory)")
	port := fs.Int("port", setup.DefaultPort, "loopback port used while creating the GitHub App")
	board := fs.String("board", "", "where the tickets are kept: github or linear (asked when not given)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	if *board != "" && *board != setup.TrackerGitHub && *board != setup.TrackerLinear {
		fmt.Fprintln(stderr, "--board is github or linear")
		return 2
	}
	if *dir == "" {
		found, err := configDir()
		if err != nil {
			fmt.Fprintln(stderr, "no configuration directory was found for this user, pass --dir:", err)
			return 1
		}
		*dir = found
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	st, err := setup.LoadState(abs)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if ps, err := listProjects(); err == nil {
		st.TakenWorkers = workersOf(ps, abs)
	}
	if *board != "" {
		kept, err := setup.SwitchTracker(st, *board)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if kept != "" {
			fmt.Fprintf(stdout, "The settings file for the other board is kept as %s. A new one is written for %s.\n", kept, *board)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ui := setup.Terminal(stdin, stdout)
	title := "Maestro setup"
	if chosen.named {
		title += ": " + chosen.name
	}
	ui.Banner(title, "Every step says what it will do and waits for you.",
		"You can stop with Ctrl-C and run this command again; finished steps are skipped.",
		fmt.Sprintf("State and the App key are kept in %s", abs))
	steps := []setup.Step{
		&setup.Machine{Dir: abs},
		&setup.CreateApp{Port: *port},
		&setup.InstallApp{},
		&setup.Tickets{Linear: setup.LinearBoard{Endpoint: linearEndpoint,
			AskKey: func(ui *setup.UI, prompt string) (string, error) { return askSecret(ui.In, stdin, ui.Out, prompt) }}},
		&setup.WorkflowFile{},
	}
	// Ctrl-C ends whatever the wizard waits for on the network. A question
	// that waits for the keyboard is not ended by that, so after a moment
	// the wizard is stopped from here.
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-finished:
			return
		case <-ctx.Done():
		}
		select {
		case <-finished:
		case <-time.After(300 * time.Millisecond):
			ui.Restore()
			ui.Fail(stderr, setup.ErrAborted, true)
			os.Exit(3)
		}
	}()
	if err := setup.Run(ctx, ui, st, steps); err != nil {
		aborted := errors.Is(err, setup.ErrAborted)
		ui.Fail(stderr, err, aborted)
		if chosen.named {
			fmt.Fprintf(stderr, "To continue this project: maestro setup --project %s\n", chosen.name)
		}
		if aborted {
			return 3
		}
		return 1
	}
	ui.Say("")
	tickets, pickup := "the labels", "issues labelled status:todo and run:"+st.Worker
	switch {
	case st.UsesLinear():
		tickets, pickup = "the Linear team "+st.LinearTeam, "tickets with the label run:"+st.Worker+" that you move to Todo"
	case st.OnBoard():
		tickets, pickup = "the board", "cards in the Todo column whose Worker is "+st.Worker
	}
	ui.Headline(true, fmt.Sprintf("Setup is finished: the GitHub App, its installation on %s, %s and the settings file.", st.Repo, tickets))
	ui.Say("Three commands are left. Run them in this order, with Docker running:")
	ui.Commands([][2]string{
		{mention("maestro image"), "builds the container the agent works in (a few minutes, once)"},
		{mention("maestro login"), "signs Claude Code in inside that container (once)"},
		{mention("maestro run"), "starts the service; it then picks up " + pickup},
	})
	if st.UsesLinear() {
		ui.Say("")
		ui.Say("Working with Linear, once the service runs:")
		ui.Commands([][2]string{
			{"a ticket", "add the label run:" + st.Worker + " and move it to Todo; the pull request opens on GitHub"},
			{"something bigger", "add the label and move it to Idea; answer the questions, then move it to Planned"},
			{"your review", "comment on the pull request; the ticket goes to Rework and the agent continues"},
		})
		ui.Say("Only your own moves count as approval (%s). `%s` checks the team at any time.", st.LinearAuthor, mention("maestro doctor"))
	}
	ui.Say("Your settings: %s", st.WorkflowFile)
	ui.Say("To check them at any time: `%s`", mention("maestro check"))
	return 0
}
