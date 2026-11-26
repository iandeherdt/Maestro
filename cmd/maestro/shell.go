package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/iandeherdt/Maestro/internal/claude"
	"github.com/iandeherdt/Maestro/internal/dockerbox"
	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
	"github.com/iandeherdt/Maestro/internal/sandbox"
)

// sessionFlags are the flags of `maestro shell` and `maestro talk`.
type sessionFlags struct {
	dir, file string
	agent     bool
	new       bool
	ticket    string
}

func parseSession(name string, args []string, stderr io.Writer) (sessionFlags, bool) {
	var f sessionFlags
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.dir, "dir", "", "where setup keeps its state (default: the user's configuration directory)")
	fs.StringVar(&f.file, "file", "", "the settings file (default: the one setup wrote)")
	if name == "shell" {
		fs.BoolVar(&f.agent, "agent", false, "the agent's view: its Claude Code login and network, no Unity licence")
	} else {
		fs.BoolVar(&f.new, "new", false, "start a new conversation instead of continuing the agent's")
	}
	// The ticket may come before or after the flags.
	var rest []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return f, false
		}
		if fs.NArg() == 0 {
			break
		}
		rest = append(rest, fs.Arg(0))
		args = fs.Args()[1:]
	}
	usage := "usage: maestro shell [TICKET] [--agent]"
	if name == "talk" {
		usage = "usage: maestro talk TICKET [--new]"
	}
	switch {
	case len(rest) > 1, name == "talk" && len(rest) == 0:
		fmt.Fprintln(stderr, usage)
		return f, false
	case len(rest) == 1:
		f.ticket = rest[0]
	}
	return f, true
}

// findWorkspace is the workspace of a ticket as the owner typed it: GH-12,
// 12 or ENG-7.
func findWorkspace(keys []string, typed string) (string, error) {
	var found []string
	for _, k := range keys {
		if orchestrator.SameIdentifier(typed, k) {
			found = append(found, k)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", explain("%s has no workspace on this machine. A ticket has one from its first run until it is done.\n"+
			"Workspaces here: %s", typed, orNone(keys))
	}
	return "", explain("%s could be %s. Type the whole identifier.", typed, strings.Join(found, " or "))
}

func orNone(keys []string) string {
	if len(keys) == 0 {
		return "none"
	}
	return strings.Join(keys, ", ")
}

// sessionBox opens the installation and checks that it can run an owner's
// session. stop takes down the proxies again when they were started here.
func sessionBox(ctx context.Context, f sessionFlags, command string) (in *install, stop func(), err error) {
	stop = func() {}
	if in, err = open(f.dir, f.file, nil); err != nil {
		return nil, stop, err
	}
	if in.dbox == nil {
		return nil, stop, explain("`maestro %s` works with the container sandbox only. With the systemd sandbox, the agent's steps\n"+
			"run as systemd units in %s; look there with sudo.", command, in.cfg.Workspace.Root)
	}
	if err = in.docker(ctx); err == nil {
		err = in.image(ctx)
	}
	if err != nil {
		return nil, stop, err
	}
	// A running service already has the proxies up. Starting them again
	// would stop its work, so they are only started when they are missing.
	if !in.box.ProxyRunning(ctx, sandbox.NetAgent) || !in.box.ProxyRunning(ctx, sandbox.NetHooks) {
		if err := in.up(ctx); err != nil {
			return nil, stop, err
		}
		stop = func() { in.box.Down(context.Background()) }
	}
	return in, stop, nil
}

// runShell is `maestro shell`.
func runShell(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	f, ok := parseSession("shell", args, stderr)
	if !ok {
		return 2
	}
	ctx, cancel := interruptible()
	defer cancel()
	in, stop, err := sessionBox(ctx, f, "shell")
	if err != nil {
		return report(stderr, err)
	}
	defer stop()
	view := dockerbox.ViewChecks
	if f.agent {
		view = dockerbox.ViewAgent
	}
	sess := dockerbox.Session{View: view, Env: in.cfg.Sandbox.Env}
	if f.ticket == "" {
		fmt.Fprintln(stdout, "A shell in a new, empty sandbox from the image "+in.cfg.Sandbox.Image+", with "+view.String()+".")
		fmt.Fprintln(stdout, "Everything in it is removed when you leave it (exit or Ctrl-D).")
		fmt.Fprintln(stdout)
		code, err := in.dbox.Interactive(ctx, sess, stdin, stdout, stderr)
		if err != nil {
			return report(stderr, err)
		}
		return code
	}
	keys, err := in.dbox.Workspaces(ctx)
	if err != nil {
		return report(stderr, err)
	}
	key, err := findWorkspace(keys, f.ticket)
	if err != nil {
		return report(stderr, err)
	}
	running, err := in.dbox.Running(ctx, key)
	if err != nil {
		return report(stderr, err)
	}
	if len(running) > 0 && !f.agent {
		c := running[0]
		fmt.Fprintf(stdout, "The %s step of %s runs now. This shell joins its container, with what that step has.\n", c.Step, key)
		fmt.Fprintln(stdout, "You see its processes and files as they change. Leaving (exit or Ctrl-D) does not stop the step;")
		fmt.Fprintln(stdout, "what you change in the workspace is part of the agent's work.")
		fmt.Fprintln(stdout)
		if err := in.dbox.Join(ctx, c.Name, stdin, stdout, stderr); err != nil {
			var ee interface{ ExitCode() int }
			if errors.As(err, &ee) {
				return ee.ExitCode()
			}
			return report(stderr, err)
		}
		return 0
	}
	sess.Key = key
	fmt.Fprintf(stdout, "A shell in the workspace of %s, in a new sandbox with %s.\n", key, view.String())
	if len(running) > 0 {
		fmt.Fprintf(stdout, "The %s step of %s runs now, next to this shell, in the same workspace.\n", running[0].Step, key)
	}
	fmt.Fprintln(stdout, "The repository is in /workspace/repo. What you change there is part of the ticket's work, and goes")
	fmt.Fprintln(stdout, "through the same checks before it reaches a pull request. Leave with exit or Ctrl-D.")
	fmt.Fprintln(stdout)
	code, err := in.dbox.Interactive(ctx, sess, stdin, stdout, stderr)
	if err != nil {
		return report(stderr, err)
	}
	return code
}

// busy says whether the service works on a ticket now. Between two turns
// no container runs, so the service is asked too.
func busy(ctx context.Context, in *install, key string) (bool, error) {
	running, err := in.dbox.Running(ctx, key)
	if err != nil {
		return false, err
	}
	if len(running) > 0 {
		return true, nil
	}
	var s statusJSON
	if reach(ctx, in.cfg.Budget.StateDir, http.MethodGet, "/api/v1/state", nil, &s) != nil {
		return false, nil
	}
	for _, r := range s.Running {
		if k, err := issue.WorkspaceKey(r.Identifier); err == nil && k == key {
			return true, nil
		}
	}
	return false, nil
}

// runTalk is `maestro talk`.
func runTalk(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	f, ok := parseSession("talk", args, stderr)
	if !ok {
		return 2
	}
	ctx, cancel := interruptible()
	defer cancel()
	in, stop, err := sessionBox(ctx, f, "talk")
	if err != nil {
		return report(stderr, err)
	}
	defer stop()
	yes, err := in.box.LoggedIn(ctx, in.cfg.Claude.Command)
	if err != nil {
		return report(stderr, err)
	}
	if !yes {
		return report(stderr, explain("Claude Code is not signed in inside the sandbox. Run %s first.", mention("maestro login")))
	}
	keys, err := in.dbox.Workspaces(ctx)
	if err != nil {
		return report(stderr, err)
	}
	key, err := findWorkspace(keys, f.ticket)
	if err != nil {
		return report(stderr, err)
	}
	if b, err := busy(ctx, in, key); err != nil {
		return report(stderr, err)
	} else if b {
		return report(stderr, explain("an agent works on %s now. Talk to it when its run has ended, so that the two of you do not\n"+
			"change the workspace at the same time. %s shows when it is done; %s lets you watch it meanwhile.",
			key, mention("maestro status"), mention("maestro shell "+key)))
	}
	talks := in.conversations()
	id := ""
	if !f.new {
		id = talks.Last(key, filepath.Join(in.auditDir, key+".jsonl"))
	}
	resume := id != ""
	if !resume {
		if id, err = claude.NewSessionID(); err != nil {
			return report(stderr, err)
		}
	}
	argv, err := claude.InteractiveArgs(in.cfg.Claude, id, resume)
	if err != nil {
		return report(stderr, err)
	}
	if resume {
		fmt.Fprintf(stdout, "Claude Code continues the agent's last conversation on %s, in its workspace and sandbox.\n", key)
	} else {
		fmt.Fprintf(stdout, "Claude Code starts a new conversation in the workspace of %s, in the sandbox. It sees the\n", key)
		fmt.Fprintln(stdout, "repository but not the ticket, so tell it what the ticket is about.")
	}
	fmt.Fprintln(stdout, "It has the agent's login, network and restrictions. When you leave (/exit or Ctrl-D twice), the")
	fmt.Fprintln(stdout, "next run on this ticket continues this conversation: for an idea, the plan it posts takes what you")
	fmt.Fprintln(stdout, "agreed into account. Move the card as usual to start that run.")
	fmt.Fprintln(stdout)
	code, err := in.dbox.Interactive(ctx, dockerbox.Session{Key: key, View: dockerbox.ViewAgent, Argv: argv, Env: in.cfg.Sandbox.Env},
		stdin, stdout, stderr)
	if err != nil {
		return report(stderr, err)
	}
	if err := talks.Save(key, id); err != nil {
		return report(stderr, err)
	}
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "The conversation is kept. The next run on %s continues it.\n", key)
	return code
}
