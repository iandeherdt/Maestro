package main

import (
	"fmt"
	"io"
	"strings"
)

// helpPage is what `maestro help COMMAND` shows for one command.
type helpPage struct {
	name  string
	group string
	// usage is the command line, one per line when there are several.
	usage string
	// summary is the line in the overview.
	summary string
	// body says what the command does, when to use it, and its flags.
	body string
	// see names related commands.
	see []string
}

// The groups, in the order the overview shows them.
var helpGroups = []string{
	"Getting started",
	"Running the service",
	"Watching and steering",
	"Projects, skills and rule packs",
	"Keeping it up to date",
}

// Flags most commands take.
const commonFlags = `Flags most commands take:
  --project NAME        the project to work on, when this machine has several
  --dir DIR             where setup keeps its state (default: the user's
                        configuration directory)
  --file WORKFLOW.md    the settings file (default: the one setup wrote)`

var helpPages = []helpPage{
	{
		name: "setup", group: "Getting started",
		usage:   "maestro setup [--board github|linear] [--sandbox container|systemd] [--dir DIR] [--port N]",
		summary: "guided installation, safe to run again",
		body: `Walks through everything the service needs, one step at a time. Every step
first checks whether it is already done, says what it will do, and waits for
you. Running it again skips what is in place and repairs what is not, so it is
also the way to change a choice made earlier.

The steps:
  1. Check this machine: git, and Docker or Podman (or, with --sandbox
     systemd, systemd 239 or later, cgroup v2, sudo, Node 18 and npm).
  2. Set up where tickets are approved: a GitHub board or Linear.
  3. Create the service's GitHub App, on github.com, with one button.
  4. Give the App access to the repository.
  5. Create the labels, and the board or the Linear team.
  6. Write the workflow file, the settings the service runs with.
  7. With --sandbox systemd: install the Linux sandbox. The root script is
     shown before it runs, and runs once with sudo.
  8. For a Unity project: set up Unity's checks.

Flags:
  --board github|linear
        where the tickets are kept (asked when not given)
  --sandbox container|systemd
        where the agent's steps run: in a container with Docker or Podman
        (the default), or, on Linux, as systemd units without containers
  --dir DIR
        where setup keeps its state and the App key
  --port N
        loopback port used while the GitHub App is created (default 8976)

Examples:
  maestro setup
  maestro setup --board linear
  maestro setup --sandbox systemd`,
		see: []string{"image", "login", "doctor", "run"},
	},
	{
		name: "new", group: "Getting started",
		usage:   "maestro new [--another] [--dir DIR]",
		summary: "guided start of a Unity or Godot game for the agents",
		body: `A guided start of a game project that the agents work on. It finds or makes
the project, adds the rule packs, puts it on GitHub, points Maestro at it,
imports the Unity licence and builds the sandbox image. Every step says what it
will do and waits for you. Run it again to continue where you stopped.

Flags:
  --another   start another game instead of continuing the last one
  --dir DIR   where setup keeps its state`,
		see: []string{"setup", "conventions", "unity"},
	},
	{
		name: "image", group: "Getting started",
		usage:   "maestro image",
		summary: "build the sandbox image (container sandbox only)",
		body: `Builds the container image the agent's steps run in, with Docker or Podman.
The image holds this version of maestro and the pinned version of Claude Code,
and the packs' tools when rule packs are on. Run it after setup and after an
update; ` + "`maestro run`" + ` refuses an image that is out of date and says so.

With the systemd sandbox there is no image: setup installs Claude Code under
/opt/maestro instead.`,
		see: []string{"setup", "login", "doctor"},
	},
	{
		name: "login", group: "Getting started",
		usage:   "maestro login",
		summary: "sign Claude Code in inside the sandbox",
		body: `Runs Claude Code's sign-in inside the sandbox, on this terminal. The login is
kept in the sandbox's own folder and is used by every run after it. It is not
your own Claude Code login on this machine, and the agent cannot read the
service's other credentials.

Run it once after setup, and again when ` + "`maestro doctor`" + ` says the login is
missing or expired. It works while the service is running.`,
		see: []string{"doctor", "run"},
	},
	{
		name: "doctor", group: "Getting started",
		usage:   "maestro doctor",
		summary: "check this machine, the sandbox, GitHub and the board",
		body: `Checks everything the service depends on and says, for each problem, what to
do about it. It changes nothing. It looks at:
  - this machine: git, Docker or Podman and its network backend, cgroups,
    or for the systemd sandbox the launcher, the sudo rule and the folders;
  - the sandbox: the image, the Claude Code login, and a self-test that tries
    to get out of the sandbox for each network profile;
  - GitHub: the App, its rights on the repository and the base branch's
    protection;
  - the board, the workflow file, and the background service.

It exits with status 1 when a problem stops the service, and 0 otherwise,
also when there are only warnings. Run it after setup, after an update, and
whenever something looks wrong.`,
		see: []string{"setup", "login", "status"},
	},
	{
		name: "check", group: "Getting started",
		usage:   "maestro check [path-to-WORKFLOW.md]",
		summary: "validate a workflow file",
		body: `Reads a workflow file, checks every setting and renders the prompt once with
a sample ticket, without starting anything. Use it after editing the file by
hand. Without a path it checks WORKFLOW.md in the current folder, or else the
file setup wrote.

The running service also reloads the file when it changes, and keeps the old
settings when the new ones do not pass this check.`,
		see: []string{"doctor"},
	},
	{
		name: "run", group: "Running the service",
		usage:   "maestro run",
		summary: "start the service in this terminal, stop with Ctrl-C",
		body: `Starts the service in the foreground. It watches the board, runs the agent on
approved tickets in the sandbox, checks and publishes the work as pull
requests, and follows them until they are merged. Ctrl-C stops it; tickets
that were running are picked up again at the next start.

To keep it running after you log out, use ` + "`maestro service install`" + ` instead.`,
		see: []string{"service", "status", "dashboard"},
	},
	{
		name: "service", group: "Running the service",
		usage:   "maestro service install\nmaestro service uninstall",
		summary: "run in the background and start at login, or stop that",
		body: `install runs the service in the background, as you and not as root: a systemd
user unit on Linux, a launchd agent on a Mac, and on WSL also a Windows task
that starts it when you log in. It starts again after a restart. Run install again after changing how
maestro is installed; it replaces the old service.

uninstall stops the background service and removes it. Your settings, the
workspaces and the Claude login are untouched, and ` + "`maestro run`" + ` still works.`,
		see: []string{"run", "status", "update"},
	},
	{
		name: "status", group: "Watching and steering",
		usage:   "maestro status",
		summary: "what the running service is doing",
		body: `Asks the running service what it is doing: the tickets it works on and in
which step, the pull requests waiting for their checks, the tickets that will
be tried again and why, today's agent runs and tokens, and whether it is
paused or waiting for the Claude usage limit. With several projects and no
--project, it shows them all.`,
		see: []string{"dashboard", "pause", "unblock"},
	},
	{
		name: "dashboard", group: "Watching and steering",
		usage:   "maestro dashboard",
		summary: "open the live status page in the browser",
		body: `Opens the running service's status page in the browser. It shows the same as
` + "`maestro status`" + ` and updates itself. The page is served on this machine's
loopback address only.`,
		see: []string{"status"},
	},
	{
		name: "pause", group: "Watching and steering",
		usage:   "maestro pause [reason]",
		summary: "start no new tickets until resume",
		body: `Tells the running service to start no new tickets. Tickets that are running
finish. The pause stays, also after a restart, until ` + "`maestro resume`" + `. The
reason is shown by status and the dashboard.

Example:
  maestro pause updating the server tonight`,
		see: []string{"resume", "status"},
	},
	{
		name: "resume", group: "Watching and steering",
		usage:   "maestro resume",
		summary: "continue after a pause",
		body:    `Ends a pause: the service looks for tickets again.`,
		see:     []string{"pause", "status"},
	},
	{
		name: "unblock", group: "Watching and steering",
		usage:   "maestro unblock ISSUE",
		summary: "give a blocked ticket a fresh start",
		body: `Clears a blocked ticket's failure count and moves it back to the state it can
be picked up in, so the next run starts afresh. Fix the reason it was blocked
first; ` + "`maestro status`" + ` shows it. ISSUE is the ticket's identifier, such as
GH-12 or ENG-7; for GitHub the number alone is enough.

Example:
  maestro unblock 12`,
		see: []string{"status"},
	},
	{
		name: "assistant", group: "Watching and steering",
		usage:   "maestro assistant\nmaestro assistant remove",
		summary: "let Claude Code on this machine ask about and pause the service",
		body: `Gives Claude Code on this machine a skill that explains Maestro and how to
write a ticket for it, and prints the one command that adds Maestro's tools
(status, pause, resume, check). It also prints the server settings for another
assistant that speaks MCP. The tools cannot approve a ticket, merge or change
settings: approving work stays with you.

remove deletes the skill again and says how to remove the tools.`,
		see: []string{"status", "pause"},
	},
	{
		name: "projects", group: "Projects, skills and rule packs",
		usage:   "maestro projects\nmaestro projects add [NAME]\nmaestro projects remove NAME",
		summary: "the projects on this machine; add and remove them",
		body:    strings.TrimPrefix(projectsUsage, "usage:\n"),
		see:     []string{"setup", "status"},
	},
	{
		name: "skills", group: "Projects, skills and rule packs",
		usage:   "maestro skills\nmaestro skills add SOURCE [--ref REF] [--path DIR] [--yes]\nmaestro skills remove NAME",
		summary: "give the agents extra skills and agent files",
		body:    strings.TrimPrefix(skillsUsage, "usage:\n"),
		see:     []string{"conventions"},
	},
	{
		name: "conventions", group: "Projects, skills and rule packs",
		usage:   "maestro conventions [show|apply|enable|disable|check] [PACK...]",
		summary: "rule packs per kind of project, enforced by tools",
		body: `A rule pack is a set of rules for one kind of project, such as a Godot game,
each enforced by a tool in the service's verify step and in CI.

` + strings.TrimPrefix(conventionsUsage, "usage:\n"),
		see: []string{"skills", "new"},
	},
	{
		name: "unity", group: "Projects, skills and rule packs",
		usage:   "maestro unity\nmaestro unity activate [--password [--serial] | --file FILE]\nmaestro unity remove",
		summary: "the Unity licence the checks of a Unity project use",
		body:    strings.TrimPrefix(unityUsage, "usage:\n"),
		see:     []string{"conventions", "new"},
	},
	{
		name: "linear", group: "Projects, skills and rule packs",
		usage:   "maestro linear\nmaestro linear key\nmaestro linear remove",
		summary: "the Linear API key, when the board is Linear",
		body:    strings.TrimPrefix(linearUsage, "usage:\n"),
		see:     []string{"setup"},
	},
	{
		name: "update", group: "Keeping it up to date",
		usage:   "maestro update [--version vX.Y.Z] [--no-restart]",
		summary: "install the latest release and restart the service",
		body: `Downloads a release from GitHub, checks it against the release's SHA256SUMS,
replaces this maestro (and on Linux maestro-launch next to it) and restarts
the background service. After an update, rebuild the image with
` + "`maestro image`" + ` when you use the container sandbox, and run ` + "`maestro doctor`" + `.
With the systemd sandbox, run ` + "`maestro setup`" + ` again so the root copy is
updated too.

Flags:
  --version vX.Y.Z   install this release instead of the latest
  --no-restart       replace the binary, but leave the background service
                     running the old one`,
		see: []string{"image", "doctor", "version"},
	},
	{
		name: "version", group: "Keeping it up to date",
		usage:   "maestro version",
		summary: "print the version",
		body:    `Prints the version of this maestro.`,
		see:     []string{"update"},
	},
	{
		name: "help", group: "Keeping it up to date",
		usage:   "maestro help [COMMAND]",
		summary: "this overview, or all about one command",
		body: `Without a command, lists every command. With one, says what it does, when to
use it, its flags and examples. ` + "`maestro COMMAND --help`" + ` shows the same.`,
	},
}

func findHelp(name string) *helpPage {
	for i := range helpPages {
		if helpPages[i].name == name {
			return &helpPages[i]
		}
	}
	return nil
}

// writeOverview prints every command, by group.
func writeOverview(w io.Writer) {
	fmt.Fprintln(w, "Maestro runs Claude Code on the tickets you approve, in a sandbox, and opens")
	fmt.Fprintln(w, "a pull request for each one. Nothing is merged without you.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "usage: maestro COMMAND [flags]")
	for _, g := range helpGroups {
		fmt.Fprintf(w, "\n%s:\n", g)
		for _, p := range helpPages {
			if p.group == g {
				fmt.Fprintf(w, "  %-13s %s\n", p.name, p.summary)
			}
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "The usual order the first time: setup, image, login, doctor, run, then")
	fmt.Fprintln(w, "service install to keep it running.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Several projects on one machine: name one with --project NAME on any command.")
	fmt.Fprintln(w, "With several and no name, maestro asks which one.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "More about one command: maestro help COMMAND")
}

// writePage prints one command's page.
func writePage(w io.Writer, p *helpPage) {
	fmt.Fprintf(w, "maestro %s: %s\n\n", p.name, p.summary)
	fmt.Fprintln(w, "usage:")
	for _, line := range strings.Split(p.usage, "\n") {
		fmt.Fprintf(w, "  %s\n", line)
	}
	fmt.Fprintf(w, "\n%s\n", strings.TrimRight(p.body, "\n"))
	if p.name != "help" && p.name != "version" && p.name != "check" {
		fmt.Fprintf(w, "\n%s\n", commonFlags)
	}
	if len(p.see) > 0 {
		fmt.Fprintf(w, "\nSee also: maestro help %s\n", strings.Join(p.see, ", maestro help "))
	}
}

func isHelpFlag(s string) bool { return s == "-h" || s == "--help" || s == "-help" }

// runHelp answers `maestro help`, `maestro help COMMAND`, `maestro -h` and
// `maestro COMMAND --help`. It reports whether it answered.
func runHelp(args []string, stdout, stderr io.Writer) (code int, done bool) {
	switch {
	case len(args) == 0:
		return 0, false
	case args[0] == "help" || isHelpFlag(args[0]):
		if len(args) == 1 {
			writeOverview(stdout)
			return 0, true
		}
		if p := findHelp(args[1]); p != nil {
			writePage(stdout, p)
			return 0, true
		}
		fmt.Fprintf(stderr, "maestro has no command %q.\n\n", args[1])
		writeOverview(stderr)
		return 2, true
	}
	p := findHelp(args[0])
	if p == nil {
		return 0, false
	}
	for _, a := range args[1:] {
		if a == "--" {
			break
		}
		if isHelpFlag(a) {
			writePage(stdout, p)
			return 0, true
		}
	}
	return 0, false
}
