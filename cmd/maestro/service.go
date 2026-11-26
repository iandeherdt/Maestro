package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/iandeherdt/Maestro/internal/budget"
	"github.com/iandeherdt/Maestro/internal/claude"
	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/conventions"
	"github.com/iandeherdt/Maestro/internal/dockerbox"
	"github.com/iandeherdt/Maestro/internal/gate"
	"github.com/iandeherdt/Maestro/internal/hostcheck"
	"github.com/iandeherdt/Maestro/internal/httpapi"
	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/launch"
	"github.com/iandeherdt/Maestro/internal/logging"
	"github.com/iandeherdt/Maestro/internal/mirror"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
	"github.com/iandeherdt/Maestro/internal/plan"
	"github.com/iandeherdt/Maestro/internal/proxy"
	"github.com/iandeherdt/Maestro/internal/publish"
	"github.com/iandeherdt/Maestro/internal/reload"
	"github.com/iandeherdt/Maestro/internal/setup"
	"github.com/iandeherdt/Maestro/internal/skills"
	"github.com/iandeherdt/Maestro/internal/systemdbox"
	"github.com/iandeherdt/Maestro/internal/trace"
	"github.com/iandeherdt/Maestro/internal/tracker"
	"github.com/iandeherdt/Maestro/internal/tracker/github"
	"github.com/iandeherdt/Maestro/internal/worker"
	"github.com/iandeherdt/Maestro/internal/workspace"
)

// install is what `run`, `image` and `login` share: the workflow, the setup
// state and the sandbox they describe.
type install struct {
	path   string
	source *reload.Source
	cfg    *config.Config
	state  *setup.State
	// box is the sandbox; dbox is the same box when it is the container
	// backend, and nil with systemd. Images, Podman and the Unity
	// activation exist only there.
	box    sandboxBox
	dbox   *dockerbox.Box
	bot    string
	skills skills.Store
	// dir is where setup keeps its state.
	dir string
	// Directories of the control zone.
	mirrorDir, handoffDir, allowDir, auditDir string
}

// problem is an error whose text is written for the person at the terminal.
type problem struct{ text string }

func (p *problem) Error() string { return p.text }

func explain(format string, a ...any) error { return &problem{fmt.Sprintf(format, a...)} }

// parse reads the flags the service commands share.
func parse(name string, args []string, stderr io.Writer) (dir, explicit string, ok bool) {
	dir, explicit, _, ok = parseWords(name, args, stderr, false)
	return dir, explicit, ok
}

// parseWords is parse for a command whose remaining words are free text.
func parseWords(name string, args []string, stderr io.Writer, words bool) (dir, explicit string, rest []string, ok bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	d := fs.String("dir", "", "where setup keeps its state (default: the user's configuration directory)")
	f := fs.String("file", "", "the settings file (default: the one setup wrote)")
	err := fs.Parse(args)
	switch {
	case err != nil, !words && fs.NArg() > 1, !words && fs.NArg() == 1 && *f != "":
		fmt.Fprintf(stderr, "usage: maestro %s [--dir DIR] [--file WORKFLOW.md]\n", name)
		return "", "", nil, false
	case words:
		return *d, *f, fs.Args(), true
	case *f != "":
		return *d, *f, nil, true
	}
	return *d, fs.Arg(0), nil, true
}

// open loads the workflow and the setup state and describes the sandbox.
// It starts nothing.
func open(dir, explicit string, log *slog.Logger) (*install, error) {
	var err error
	if dir == "" {
		if dir, err = configDir(); err != nil {
			return nil, explain("%v", err)
		}
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return nil, err
	}
	in := &install{path: workflowIn(dir, explicit)}
	home, _ := os.UserHomeDir()
	src, err := reload.Open(in.path, config.Options{Env: os.Getenv, HomeDir: home, DefaultImage: defaultImage(dir)}, log)
	if err != nil {
		return nil, explain("%v\nThe settings file is %s. Run `maestro setup` if you have not yet.", err, in.path)
	}
	in.source, in.cfg = src, src.Current().Config
	if b := in.cfg.Sandbox.Backend; b != "docker" && b != "podman" && b != "systemd" {
		return nil, explain("%s says `sandbox.backend: %s`, which is not built yet. The container sandbox (docker or podman) and systemd are.\n"+
			"If setup wrote this file, the easiest fix is to let it write a new one:\n"+
			"  mv %q %q\n  maestro setup\n"+
			"Setup skips the steps that are done and only asks the last questions again.",
			in.path, in.cfg.Sandbox.Backend, in.path, in.path+".old")
	}
	if k := in.cfg.Tracker.Kind; k != "github" && k != "linear" {
		return nil, explain("the tracker %q is not built; github and linear are", k)
	}
	if err := clash(dir, in.cfg.Worker.Name); err != nil {
		return nil, err
	}
	st, err := setup.LoadState(dir)
	if err != nil {
		return nil, err
	}
	if in.cfg.Workspace.Host != st.HostName() {
		return nil, explain("setup was done for the code on %s, but %s says `workspace.host: %s`. Run `maestro setup --host %s`.",
			st.HostName(), in.path, in.cfg.Workspace.Host, in.cfg.Workspace.Host)
	}
	switch {
	case st.UsesGitLab():
		if st.GitLabBot == "" || st.GitLabProject <= 0 {
			return nil, explain("the GitLab project is not set up in %s. Run `maestro setup --host gitlab` first.", dir)
		}
	case st.UsesGitea():
		if st.GiteaBot == "" || st.HostURL == "" {
			return nil, explain("the Gitea repository is not set up in %s. Run `maestro setup --host gitea` first.", dir)
		}
	case st.AppID == "" || st.AppKeyFile == "" || st.InstallationID == "" || st.AppSlug == "":
		return nil, explain("the GitHub App is not set up in %s. Run `maestro setup` first.", dir)
	}
	if !strings.EqualFold(st.Repo, in.cfg.Workspace.Repo) {
		return nil, explain("setup was done for %s, but %s is about %s. Run `maestro setup` for that repository.",
			st.Repo, in.path, in.cfg.Workspace.Repo)
	}
	in.state = st

	stateDir := in.cfg.Budget.StateDir
	in.mirrorDir = filepath.Join(stateDir, "mirror")
	in.allowDir = filepath.Join(stateDir, "proxy")
	in.auditDir = filepath.Join(stateDir, "audit")
	in.handoffDir = filepath.Join(in.cfg.Workspace.Root, ".handoff")
	bot := st.AppSlug + "[bot]"
	switch {
	case st.UsesGitLab():
		bot = st.GitLabBot
	case st.UsesGitea():
		bot = st.GiteaBot
	}
	in.dir = dir
	in.skills = skills.Store{Dir: filepath.Join(dir, "agent-files")}
	if err := in.skills.Ensure(); err != nil {
		return nil, err
	}
	in.bot = bot
	packs, err := conventions.Resolve(in.cfg.Conventions)
	if err != nil {
		return nil, err
	}
	if err := conventions.SyncSkills(in.skills.SkillsDir(), packs); err != nil {
		return nil, err
	}
	if in.cfg.Sandbox.Backend == "systemd" {
		if slices.Contains(in.cfg.Conventions, "unity") {
			return nil, explain("the unity pack needs the container sandbox: Unity's Editor for Linux comes with the sandbox image.\n" +
				"Use `sandbox.backend: podman` (or docker) for this project.")
		}
		in.box = &systemdbox.Box{Worker: in.cfg.Worker.Name, SocketDir: filepath.Join(stateDir, "proxy-sockets"), Log: log}
		// The launcher acts on the hand-off folder as root, so it is
		// root's: `maestro setup` makes it, and the service only reads the
		// bundles the launcher leaves there for it.
		in.handoffDir = systemdHandoff(in.cfg.Worker.Name)
		return in, nil
	}
	in.dbox = &dockerbox.Box{
		SkillsDir: in.skills.SkillsDir(), AgentsDir: in.skills.AgentsDir(),
		Podman: in.cfg.Sandbox.Backend == "podman",
		Worker: in.cfg.Worker.Name, Image: in.cfg.Sandbox.Image,
		MirrorDir: in.mirrorDir, HandoffDir: in.handoffDir,
		Memory: strings.ToLower(in.cfg.Sandbox.MemoryMax), CPUs: cpus(in.cfg.Sandbox.CPUQuota), PIDs: in.cfg.Sandbox.TasksMax,
		BotName: bot, BotEmail: in.botEmail(),
	}
	// The checks get the Unity licence when the unity pack is on and there
	// is one (`maestro unity activate`). The agent's own step never does.
	if lic := unityStore(dir); lic.Has() && slices.Contains(in.cfg.Conventions, "unity") {
		in.dbox.UnityLicenceDir = lic.Dir
	}
	in.dbox.Platform = conventions.Derive(packs).Platform
	in.box = in.dbox
	return in, nil
}

// cpus turns "300%" into docker's "3.00".
func cpus(quota string) string {
	n, err := strconv.Atoi(strings.TrimSuffix(quota, "%"))
	if err != nil || n <= 0 {
		return ""
	}
	return fmt.Sprintf("%d.%02d", n/100, n%100)
}

// docker checks that the container command is installed and answers.
func (in *install) docker(ctx context.Context) error {
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if in.dbox == nil {
		return in.systemdReady(ctx)
	}
	if in.dbox.Podman {
		if _, err := exec.LookPath("podman"); err != nil {
			return explain("the `podman` command was not found. Install it (on RHEL: sudo dnf install podman) and try again.")
		}
		if err := exec.CommandContext(c, "podman", "info").Run(); err != nil {
			return explain("Podman is installed but `podman info` fails for this user. Run it yourself to see why.")
		}
		out, err := exec.CommandContext(c, "podman", "info", "--format", "{{.Host.NetworkBackend}}").Output()
		if err == nil && strings.TrimSpace(string(out)) == "cni" {
			return explain(cniHelp)
		}
		return nil
	}
	if _, err := exec.LookPath("docker"); err != nil {
		if inWSL() {
			return explain(wslDockerHint)
		}
		return explain("the `docker` command was not found. Install Docker Desktop (Mac) or Docker Engine (Linux) and try again.")
	}
	if err := exec.CommandContext(c, "docker", "version", "--format", "{{.Server.Version}}").Run(); err != nil {
		if inWSL() {
			return explain("Docker does not answer. " + wslDockerHint)
		}
		return explain("Docker is installed but not running. Start Docker Desktop, wait until it says it is running, and try again.")
	}
	return nil
}

// cniHelp is said when Podman uses its old CNI network backend (RHEL 8).
// CNI turns DNS off on internal networks, so the agent cannot find its
// proxy by name.
const cniHelp = `Podman uses its old CNI network backend. On it, containers on an internal network cannot
find each other by name, so the agent cannot reach its proxy. Switch Podman to netavark:
  sudo dnf install -y netavark aardvark-dns
  podman ps -a -q --filter label=maestro.worker | xargs -r podman rm -f
  podman network ls --format '{{.Name}}' | grep '^maestro-' | xargs -r podman network rm
  mkdir -p ~/.config/containers
  printf '[network]\nnetwork_backend = "netavark"\n' >> ~/.config/containers/containers.conf
Then podman info --format '{{.Host.NetworkBackend}}' says netavark. Your images are kept.`

// hostChecks runs the unity pack's checks with the Editor of this Mac
// (unity.editor), inside macOS's sandbox. Nil where there is no such
// sandbox; a settings file that asks for them then fails at the checks
// with words.
func (in *install) hostChecks(cfg func() *config.Config, log *slog.Logger) gate.HostChecks {
	if runtime.GOOS != "darwin" {
		return nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil
	}
	return &hostcheck.Runner{
		MirrorDir: in.mirrorDir, HandoffDir: in.handoffDir,
		// Unity's Library is big and can be made again: it belongs in the
		// caches, apart from Maestro's setup, which the checks cannot read.
		WorkDir: filepath.Join(cache, "maestro", "unity-checks", in.cfg.Worker.Name),
		Secret:  absAll(in.dir, in.cfg.Budget.StateDir, in.cfg.Workspace.Root),
		Allow:   func() []string { return cfg().Sandbox.AllowHooks },
		Log:     log,
	}
}

func absAll(paths ...string) []string {
	var out []string
	for _, p := range paths {
		if a, err := filepath.Abs(p); err == nil && p != "" {
			out = append(out, a)
		}
	}
	return out
}

// allow writes the proxies' allowlists from the configuration.
func (in *install) allow(cfg *config.Config) error {
	agent := append(append([]string{}, proxy.DefaultAgentHosts...), cfg.Sandbox.AllowAgent...)
	if err := dockerbox.WriteAllow(in.allowDir, "agent", agent); err != nil {
		return err
	}
	return dockerbox.WriteAllow(in.allowDir, "hooks", cfg.Sandbox.AllowHooks)
}

// up prepares the directories and starts the proxies.
func (in *install) up(ctx context.Context) error {
	if err := in.dirs(); err != nil {
		return err
	}
	if err := in.allow(in.cfg); err != nil {
		return err
	}
	return in.box.Up(ctx, in.allowDir)
}

// dirs makes the directories of the control zone and sets who may enter
// them. Containers read the mirror and the allowlists, and write into the
// hand-off folder of a workspace, as the sandbox user and not as the
// service, so those folders are open to every user. What keeps the other
// users of the machine out is the folder above them: a container is given
// the folder itself and never walks the path to it, another user has to.
// So the state directory and the hand-off directory are the service's
// alone.
func (in *install) dirs() error {
	for _, d := range []string{in.cfg.Budget.StateDir, in.handoffDir, in.auditDir} {
		if in.cfg.Sandbox.Backend == "systemd" && d == in.handoffDir {
			continue // root's, with the systemd sandbox
		}
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	for _, d := range []string{in.mirrorDir, in.allowDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func report(stderr io.Writer, err error) int {
	var p *problem
	if errors.As(err, &p) {
		fmt.Fprintln(stderr, p.text)
	} else {
		fmt.Fprintln(stderr, "error:", err)
	}
	return 1
}

func interruptible() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// browserLine adds a browser and fonts to the sandbox image.
const browserLine = "RUN apt-get update && apt-get install -y --no-install-recommends chromium fonts-liberation fonts-noto-color-emoji && rm -rf /var/lib/apt/lists/*"

// imagePacksLabel says which conventions packs the image was built for.
const imagePacksLabel = "maestro.conventions"

// imageLinesLabel holds a hash of the packs' own lines of the image, so a
// newer Maestro whose packs install something more can tell the image is
// out of date.
const imageLinesLabel = "maestro.image-lines"

// imageExtra is what the image gets on top of the base: the packs' lines
// and, when the service takes screenshots (SPEC 24 and 25), the browser.
func imageExtra(packs []*conventions.Pack, browser bool) []string {
	extra := conventions.Derive(packs).Image
	if browser {
		extra = append(extra, browserLine)
	}
	return extra
}

func linesHash(lines []string) string {
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:8])
}

// runImage builds the sandbox image.
func runImage(args []string, stdout, stderr io.Writer) int {
	dir, explicit, ok := parse("image", args, stderr)
	if !ok {
		return 2
	}
	ctx, stop := interruptible()
	defer stop()
	in, err := open(dir, explicit, nil)
	if err == nil {
		err = in.docker(ctx)
	}
	if err != nil {
		return report(stderr, err)
	}
	packs, err := conventions.Resolve(in.cfg.Conventions)
	if err != nil {
		return report(stderr, err)
	}
	extra := imageExtra(packs, len(in.cfg.Screenshots.Pages) > 0 || in.cfg.Tracker.DesignState != "")
	b := dockerbox.Build{Out: stdout, Version: version, Extra: extra,
		Labels: map[string]string{imagePacksLabel: strings.Join(in.cfg.Conventions, ","), imageLinesLabel: linesHash(extra)}}

	if !strings.HasPrefix(b.Version, "v") {
		b.Version = "v" + b.Version
	}
	if in.dbox == nil {
		fmt.Fprintln(stdout, "The systemd sandbox uses no image: its steps run Claude Code from "+launch.DefaultTools+".")
		return 0
	}
	if runtime.GOOS == "linux" && (in.dbox.Platform == "" || in.dbox.Platform == "linux/"+runtime.GOARCH) {
		if self, err := os.Executable(); err == nil {
			b.Binary = self
		}
	}
	fmt.Fprintf(stdout, "Building the sandbox image %s. The first time takes a few minutes: it downloads\n", in.cfg.Sandbox.Image)
	fmt.Fprintf(stdout, "Node.js, git and Claude Code %s. Nothing is installed on this machine outside Docker.\n", dockerbox.ClaudeVersion)
	for _, p := range packs {
		if len(p.Image) > 0 {
			fmt.Fprintf(stdout, "For the conventions pack %s it also gets %s.\n", p.Name, p.ImageNote)
		}
	}
	fmt.Fprintln(stdout)
	if err := in.dbox.BuildImage(ctx, b); err != nil {
		return report(stderr, err)
	}
	fmt.Fprintln(stdout, "\nThe image is ready.")
	fmt.Fprintf(stdout, "Next: %s    (sign Claude Code in inside the sandbox, once)\n", mention("maestro login"))
	return 0
}

// image explains what to do when the image is missing.
func (in *install) image(ctx context.Context) error {
	if in.dbox == nil {
		return nil
	}
	ok, err := in.dbox.ImageState(ctx)
	if err != nil {
		return explain("Docker could not say whether the sandbox image %s is there: %v\nTry again in a moment; Docker Desktop may still be starting.", in.cfg.Sandbox.Image, err)
	}
	if !ok {
		return explain("the sandbox image %s does not exist yet. Build it with: %s", in.cfg.Sandbox.Image, mention("maestro image"))
	}
	return nil
}

// runLogin signs Claude Code in inside the sandbox.
func runLogin(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	dir, explicit, ok := parse("login", args, stderr)
	if !ok {
		return 2
	}
	ctx, stop := interruptible()
	defer stop()
	in, err := open(dir, explicit, nil)
	if err == nil {
		err = in.docker(ctx)
	}
	if err == nil {
		err = in.image(ctx)
	}
	if err != nil {
		return report(stderr, err)
	}
	// A running service already has the proxies up. Starting them again
	// would stop its work, so they are only started when they are missing.
	if !in.box.ProxyRunning(ctx, "agent") {
		if err := in.up(ctx); err != nil {
			return report(stderr, err)
		}
		defer in.box.Down(context.Background())
	}
	fmt.Fprintln(stdout, "Claude Code's sign-in now runs inside the sandbox, with your Claude subscription.")
	fmt.Fprintln(stdout, "  1. It shows a link. Open it in your browser and approve.")
	fmt.Fprintln(stdout, "  2. The browser shows a code. Paste it back here and press Enter.")
	if in.dbox != nil {
		fmt.Fprintln(stdout, "The login is stored in a Docker volume that only the agent's containers get. It is not copied")
		fmt.Fprintln(stdout, "to this machine's files or keychain.")
	} else {
		fmt.Fprintln(stdout, "The login is stored in the sandbox user's folder, which only the agent's steps see. It is not")
		fmt.Fprintln(stdout, "copied to your files or keychain.")
	}
	fmt.Fprintln(stdout)
	in.box.Login(ctx, []string{in.cfg.Claude.Command, "auth", "login", "--claudeai"}, stdin, stdout, stderr)
	fmt.Fprintln(stdout)
	yes, err := in.box.LoggedIn(ctx, in.cfg.Claude.Command)
	if err != nil {
		return report(stderr, err)
	}
	if !yes {
		fmt.Fprintln(stderr, "Claude Code is not signed in yet. Run `maestro login` again and finish the sign-in.")
		return 1
	}
	fmt.Fprintln(stdout, "Claude Code is signed in inside the sandbox.")
	fmt.Fprintln(stdout, "Next: "+mention("maestro run"))
	return 0
}

// view is the configuration in force, shared between goroutines.
type view struct{ p atomic.Pointer[reload.Loaded] }

func (v *view) config() *config.Config { return v.p.Load().Config }

// audit appends the raw agent stream of a workspace to a file.
type audit struct{ path string }

func (a audit) Write(p []byte) (int, error) {
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.Write(p)
}

// synced refreshes the mirror before a workspace is created or continued,
// so that new work starts from the current base branch.
type synced struct {
	*workspace.Manager
	sync func(context.Context) error
	log  *slog.Logger
	mu   sync.Mutex
	// host drops what the checks on this machine kept for a workspace.
	host gate.HostChecks
}

// Remove removes the workspace, and the folder its checks on this machine
// used, which holds Unity's imported assets.
func (s *synced) Remove(ctx context.Context, identifier string) error {
	if err := s.Manager.Remove(ctx, identifier); err != nil {
		return err
	}
	if k, err := issue.WorkspaceKey(identifier); err == nil && s.host != nil {
		s.host.Forget(k)
	}
	return nil
}

func (s *synced) Ensure(ctx context.Context, is issue.Issue) (workspace.Workspace, error) {
	s.mu.Lock()
	if err := s.sync(ctx); err != nil {
		// The mirror from the last fetch is still good to work from.
		s.log.Warn("the mirror could not be refreshed", "error", err)
	}
	s.mu.Unlock()
	return s.Manager.Ensure(ctx, is)
}

// defaultBranch asks GitHub for the repository's default branch.
func defaultBranch(ctx context.Context, apiURL, repo, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/repos/"+repo, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	hc := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub answered %d for the repository", res.StatusCode)
	}
	var body struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body); err != nil {
		return "", err
	}
	if !mirror.ValidBranch(body.DefaultBranch) {
		return "", fmt.Errorf("the repository's default branch has a name the service does not accept")
	}
	return body.DefaultBranch, nil
}

// endpoints are where GitHub is. They change only in tests.
type endpoints struct {
	api, git string
	// fileRemote lets git be a local directory.
	fileRemote bool
}

var gitHub = endpoints{api: "https://api.github.com", git: "https://github.com"}

// runService is `maestro run`.
func runService(args []string, stdout, stderr io.Writer) int {
	dir, explicit, ok := parse("run", args, stderr)
	if !ok {
		return 2
	}
	ctx, stop := interruptible()
	defer stop()
	log := logging.New(stderr, slog.LevelInfo)
	in, err := open(dir, explicit, log)
	if err == nil {
		err = in.docker(ctx)
	}
	if err == nil {
		err = in.image(ctx)
	}
	if err == nil {
		err = systemdCurrent(in)
	}
	if err == nil {
		err = serve(ctx, in, gitHub, log, stdout)
	}
	if err != nil {
		return report(stderr, err)
	}
	return 0
}

// serve runs the service until ctx ends.
func serve(ctx context.Context, in *install, gh endpoints, log *slog.Logger, stdout io.Writer) error {
	cfg := in.cfg
	if _, err := exec.LookPath("git"); err != nil {
		return explain("the `git` command was not found on this machine. Install git and try again.")
	}
	cs, err := in.codeSide(ctx, gh, log)
	if err != nil {
		return err
	}
	code, tickets, onBoard := cs.code, cs.tickets, cs.onBoard
	base := cfg.Workspace.BaseBranch
	if base == "" {
		if base, err = cs.defaultBranch(ctx); err != nil {
			return err
		}
	}
	// A reloaded file that names no base branch gets the same answer.
	patch := func(l *reload.Loaded) *reload.Loaded {
		if l.Config.Workspace.BaseBranch == "" {
			l.Config.Workspace.BaseBranch = base
		}
		return l
	}
	var cur view
	cur.p.Store(patch(in.source.Current()))

	// Starting a second service would stop the containers of the first.
	if alive(ctx, cfg.Budget.StateDir) {
		return explain("Maestro is already running for this installation, in another terminal or in the background.\n" +
			"See what it does with `maestro status`. To stop a background service: maestro service uninstall")
	}
	if err := in.up(ctx); err != nil {
		return err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		in.box.Down(c)
	}()
	if err := startupSelfTest(ctx, in, log); err != nil {
		return err
	}
	yes, err := in.box.LoggedIn(ctx, cfg.Claude.Command)
	if err != nil {
		return err
	}
	if !yes {
		return explain("Claude Code is not signed in inside the sandbox. Sign in with: %s", mention("maestro login"))
	}

	mir := &mirror.Mirror{Dir: in.mirrorDir, OwnerUID: -1, MaxBundleBytes: cfg.Publish.MaxBundleBytes, AllowFileRemote: gh.fileRemote}
	remote := cs.remote
	// One fetch at a time: workers refresh the mirror before a workspace,
	// and the orchestrator before it brings a branch up to date.
	var mirrorMu sync.Mutex
	syncMirror := func(ctx context.Context) error {
		mirrorMu.Lock()
		defer mirrorMu.Unlock()
		t, err := code.Token(ctx)
		if err != nil {
			return err
		}
		return mir.Sync(ctx, remote, t, publish.ScreenshotBranch(cur.config().Publish.BranchPrefix))
	}
	if err := syncMirror(ctx); err != nil {
		return explain("the repository %s could not be fetched: %v", cfg.Workspace.Repo, err)
	}
	if !mir.HasBranch(ctx, base) {
		return explain("%s has no branch %q yet. An empty repository gives the agent nothing to start from.\n"+
			"Add a first commit there (a README is enough), then run `maestro run` again.",
			cfg.Workspace.Repo, base)
	}

	// The board: GitHub's issues, or Linear. The code is on GitHub either
	// way.
	var trk tracker.Tracker
	var ghBoard *github.Adapter
	var planner tracker.Planner // the board the planning stage writes to
	var linearKey string
	if cfg.Tracker.Kind == "linear" {
		la, key, err := in.linear(ctx, log)
		if err != nil {
			return err
		}
		trk, planner, linearKey = la, la, key
	} else {
		if ghBoard, err = github.New(cfg, github.Options{Token: tickets.Token, Log: log}); err != nil {
			return err
		}
		trk = ghBoard
		if onBoard {
			planner = ghBoard
		}
	}
	if onBoard {
		// Cards of other repositories on this project's board were put
		// there by a board automation; with several projects, every new
		// issue would land on every board.
		t, err := ghBoard.Tidy(ctx)
		switch {
		case err != nil:
			log.Warn("cards of other repositories could not be taken off the board", "error", err)
		case t.Cards > 0:
			log.Info("took cards of other repositories off the board; the issues themselves stay",
				"cards", t.Cards, "repositories", strings.Join(t.Repos, ", "), "deleted_automations", strings.Join(t.Workflows, ", "))
		}
	}
	store, err := budget.Open(cfg.Budget.StateDir)
	if err != nil {
		return err
	}
	pub := &publish.Publisher{
		Config: cur.config, Mirror: mir, HandoffDir: in.handoffDir, Token: code.Token,
		HeldSecrets: func() []string {
			// The Unity licence reaches code the agent wrote, in the checks:
			// it may not leave in a commit.
			held := append([]string{code.Current()}, unityStore(in.dir).Secrets()...)
			if tickets != nil {
				held = append(held, tickets.Current())
			}
			if linearKey != "" {
				held = append(held, linearKey)
			}
			return held
		},
		BotName: in.bot, BotEmail: in.botEmail(), BotLogin: in.bot,
		APIURL: gh.api, GitURL: gh.git, Fetch: syncMirror,
	}
	spaces := workspace.New(cur.config(), in.box, log)
	host := in.hostChecks(cur.config, log)
	gates := &gate.Gates{Config: cur.config, Launcher: in.box, Publisher: pub, Host: host}
	// What is pushed is the commit the export step named and verify ran on.
	pub.ExportedHead = gates.ExportedHead
	wk := &worker.Worker{
		Settings: func() worker.Settings {
			l := cur.p.Load()
			return worker.Settings{Config: l.Config, Prompt: l.Prompt}
		},
		Tracker:    trk,
		Workspaces: &synced{Manager: spaces, sync: syncMirror, log: log, host: host},
		Runner: &claude.Runner{Config: cur.config, Launcher: in.box,
			Audit: func(key string) io.Writer { return audit{filepath.Join(in.auditDir, key+".jsonl")} },
			Trace: turnTraces(cur.config, trace.New(version, log))},
		Gates: gates,
		Notes: pub,
	}
	// The planning stage needs a board. The names of its states are read
	// at start; changing them takes a restart.
	var plans orchestrator.Plans
	var designs orchestrator.Designs
	if planner != nil && cfg.Tracker.PlanningState != "" {
		plansDir, err := plan.OpenStore(filepath.Join(cfg.Budget.StateDir, "plans"))
		if err != nil {
			return err
		}
		desk := &plan.Desk{Store: plansDir, Tracker: trk, Planner: planner, PlanningState: cfg.Tracker.PlanningState,
			PlannedState: cfg.Tracker.PlannedState, TaskState: cfg.Tracker.ActiveStates[0],
			DesignState: cfg.Tracker.DesignState, DesignFolder: cfg.Design.Folder, Designs: pub,
			TerminalStates: cfg.Tracker.TerminalStates, Self: cfg.Worker.Name}
		wk.Planning, plans = desk, desk
		if cfg.Tracker.DesignState != "" {
			wk.Designing, designs = desk, desk
		}
	}
	orch := orchestrator.New(cur.config(), orchestrator.Deps{
		Tracker: trk, Worker: wk, Workspaces: spaces, Budget: store, Checks: pub, Reviews: pub, Updater: pub, Plans: plans, Designs: designs, Log: log,
		Preflight: func() (*config.Config, error) {
			l, err := in.source.Check()
			if l == nil {
				return nil, err
			}
			cur.p.Store(patch(l))
			if werr := in.allow(l.Config); werr != nil {
				log.Warn("the network allowlists could not be rewritten", "error", werr)
			}
			return l.Config, err
		},
	})
	// The status server is how `maestro status`, `pause` and `resume`
	// reach this process. It listens on loopback only; without a
	// configured port it takes a free one.
	port := 0
	if cfg.Server.Port != nil {
		port = int(*cfg.Server.Port)
	}
	srv, err := httpapi.Listen(port, orch)
	if err != nil {
		return err
	}
	go srv.Serve()
	defer srv.Shutdown(context.Background())
	if err := writeServiceInfo(cfg.Budget.StateDir, srv.Addr()); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(cfg.Budget.StateDir, serviceFile))
	fmt.Fprintf(stdout, "Maestro is running for %s (worker %q, base branch %s).\n", cfg.Workspace.Repo, cfg.Worker.Name, base)
	if cfg.Tracker.Kind == "linear" {
		fmt.Fprintf(stdout, "It looks at the Linear team %v every %d seconds. To give it a ticket: add the label run:%s\n",
			cfg.Tracker.Provider["team_key"], cfg.Polling.IntervalMS/1000, cfg.Worker.Name)
		fmt.Fprintf(stdout, "and move it to %s. The pull request goes to %s on GitHub.\n", cfg.Tracker.ActiveStates[0], cfg.Workspace.Repo)
		if plans != nil {
			fmt.Fprintf(stdout, "For something bigger, move it to %s instead: an agent asks you questions on the ticket and proposes\n", cfg.Tracker.PlanningState)
			fmt.Fprintf(stdout, "a plan of small tasks. Move it to %s to approve the plan; the tasks become its sub-issues.\n", cfg.Tracker.PlannedState)
		}
	} else if onBoard {
		fmt.Fprintf(stdout, "It looks at the board every %d seconds. To give it a ticket: put an issue of %s on the board,\n", cfg.Polling.IntervalMS/1000, cfg.Workspace.Repo)
		fmt.Fprintf(stdout, "set the card's Worker to %s, and move the card to the column %s.\n", cfg.Worker.Name, cfg.Tracker.ActiveStates[0])
		if plans != nil {
			fmt.Fprintf(stdout, "For something bigger, move the card to %s instead: an agent asks you questions on the issue and proposes\n", cfg.Tracker.PlanningState)
			fmt.Fprintf(stdout, "a plan of small tasks. Move the card to %s to approve the plan.\n", cfg.Tracker.PlannedState)
			if designs != nil {
				fmt.Fprintf(stdout, "To see a design before anything is built, move a card with a plan to %s first.\n", cfg.Tracker.DesignState)
			}
		}
	} else {
		fmt.Fprintf(stdout, "It looks for issues every %d seconds. To give it one, put these labels on an issue:\n", cfg.Polling.IntervalMS/1000)
		fmt.Fprintf(stdout, "  status:todo  %s\n", strings.Join(cfg.Tracker.RequiredLabels, "  "))
	}
	if es, _ := in.skills.List(); len(es) > 0 {
		names := make([]string, len(es))
		for i, e := range es {
			names[i] = e.Name
		}
		fmt.Fprintf(stdout, "The agents have these extra skills and agent files: %s (maestro skills)\n", strings.Join(names, ", "))
	}
	cs.guard(ctx, base, stdout)
	fmt.Fprintf(stdout, "From another terminal: %s, %s, %s.\n", mention("maestro status"), mention("maestro pause"), mention("maestro resume"))
	fmt.Fprintf(stdout, "Status page in a browser on this machine: http://%s/\n", srv.Addr())
	fmt.Fprintln(stdout, "Stop with Ctrl-C. Running agents are stopped; their work so far stays in the workspace.")
	go keepPacksCurrent(ctx, pub, log)
	orch.Run(ctx)
	fmt.Fprintln(stdout, "Stopped.")
	return nil
}

// keepPacksCurrent opens the pull request that brings the repository's
// rule pack files up to this version, shortly after the start and then a
// few times a day, so that an update of Maestro reaches the repository
// without anyone copying files by hand.
func keepPacksCurrent(ctx context.Context, pub *publish.Publisher, log *slog.Logger) {
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		u, err := pub.UpdatePacks(ctx)
		switch {
		case err != nil:
			log.Warn("the rule pack files of the repository could not be brought up to date", "error", err)
		case u.PRNumber > 0:
			log.Info("opened a pull request with the newer rule pack files", "pr", u.PRNumber, "packs", u.Packs, "files", len(u.Files))
		case u.Declined:
			log.Info("the pull request with the newer rule pack files was closed; it is not opened again for the same files")
		}
		if len(u.ByHand) > 0 {
			log.Warn("newer CI workflow files of the rule packs must be copied by hand: maestro conventions apply PACK --replace", "files", u.ByHand)
		}
		t.Reset(6 * time.Hour)
	}
}

// oldProbe says what to do when the sandbox's maestro cannot run the
// self-test.
func (in *install) oldProbe() string {
	if in.dbox == nil {
		return "the maestro in " + launch.DefaultTools + ", which the steps run, is older than this one and cannot run\n" +
			"the sandbox's self-test. Install the current one with: " + mention("maestro setup")
	}
	return "the sandbox image " + in.cfg.Sandbox.Image + " was built by an older Maestro, which cannot run the sandbox's\n" +
		"self-test. Build it again with: " + mention("maestro image")
}

// turnTraces sends each agent turn to the collector the settings name,
// read again for every turn so a reload takes effect.
func turnTraces(cfg func() *config.Config, exp *trace.Exporter) func(worker.Turn) claude.TurnTrace {
	return func(t worker.Turn) claude.TurnTrace {
		c := cfg()
		if c.Traces.URL == "" {
			return nil // not a typed nil: the runner checks for nil
		}
		model := t.Model
		if model == "" {
			model = c.Claude.Model
		}
		return exp.Turn(trace.Endpoint{URL: c.Traces.URL, Project: c.Traces.Project}, t.WorkspaceKey, t.Prompt, model)
	}
}

// startupSelfTest runs the sandbox's self-test before any work (SPEC 11.6).
// A required check that fails stops the start; an advisory one is logged.
func startupSelfTest(ctx context.Context, in *install, log *slog.Logger) error {
	rep, err := in.box.SelfTest(ctx)
	if err != nil {
		return explain("the sandbox's self-test could not run: %s\nSee more with: %s", errText(err), mention("maestro doctor"))
	}
	if rep.OldImage {
		return explain("%s", in.oldProbe())
	}
	if failed := rep.Failed(); len(failed) > 0 {
		var b strings.Builder
		b.WriteString("the sandbox's self-test failed, so the service does not start:\n")
		for _, c := range failed {
			fmt.Fprintf(&b, "  - %s (network %s)", c.Name, c.Profile)
			if c.Detail != "" {
				fmt.Fprintf(&b, ": %s", c.Detail)
			}
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "See more with: %s", mention("maestro doctor"))
		return explain("%s", b.String())
	}
	for _, c := range rep.Warnings() {
		log.Warn("sandbox self-test: "+c.Name, "network", c.Profile, "detail", c.Detail)
	}
	log.Info("sandbox self-test passed", "checks", len(rep.Checks), "warnings", len(rep.Warnings()))
	return nil
}
