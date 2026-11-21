// Command symphony runs the orchestrator described in docs/SPEC.md.
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

	"github.com/iandeherdt/Symphony-beads/internal/config"
	"github.com/iandeherdt/Symphony-beads/internal/dockerbox"
	"github.com/iandeherdt/Symphony-beads/internal/issue"
	"github.com/iandeherdt/Symphony-beads/internal/prompt"
	"github.com/iandeherdt/Symphony-beads/internal/proxy"
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
  symphony image                          build the sandbox image (needs Docker)
  symphony login                          sign Claude Code in inside the sandbox
  symphony run                            start the service, stop with Ctrl-C
  symphony status                         what the running service is doing
  symphony pause [reason]                 start no new tickets until resume
  symphony resume                         continue after a pause
  symphony service install                run in the background, start at login
  symphony service uninstall              stop running in the background
  symphony version                        print the version

The usual order the first time: setup, image, login, run.`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintln(stdout, "symphony", version)
		return 0
	}
	if len(args) > 0 && args[0] == "proxy" {
		return runProxy(args[1:], stderr)
	}
	if len(args) > 0 {
		switch args[0] {
		case "run":
			return runService(args[1:], stdout, stderr)
		case "image":
			return runImage(args[1:], stdout, stderr)
		case "status", "pause", "resume":
			return runControl(args[0], args[1:], stdout, stderr)
		case "service":
			return runBackground(args[1:], os.Stdin, stdout, stderr)
		case "login":
			return runLogin(args[1:], os.Stdin, stdout, stderr)
		}
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

// runProxy is the egress proxy. It is started by the service inside the
// proxy container, not by a person, so it is not in the usage text.
func runProxy(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "", "address to listen on")
	profile := fs.String("profile", "", "network profile, for the log")
	allowFile := fs.String("allow-file", "", "JSON file with the allowed host names")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *listen == "" || *allowFile == "" {
		fmt.Fprintln(stderr, "usage: symphony proxy --listen ADDR --profile NAME --allow-file FILE")
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
		&setup.Tickets{},
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
	tickets, pickup := "the labels", "issues labelled status:todo and run:"+st.Worker
	if st.OnBoard() {
		tickets, pickup = "the board", "cards in the Todo column whose Worker is "+st.Worker
	}
	ui.Say("Setup is finished: the GitHub App, its installation on %s, %s and the settings file.", st.Repo, tickets)
	ui.Say("Three commands are left. Run them in this order, with Docker running:")
	ui.Say("  symphony image    builds the container the agent works in (a few minutes, once)")
	ui.Say("  symphony login    signs Claude Code in inside that container (once)")
	ui.Say("  symphony run      starts the service; it then picks up %s", pickup)
	ui.Say("Your settings: %s", st.WorkflowFile)
	ui.Say("To check them at any time: symphony check")
	return 0
}
