package main

// Several projects on one machine.
//
// A project is one setup directory: its GitHub App installation, its board
// or labels, its WORKFLOW.md, its rule packs, its skills, its Unity
// licence, its workspaces and its state. Every project has its own worker
// name, so its containers, volumes, networks, Claude login and background
// service are its own as well, and its own sandbox image. Nothing that one
// project holds is given to another project's agents or checks.
//
// The first project lives in Maestro's directory itself, where a single
// installation always lived, so an installation from before projects keeps
// working unchanged. Every further project is a directory under projects/.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/setup"
	"github.com/iandeherdt/Maestro/internal/workflow"
	"golang.org/x/term"
)

// projectsDir is the directory under Maestro's own that holds the projects
// after the first.
const projectsDir = "projects"

// projectNameFile names the first project. Without it, the first project is
// called after its worker.
const projectNameFile = "project-name"

var projectNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)

func validProjectName(s string) error {
	if !projectNameRE.MatchString(s) {
		return errors.New("use a short lowercase name such as web or game (letters, digits and -)")
	}
	return nil
}

type project struct {
	Name string
	Dir  string
	// First is the project in Maestro's directory itself.
	First bool
	State *setup.State
}

// label is how a project is shown in a list.
func (p project) label() string {
	if d := p.details(); d != "" {
		return p.Name + "  (" + d + ")"
	}
	return p.Name
}

// details are the repository, the worker and whether setup is finished.
func (p project) details() string {
	var parts []string
	if p.State.Repo != "" {
		parts = append(parts, p.State.Repo)
	}
	if p.State.Worker != "" {
		parts = append(parts, "worker "+p.State.Worker)
	}
	if p.State.WorkflowFile == "" {
		parts = append(parts, "setup not finished")
	}
	return strings.Join(parts, ", ")
}

// ready reports whether setup is finished for the project.
func (p project) ready() bool { return p.State.WorkflowFile != "" }

// chosen is the project this command works on, once it is known.
var chosen selection

type selection struct {
	dir, name string
	// named is set when the person chose it: with --project, with
	// MAESTRO_PROJECT or in a list.
	named bool
}

func choose(p project, named bool) {
	chosen.dir, chosen.name, chosen.named = p.Dir, p.Name, named
}

// readState reads a setup directory's state without creating anything.
func readState(dir string) (*setup.State, bool) {
	if _, err := os.Stat(filepath.Join(dir, "setup.json")); err != nil {
		return nil, false
	}
	st, err := setup.LoadState(dir)
	if err != nil {
		return nil, false
	}
	return st, true
}

// listProjects finds the projects on this machine, the first one first and
// the others by name.
func listProjects() ([]project, error) {
	home, err := maestroHome()
	if err != nil {
		return nil, err
	}
	var out []project
	if st, ok := readState(home); ok {
		out = append(out, project{Name: firstName(home, st), Dir: home, First: true, State: st})
	}
	entries, err := os.ReadDir(filepath.Join(home, projectsDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var rest []project
	for _, e := range entries {
		if !e.IsDir() || validProjectName(e.Name()) != nil {
			continue
		}
		dir := filepath.Join(home, projectsDir, e.Name())
		st, ok := readState(dir)
		if !ok {
			// Made but not yet set up: still a project, so setup can
			// continue it.
			st = &setup.State{}
		}
		rest = append(rest, project{Name: e.Name(), Dir: dir, State: st})
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].Name < rest[j].Name })
	// A later project cannot take the first one's name.
	for _, p := range rest {
		if len(out) > 0 && out[0].Name == p.Name {
			out[0].Name = "first"
		}
	}
	return append(out, rest...), nil
}

// firstName is the name of the project in Maestro's directory itself.
func firstName(home string, st *setup.State) string {
	if raw, err := os.ReadFile(filepath.Join(home, projectNameFile)); err == nil {
		if n := strings.TrimSpace(string(raw)); validProjectName(n) == nil {
			return n
		}
	}
	if validProjectName(st.Worker) == nil {
		return st.Worker
	}
	return "first"
}

func findProject(ps []project, name string) (project, bool) {
	for _, p := range ps {
		if p.Name == name {
			return p, true
		}
	}
	return project{}, false
}

func samePath(a, b string) bool {
	a, _ = filepath.Abs(a)
	b, _ = filepath.Abs(b)
	return filepath.Clean(a) == filepath.Clean(b)
}

// configDir is the setup directory of the project this command works on.
// With several projects and none chosen yet, the person is asked which one
// when they are at a terminal.
func configDir() (string, error) {
	if chosen.dir != "" {
		return chosen.dir, nil
	}
	ps, err := listProjects()
	if err != nil {
		return "", err
	}
	switch len(ps) {
	case 0:
		return maestroHome()
	case 1:
		choose(ps[0], false)
		return chosen.dir, nil
	}
	if !atTerminal() {
		return "", explain("%s", severalProjects(ps))
	}
	ui := setup.Terminal(os.Stdin, os.Stdout)
	options := make([]string, len(ps))
	for i, p := range ps {
		options[i] = p.label()
	}
	i, err := ui.Choose("Which project?", options)
	if err != nil {
		return "", err
	}
	choose(ps[i], true)
	return chosen.dir, nil
}

func severalProjects(ps []project) string {
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = p.Name
	}
	return fmt.Sprintf("There are %d projects on this machine (%s). Say which one with --project NAME,\n"+
		"for example: maestro status --project %s\nOr set MAESTRO_PROJECT. `maestro projects` lists them.",
		len(ps), strings.Join(names, ", "), ps[len(ps)-1].Name)
}

// atTerminal reports whether a person is at the keyboard.
var atTerminal = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// takeProject removes --project NAME (or --project=NAME) from the words of
// a command. The flag can stand anywhere, so it works the same for every
// command.
func takeProject(args []string) (rest []string, name string, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		switch {
		case a == "--project" || a == "-project":
			if i+1 >= len(args) {
				return nil, "", errors.New("--project needs the name of a project")
			}
			name = args[i+1]
			i++
		case strings.HasPrefix(a, "--project=") || strings.HasPrefix(a, "-project="):
			_, name, _ = strings.Cut(a, "=")
		default:
			rest = append(rest, a)
		}
	}
	return rest, name, nil
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "-"+flag || a == "--"+flag || strings.HasPrefix(a, "-"+flag+"=") || strings.HasPrefix(a, "--"+flag+"=") {
			return true
		}
	}
	return false
}

// selectProject reads --project and MAESTRO_PROJECT and settles the project
// where that has to happen before the command starts. It returns the words
// left for the command; done means the command is finished with code.
func selectProject(cmd string, args []string, stdin io.Reader, stdout, stderr io.Writer) (rest []string, code int, done bool) {
	rest, name, err := takeProject(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return nil, 2, true
	}
	fromFlag := name != ""
	if name == "" {
		name = strings.TrimSpace(os.Getenv("MAESTRO_PROJECT"))
	}
	if hasFlag(rest, "dir") {
		if fromFlag {
			fmt.Fprintln(stderr, "--dir and --project both say where the project is; give one of them")
			return nil, 2, true
		}
		return rest, 0, false
	}
	ps, err := listProjects()
	if err != nil {
		return nil, report(stderr, err), true
	}
	creates := cmd == "setup" || cmd == "new"
	if name != "" {
		if p, ok := findProject(ps, name); ok {
			choose(p, true)
			return rest, 0, false
		}
		if !creates {
			msg := fmt.Sprintf("There is no project called %s on this machine.", name)
			if len(ps) > 0 {
				msg += "\n`maestro projects` lists them; `maestro projects add` makes a new one."
			}
			return nil, report(stderr, explain("%s", msg)), true
		}
		if err := validProjectName(name); err != nil {
			return nil, report(stderr, explain("%q cannot be a project name: %v", name, err)), true
		}
		ui := setup.Terminal(stdin, stdout)
		p, err := addProject(ui, name, ps, cmd == "new")
		if err != nil {
			return nil, failAdd(ui, stderr, err), true
		}
		choose(p, true)
		return rest, 0, false
	}
	switch {
	case cmd == "status" && len(ps) > 1:
		return nil, statusAll(ps, stdout, stderr), true
	case creates && len(ps) > 0 && (len(ps) > 1 || ps[0].ready()):
		// Setup again, or a new game: for one of the projects, or a new
		// one. A finished project is not quietly set up a second time.
		if !atTerminal() {
			if len(ps) == 1 {
				choose(ps[0], false)
				return rest, 0, false
			}
			return nil, report(stderr, explain("%s\nA new project: maestro %s --project NAME", severalProjects(ps), cmd)), true
		}
		ui := setup.Terminal(stdin, stdout)
		options := make([]string, 0, len(ps)+1)
		for _, p := range ps {
			options = append(options, p.label())
		}
		question := "Set up which project?"
		if cmd == "new" {
			question = "Is the game a new project, or one that is already here?"
			options = append([]string{"A new project for the game"}, options...)
		} else {
			options = append(options, "A new project")
		}
		i, err := ui.Choose(question, options)
		if err != nil {
			return nil, failAdd(ui, stderr, err), true
		}
		if cmd == "new" {
			i = (i + len(ps)) % (len(ps) + 1) // the new project is shown first
		}
		if i < len(ps) {
			choose(ps[i], true)
			return rest, 0, false
		}
		p, err := addProject(ui, "", ps, cmd == "new")
		if err != nil {
			return nil, failAdd(ui, stderr, err), true
		}
		choose(p, true)
	}
	return rest, 0, false
}

func failAdd(ui *setup.UI, stderr io.Writer, err error) int {
	if errors.Is(err, setup.ErrAborted) {
		fmt.Fprintln(stderr, "No project was made.")
		return 3
	}
	return report(stderr, err)
}

// suggestWorker offers a worker name for a new project that no other
// project has.
func suggestWorker(name string, taken []string) string {
	machine := setup.DefaultWorker(runtime.GOOS)
	try := machine + "-" + name
	if len(try) > 31 {
		try = try[:31]
	}
	try = strings.TrimRight(try, "-")
	for n := 2; contains(taken, try); n++ {
		suffix := fmt.Sprintf("-%d", n)
		base := machine + "-" + name
		if len(base)+len(suffix) > 31 {
			base = base[:31-len(suffix)]
		}
		try = base + suffix
	}
	return try
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func workersOf(ps []project, except string) []string {
	var out []string
	for _, p := range ps {
		if p.State.Worker != "" && !samePath(p.Dir, except) {
			out = append(out, p.State.Worker)
		}
	}
	return out
}

// addProject makes a new project: its directory, a worker name nobody else
// uses and, when another project already has a GitHub App for the same
// owner, the choice to use that App. The setup itself is the wizard's.
func addProject(ui *setup.UI, name string, ps []project, forGame bool) (project, error) {
	home, err := maestroHome()
	if err != nil {
		return project{}, err
	}
	ui.Say("")
	ui.Say("A new project gets its own settings, board or labels, rule packs, sandbox image, Claude login and")
	ui.Say("background service. Its secrets, such as a Unity licence, are only given to its own checks.")
	valid := func(s string) error {
		if err := validProjectName(s); err != nil {
			return err
		}
		if _, ok := findProject(ps, s); ok || s == "first" {
			return fmt.Errorf("there is already a project called %s", s)
		}
		return nil
	}
	if name == "" {
		if name, err = ui.Ask("What should the new project be called?", "", valid); err != nil {
			return project{}, err
		}
	} else if err := valid(name); err != nil {
		return project{}, err
	}
	// The first project names itself, so that adding a second one does not
	// rename it when its worker changes.
	if len(ps) == 1 && ps[0].First {
		if _, err := os.Stat(filepath.Join(home, projectNameFile)); errors.Is(err, os.ErrNotExist) {
			os.WriteFile(filepath.Join(home, projectNameFile), []byte(ps[0].Name+"\n"), 0o600)
		}
	}
	if err := os.MkdirAll(filepath.Join(home, projectsDir), 0o700); err != nil {
		return project{}, err
	}
	dir := filepath.Join(home, projectsDir, name)
	st, err := setup.LoadState(dir)
	if err != nil {
		return project{}, err
	}
	if st.Worker == "" {
		st.Worker = suggestWorker(name, workersOf(ps, dir))
	}
	if err := st.Save(); err != nil {
		return project{}, err
	}
	p := project{Name: name, Dir: dir, State: st}
	if err := offerApp(ui, st, ps, forGame); err != nil {
		return project{}, err
	}
	ui.Say("The project %s is kept in %s.", name, dir)
	ui.Say("")
	return p, nil
}

// offerApp lets a new project use the GitHub App of another project, which
// saves making a second one on github.com. Each project's tokens are still
// limited to its own repository.
//
// For a game the repository is made later by `maestro new`, so instead of
// asking for it the person says whether it will have the same owner.
func offerApp(ui *setup.UI, st *setup.State, ps []project, forGame bool) error {
	var apps []project
	for _, p := range ps {
		shared := false
		for _, q := range apps {
			shared = shared || q.State.AppID == p.State.AppID
		}
		if p.State.AppID != "" && p.State.AppKeyFile != "" && p.State.AppSlug != "" && !shared {
			apps = append(apps, p)
		}
	}
	if len(apps) == 0 || st.AppID != "" {
		return nil
	}
	var from project
	if forGame {
		var options []string
		for _, p := range apps {
			owner, _, _ := strings.Cut(p.State.Repo, "/")
			options = append(options, fmt.Sprintf("Use the App %s of %s: the game's repository will be under %s", p.State.AppSlug, p.Name, owner))
		}
		options = append(options, "Make a new App for this project")
		ui.Say("Another project already has a GitHub App. The game can use it too, if its repository has the")
		ui.Say("same owner; every token the service takes stays limited to the one repository of its project.")
		i, err := ui.Choose("Which GitHub App should this project use?", options)
		if err != nil {
			return err
		}
		if i == len(apps) {
			return nil
		}
		from = apps[i]
	} else {
		repo, err := ui.Ask("Which repository should the agents of this project work on (owner/name)?", st.Repo, func(s string) error {
			if owner, name, ok := strings.Cut(s, "/"); !ok || owner == "" || name == "" || strings.ContainsAny(s, " \t") {
				return errors.New("write it as owner/name, for example acme/game")
			}
			return nil
		})
		if err != nil {
			return err
		}
		st.Repo = repo
		owner, _, _ := strings.Cut(repo, "/")
		var same []project
		for _, p := range apps {
			if o, _, _ := strings.Cut(p.State.Repo, "/"); strings.EqualFold(o, owner) {
				same = append(same, p)
			}
		}
		if len(same) == 0 {
			return st.Save()
		}
		from = same[0]
		ui.Say("The project %s already has a GitHub App for %s: %s.", from.Name, owner, from.State.AppSlug)
		ui.Say("This project can use it too, so you do not make a second App. It is then also installed on %s,", repo)
		ui.Say("and every token the service takes stays limited to the one repository of the project it works for.")
		options := []string{"Use the App " + from.State.AppSlug, "Make a new App for this project"}
		i, err := ui.Choose("Which GitHub App should this project use?", options)
		if err != nil {
			return err
		}
		if i != 0 {
			return st.Save()
		}
	}
	key, err := os.ReadFile(from.State.AppKeyFile)
	if err != nil {
		return fmt.Errorf("the App's key could not be read: %w", err)
	}
	keyPath := filepath.Join(st.Dir(), setup.KeyFileName)
	f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(key)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(keyPath)
		return werr
	}
	st.AppID, st.AppSlug, st.AppKeyFile, st.OwnerType = from.State.AppID, from.State.AppSlug, keyPath, from.State.OwnerType
	return st.Save()
}

const projectsUsage = `usage:
  maestro projects                   the projects on this machine and whether they run
  maestro projects add [NAME]        make a new project and set it up
  maestro projects remove NAME       stop a project's service and put its files aside

Every other command works on one project. Name it with --project NAME, for
example: maestro status --project game. With several projects and no name,
maestro asks which one; ` + "`maestro status`" + ` shows them all.`

// runProjects is `maestro projects`.
func runProjects(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	ps, err := listProjects()
	if err != nil {
		return report(stderr, err)
	}
	switch {
	case len(args) == 0 || args[0] == "list":
		return listProjectsOut(ps, stdout)
	case args[0] == "add" && len(args) <= 2:
		name := ""
		if len(args) == 2 {
			name = args[1]
		}
		if len(ps) == 0 {
			// The first project is the plain setup.
			if name != "" {
				if err := validProjectName(name); err != nil {
					return report(stderr, explain("%q cannot be a project name: %v", name, err))
				}
			}
			code := run([]string{"setup"}, stdout, stderr)
			if home, err := maestroHome(); code == 0 && err == nil && name != "" {
				os.WriteFile(filepath.Join(home, projectNameFile), []byte(name+"\n"), 0o600)
			}
			return code
		}
		ui := setup.Terminal(stdin, stdout)
		p, err := addProject(ui, name, ps, false)
		if err != nil {
			return failAdd(ui, stderr, err)
		}
		return run([]string{"setup", "--project", p.Name}, stdout, stderr)
	case args[0] == "remove" && len(args) == 2:
		return removeProject(ps, args[1], stdin, stdout, stderr)
	}
	fmt.Fprintln(stderr, projectsUsage)
	return 2
}

func listProjectsOut(ps []project, stdout io.Writer) int {
	if len(ps) == 0 {
		fmt.Fprintln(stdout, "There is no project on this machine yet. Start with: maestro setup")
		return 0
	}
	ctx, stop := interruptible()
	defer stop()
	fmt.Fprintf(stdout, "%d project(s) on this machine:\n", len(ps))
	for _, p := range ps {
		state := "setup not finished: maestro setup --project " + p.Name
		if p.ready() {
			state = "not running"
			if in, err := open(p.Dir, p.State.WorkflowFile, nil); err != nil {
				state = "has a problem: maestro check --project " + p.Name
			} else if alive(ctx, in.cfg.Budget.StateDir) {
				state = "running"
			} else if u, err := unitFor(in); err == nil && u.Installed() {
				state = "background service installed, not answering"
			}
		}
		fmt.Fprintf(stdout, "  %-14s %s\n", p.Name, plain(p.details(), 100))
		fmt.Fprintf(stdout, "  %-14s %s\n", "", state)
	}
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Work on one with --project NAME, for example: maestro run --project "+ps[len(ps)-1].Name)
	fmt.Fprintln(stdout, "A new one: maestro projects add")
	return 0
}

// removeProject stops the project's background service and moves its
// directory aside. Nothing is deleted: the App key and the settings stay
// on disk until the person removes them.
func removeProject(ps []project, name string, stdin io.Reader, stdout, stderr io.Writer) int {
	p, ok := findProject(ps, name)
	if !ok {
		return report(stderr, explain("There is no project called %s. `maestro projects` lists them.", name))
	}
	if p.First {
		return report(stderr, explain("%s is the first project, which lives in Maestro's own directory %s.\n"+
			"It cannot be removed with this command. To stop its background service: maestro service uninstall --project %s",
			p.Name, p.Dir, p.Name))
	}
	ctx, stop := interruptible()
	defer stop()
	ui := setup.Terminal(stdin, stdout)
	in, openErr := open(p.Dir, p.State.WorkflowFile, nil)
	if openErr == nil && alive(ctx, in.cfg.Budget.StateDir) {
		if u, err := unitFor(in); err != nil || !u.Installed() {
			return report(stderr, explain("%s is running in a terminal. Stop it there with Ctrl-C first.", name))
		}
	}
	home, err := maestroHome()
	if err != nil {
		return report(stderr, err)
	}
	aside := filepath.Join(home, "removed", name+"-"+time.Now().Format("20060102-150405"))
	ui.Say("This stops the background service of %s, if it has one, and moves its files to", name)
	ui.Say("    %s", aside)
	ui.Say("Nothing is deleted: its settings and App key stay there until you remove the folder.")
	ui.Say("Its Docker volumes stay as well (their names start with maestro-%s-).", p.State.Worker)
	yes, err := ui.Confirm("Remove the project " + name + "?")
	if err != nil || !yes {
		fmt.Fprintln(stderr, "Nothing was changed.")
		return 3
	}
	if openErr == nil {
		if u, err := unitFor(in); err == nil && u.Installed() {
			if err := u.Uninstall(ctx); err != nil {
				return report(stderr, err)
			}
			ui.Say("The background service of %s is stopped and removed.", name)
		}
	}
	if err := os.MkdirAll(filepath.Dir(aside), 0o700); err != nil {
		return report(stderr, err)
	}
	if err := os.Rename(p.Dir, aside); err != nil {
		return report(stderr, err)
	}
	ui.Say("The project %s is removed. Its files are in %s.", name, aside)
	return 0
}

// statusAll is `maestro status` with several projects and none named.
func statusAll(ps []project, stdout, stderr io.Writer) int {
	ctx, stop := interruptible()
	defer stop()
	now := time.Now()
	for i, p := range ps {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		fmt.Fprintf(stdout, "── %s\n", plain(p.label(), 100))
		if !p.ready() {
			fmt.Fprintf(stdout, "Setup is not finished. Continue it with: maestro setup --project %s\n", p.Name)
			continue
		}
		in, err := open(p.Dir, p.State.WorkflowFile, nil)
		if err != nil {
			var pr *problem
			if errors.As(err, &pr) {
				fmt.Fprintln(stdout, pr.text)
			} else {
				fmt.Fprintln(stdout, "error:", err)
			}
			continue
		}
		var s statusJSON
		if err := reach(ctx, in.cfg.Budget.StateDir, http.MethodGet, "/api/v1/state", nil, &s); err != nil {
			fmt.Fprintf(stdout, "Not running. Start it with: maestro run --project %s\n", p.Name)
			continue
		}
		printStatus(stdout, s, now)
	}
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "One project only: maestro status --project NAME")
	return 0
}

// clash refuses a project whose worker name another project uses: they
// would share containers, volumes, the Claude login and the service.
func clash(dir, worker string) error {
	ps, err := listProjects()
	if err != nil {
		return nil
	}
	for _, p := range ps {
		if samePath(p.Dir, dir) {
			continue
		}
		if p.State.Worker == worker {
			return explain("The project %s uses the worker name %s too. Two projects with one worker name would share\n"+
				"their containers, their Claude login and their service. Change worker.name in %s\n"+
				"(and on the board or the run: label), or run `maestro setup` for this project again.",
				p.Name, worker, filepath.Join(dir, workflow.DefaultFileName))
		}
	}
	return nil
}

// defaultImage is the sandbox image of a project whose settings name none.
// The first project keeps the name it always had; every other project gets
// its own, so that building one project's image for its rule packs never
// replaces another's.
func defaultImage(dir string) string {
	home, err := maestroHome()
	if err != nil {
		return ""
	}
	parent := filepath.Join(home, projectsDir)
	if samePath(filepath.Dir(dir), parent) {
		return "maestro-sandbox-" + filepath.Base(dir) + ":latest"
	}
	return ""
}

// mention adds --project to a command that is shown to the person, when
// there are several projects, so that the command can be copied as it is.
func mention(command string) string {
	if chosen.name == "" {
		return command
	}
	if ps, err := listProjects(); err != nil || len(ps) < 2 {
		return command
	}
	return command + " --project " + chosen.name
}
