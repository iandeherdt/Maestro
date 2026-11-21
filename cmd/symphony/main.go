// Command symphony runs the orchestrator described in docs/SPEC.md.
//
// The guided setup and configuration checking are implemented so far.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/iandeherdt/Symphony-beads/internal/config"
	"github.com/iandeherdt/Symphony-beads/internal/issue"
	"github.com/iandeherdt/Symphony-beads/internal/prompt"
	"github.com/iandeherdt/Symphony-beads/internal/setup"
	"github.com/iandeherdt/Symphony-beads/internal/workflow"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// version is set at build time for releases.
var version = "dev"

const usage = `usage:
  symphony setup [--dir DIR] [--port N]   guided installation, safe to run again
  symphony check [path-to-WORKFLOW.md]    validate a workflow file
  symphony version                        print the version

The service itself is not implemented yet.`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintln(stdout, "symphony", version)
		return 0
	}
	if len(args) > 0 && args[0] == "setup" {
		return runSetup(args[1:], os.Stdin, stdout, stderr)
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
	if _, err := os.Stat(path); err == nil {
		return path
	}
	if base, err := os.UserConfigDir(); err == nil {
		fromSetup := filepath.Join(base, "symphony", workflow.DefaultFileName)
		if _, err := os.Stat(fromSetup); err == nil {
			return fromSetup
		}
	}
	return path
}

func runSetup(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "where setup keeps its state and the App key (default: the user's configuration directory)")
	port := fs.Int("port", setup.DefaultPort, "loopback port used while creating the GitHub App")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	if *dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			fmt.Fprintln(stderr, "no configuration directory was found for this user, pass --dir:", err)
			return 1
		}
		*dir = filepath.Join(base, "symphony")
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ui := setup.NewUI(stdin, stdout)
	ui.Say("Symphony setup. Every step says what it will do and waits for you.")
	ui.Say("You can stop with Ctrl-C and run this command again; finished steps are skipped.")
	ui.Say("State and the App key are kept in %s", abs)
	steps := []setup.Step{
		&setup.Machine{},
		&setup.CreateApp{Port: *port},
		&setup.InstallApp{},
		&setup.Labels{},
		&setup.WorkflowFile{},
	}
	if err := setup.Run(ctx, ui, st, steps); err != nil {
		if errors.Is(err, setup.ErrAborted) {
			fmt.Fprintln(stderr, err)
			return 3
		}
		fmt.Fprintln(stderr, "setup failed:", err)
		return 1
	}
	ui.Say("")
	ui.Say("Setup is finished as far as it goes today.")
	ui.Say("  Done: the GitHub App, its installation on %s, the labels and the settings file.", st.Repo)
	ui.Say("  Not built yet: the service that picks up tickets. Nothing is running, and labelling an")
	ui.Say("  issue does nothing until it is. You do not need to do anything else for now.")
	ui.Say("  Your settings: %s", st.WorkflowFile)
	ui.Say("  To check them at any time: symphony check")
	return 0
}
