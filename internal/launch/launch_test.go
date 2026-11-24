package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/sandbox"
)

// call is one systemd-run the launcher asked for.
type call struct {
	unit string
	args []string
}

type fakeSystemd struct {
	calls []call
	// answer runs instead of the unit: it gets the command after "--" and
	// writes the unit's output.
	answer func(argv []string, stdout io.Writer) int
}

func (f *fakeSystemd) run(_ context.Context, unit string, args []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
	f.calls = append(f.calls, call{unit, args})
	if f.answer == nil {
		return 0, nil
	}
	i := len(args) - 1
	for ; i >= 0 && args[i] != "--"; i-- {
	}
	return f.answer(args[i+1:], stdout), nil
}

func (c call) has(prop string) bool {
	for _, a := range c.args {
		if a == "--property="+prop || a == "--setenv="+prop || a == prop {
			return true
		}
	}
	return false
}

func (c call) hasPrefix(p string) bool {
	for _, a := range c.args {
		if strings.HasPrefix(a, p) {
			return true
		}
	}
	return false
}

func testLauncher(t *testing.T) (*Launcher, *fakeSystemd) {
	t.Helper()
	root := t.TempDir()
	dirs := map[string]string{}
	for _, d := range []string{"ws", "handoff", "mirror", "claude", "state", "proxy"} {
		dirs[d] = filepath.Join(root, d)
		os.MkdirAll(dirs[d], 0o755)
	}
	cfg := &Config{
		Worker: "linux", User: "maestro-agent", ServiceUser: "maestro",
		WorkspaceRoot: dirs["ws"], HandoffRoot: dirs["handoff"], Mirror: dirs["mirror"], ClaudeState: dirs["claude"],
		Hidden:       []string{dirs["state"]},
		ProxySockets: map[string]string{"agent": dirs["proxy"] + "/agent.sock", "hooks": dirs["proxy"] + "/hooks.sock"},
		BotName:      "maestro-bot", BotEmail: "bot@example.com",
		MemoryMax: "4G", CPUQuota: "200%", TasksMax: 512, MaxTimeoutMS: 3_600_000,
	}
	if err := cfg.Check(); err != nil {
		t.Fatal(err)
	}
	f := &fakeSystemd{}
	me := os.Getuid()
	l := &Launcher{Cfg: cfg, Self: "/usr/local/libexec/maestro-launch", SystemdRun: f.run,
		Lookup: func(name string) (int, int, error) { return me, os.Getgid(), nil },
		Chown:  func(string, int, int) error { return nil },
		// The test folders are not root's.
		OwnerCheck: func(string) error { return nil }}
	return l, f
}

func handle(l *Launcher, req sandbox.Request) (int, string, error) {
	var out bytes.Buffer
	code, err := l.Handle(context.Background(), req, nil, &out, io.Discard)
	return code, out.String(), err
}

const sha = "0123456789abcdef0123456789abcdef01234567"

func TestAgentStepUnit(t *testing.T) {
	l, f := testLauncher(t)
	os.Mkdir(l.Cfg.workspace("GH-12"), 0o700)
	code, _, err := handle(l, sandbox.Request{Step: sandbox.StepAgent, WorkspaceKey: "GH-12", NetworkProfile: "agent",
		Argv: []string{"claude", "-p"}, Env: map[string]string{"MAESTRO_TURN": "1"}, Limits: sandbox.Limits{TimeoutMS: 1500}})
	if err != nil || code != 0 || len(f.calls) != 1 {
		t.Fatalf("%d %v %d", code, err, len(f.calls))
	}
	c := f.calls[0]
	if !strings.HasPrefix(c.unit, "maestro-linux-gh-12-agent-") {
		t.Errorf("unit %s", c.unit)
	}
	for _, want := range []string{
		"User=maestro-agent", "NoNewPrivileges=yes", "CapabilityBoundingSet=", "ProtectSystem=strict", "ProtectHome=tmpfs",
		"SystemCallFilter=@system-service", "SystemCallErrorNumber=ENOSYS", "PrivateNetwork=yes", "IPAddressDeny=any", "IPAddressAllow=localhost", "RestrictNamespaces=yes", "UMask=0077", "KillMode=control-group",
		"MemoryMax=4G", "CPUQuota=200%", "TasksMax=512", "RuntimeMaxSec=2",
		"BindPaths=" + l.Cfg.workspace("GH-12") + ":/workspace",
		"BindPaths=" + l.Cfg.ClaudeState + ":/claude",
		"BindReadOnlyPaths=" + l.Cfg.ProxySockets["agent"] + ":" + proxySocket,
		"InaccessiblePaths=-" + l.Cfg.Hidden[0], "InaccessiblePaths=-/run/docker.sock",
		"WorkingDirectory=/workspace/repo",
		"CLAUDE_CONFIG_DIR=/claude", "HTTPS_PROXY=http://127.0.0.1:3128", "MAESTRO_TURN=1",
	} {
		if !c.has(want) {
			t.Errorf("missing %q in\n%s", want, strings.Join(c.args, "\n"))
		}
	}
	tail := strings.Join(c.args[len(c.args)-8:], " ")
	if tail != "-- /usr/local/libexec/maestro-launch inside --proxy "+proxySocket+" -- claude -p" {
		t.Errorf("command %q", tail)
	}
	if c.hasPrefix("--property=BindReadOnlyPaths=" + l.Cfg.Mirror) {
		t.Error("the agent sees the mirror")
	}
	// Claude Code's own sandbox needs namespaces.
	l.Cfg.InnerSandbox = true
	handle(l, sandbox.Request{Step: sandbox.StepAgent, WorkspaceKey: "GH-12", NetworkProfile: "agent", Argv: []string{"claude"}})
	if f.calls[1].has("RestrictNamespaces=yes") {
		t.Error("namespaces restricted with the inner sandbox on")
	}
}

func TestScriptsNeverSeeTheLoginOrTheAgentNetwork(t *testing.T) {
	l, f := testLauncher(t)
	os.Mkdir(l.Cfg.workspace("GH-1"), 0o700)
	for _, step := range []sandbox.Step{sandbox.StepHook, sandbox.StepVerify} {
		if _, _, err := handle(l, sandbox.Request{Step: step, WorkspaceKey: "GH-1", NetworkProfile: "agent", Argv: []string{"sh"}}); !errors.Is(err, ErrRequest) {
			t.Errorf("%s with the agent network: %v", step, err)
		}
		if _, _, err := handle(l, sandbox.Request{Step: step, WorkspaceKey: "GH-1", NetworkProfile: "hooks", Argv: []string{"npm", "ci"}}); err != nil {
			t.Fatal(err)
		}
		c := f.calls[len(f.calls)-1]
		if c.hasPrefix("--property=BindPaths="+l.Cfg.ClaudeState) || c.hasPrefix("--setenv=CLAUDE_CONFIG_DIR") {
			t.Errorf("%s sees the login", step)
		}
		if !c.has("BindReadOnlyPaths=" + l.Cfg.ProxySockets["hooks"] + ":" + proxySocket) {
			t.Errorf("%s has not the hooks proxy", step)
		}
	}
	// Without a profile: no proxy at all.
	handle(l, sandbox.Request{Step: sandbox.StepVerify, WorkspaceKey: "GH-1", Argv: []string{"make", "test"}})
	c := f.calls[len(f.calls)-1]
	if c.hasPrefix("--setenv=HTTPS_PROXY") || c.hasPrefix("--property=BindReadOnlyPaths="+filepath.Dir(l.Cfg.ProxySockets["agent"])) || c.has("--proxy") {
		t.Errorf("a step without network has a proxy:\n%s", strings.Join(c.args, "\n"))
	}
}

func TestRequestsThatAreRefused(t *testing.T) {
	l, f := testLauncher(t)
	os.Mkdir(l.Cfg.workspace("GH-1"), 0o700)
	bad := []sandbox.Request{
		{Step: sandbox.StepVerify, WorkspaceKey: "../etc", Argv: []string{"sh"}},
		{Step: sandbox.StepVerify, WorkspaceKey: "GH-1/x", Argv: []string{"sh"}},
		{Step: sandbox.StepVerify, WorkspaceKey: "GH-2", Argv: []string{"sh"}}, // no workspace
		{Step: sandbox.StepVerify, WorkspaceKey: "GH-1"},
		{Step: sandbox.StepVerify, WorkspaceKey: "GH-1", Argv: []string{"--property=User=root"}},
		{Step: sandbox.StepVerify, WorkspaceKey: "GH-1", Argv: []string{"sh"}, NetworkProfile: "host"},
		{Step: sandbox.StepVerify, WorkspaceKey: "GH-1", Argv: []string{"sh"}, Env: map[string]string{"GITHUB_TOKEN": "x"}},
		{Step: sandbox.StepVerify, WorkspaceKey: "GH-1", Argv: []string{"sh"}, Env: map[string]string{"PATH": "/tmp"}},
		{Step: sandbox.StepVerify, WorkspaceKey: "GH-1", Argv: []string{"sh"}, Env: map[string]string{"X": "a\nb"}},
		{Step: sandbox.StepVerify, WorkspaceKey: "GH-1", Argv: []string{"sh"}, Populate: &sandbox.Populate{Op: "remove"}},
		{Step: sandbox.StepVerify, WorkspaceKey: "GH-1", Argv: []string{"sh"}, Limits: sandbox.Limits{TimeoutMS: -1}},
		{Step: "shell", WorkspaceKey: "GH-1", Argv: []string{"sh"}},
		{Step: sandbox.StepAgent, WorkspaceKey: "GH-1", Argv: []string{"claude"}, NetworkProfile: "hooks"},
		{Step: sandbox.StepExport, WorkspaceKey: "GH-1", Argv: []string{"sh"}},
		{Step: sandbox.StepPopulate, WorkspaceKey: "GH-1", Populate: &sandbox.Populate{Op: "clone", Branch: "a", BaseBranch: "-b"}},
		{Step: sandbox.StepPopulate, WorkspaceKey: "GH-1", Populate: &sandbox.Populate{Op: "clone", Branch: "../x", BaseBranch: "main"}},
		{Step: sandbox.StepPopulate, WorkspaceKey: "GH-1", Populate: &sandbox.Populate{Op: "clone", Branch: "a", BaseBranch: "main"}}, // exists
		{Step: sandbox.StepPopulate, WorkspaceKey: "GH-1", Populate: &sandbox.Populate{Op: "chmod"}},
		{Step: sandbox.StepPopulate, WorkspaceKey: "GH-1", Populate: &sandbox.Populate{Op: "probe"}, Env: map[string]string{"A": "b"}},
	}
	for i, req := range bad {
		if _, _, err := handle(l, req); !errors.Is(err, ErrRequest) {
			t.Errorf("%d: %+v: %v", i, req, err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("a refused request started %d unit(s)", len(f.calls))
	}
	// A longer time limit than the configuration's is cut down.
	handle(l, sandbox.Request{Step: sandbox.StepVerify, WorkspaceKey: "GH-1", Argv: []string{"sh"}, Limits: sandbox.Limits{TimeoutMS: 99_000_000}})
	if !f.calls[0].has("RuntimeMaxSec=3600") {
		t.Error("the time limit was not capped")
	}
}

func TestPopulate(t *testing.T) {
	l, f := testLauncher(t)
	pop := func(op, branch, base string) (int, string, error) {
		return handle(l, sandbox.Request{Step: sandbox.StepPopulate, WorkspaceKey: "GH-7",
			Populate: &sandbox.Populate{Op: op, Branch: branch, BaseBranch: base}})
	}
	if _, out, err := pop("probe", "", ""); err != nil || out != "missing\n" || len(f.calls) != 0 {
		t.Fatalf("probe of nothing: %q %v", out, err)
	}
	f.answer = func(argv []string, w io.Writer) int { io.WriteString(w, sha+"\n"); return 0 }
	if _, out, err := pop("clone", "maestro/GH-7", "main"); err != nil || out != sha+"\n" {
		t.Fatalf("clone: %q %v", out, err)
	}
	c := f.calls[0]
	if !c.has("BindReadOnlyPaths="+l.Cfg.Mirror+":/mirror") || !c.has("WorkingDirectory=/workspace") ||
		!strings.Contains(strings.Join(c.args, " "), "clone maestro/GH-7 main maestro-bot bot@example.com") {
		t.Errorf("clone unit:\n%s", strings.Join(c.args, "\n"))
	}
	if fi, err := os.Stat(l.Cfg.workspace("GH-7")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("workspace folder: %v %v", fi, err)
	}
	if _, out, _ := pop("probe", "", ""); out != "exists "+sha+"\n" {
		t.Errorf("probe: %q", out)
	}
	// What the sandbox prints is only passed on when it is a commit name.
	f.answer = func(argv []string, w io.Writer) int { io.WriteString(w, "exists; rm -rf /\n"); return 0 }
	if _, out, _ := pop("probe", "", ""); out != "missing\n" {
		t.Errorf("a bad probe answer: %q", out)
	}
	last := f.calls[len(f.calls)-1]
	if !strings.Contains(strings.Join(last.args, " "), "/usr/bin/find /workspace -mindepth 1 -delete") {
		t.Errorf("a leftover is emptied inside the sandbox:\n%s", strings.Join(last.args, "\n"))
	}
	if _, err := os.Stat(l.Cfg.workspace("GH-7")); !os.IsNotExist(err) {
		t.Error("the leftover folder is still there")
	}
	// A clone that fails leaves nothing behind.
	f.answer = func(argv []string, w io.Writer) int {
		if strings.Contains(strings.Join(argv, " "), "-delete") {
			return 0
		}
		return 128
	}
	if code, _, _ := pop("clone", "maestro/GH-7", "main"); code != 128 {
		t.Errorf("clone failure: %d", code)
	}
	if _, err := os.Stat(l.Cfg.workspace("GH-7")); !os.IsNotExist(err) {
		t.Error("a failed clone left its folder")
	}
	os.Mkdir(l.Cfg.workspace("GH-7"), 0o700)
	f.answer = nil
	if _, _, err := pop("fast-forward", "maestro/GH-7", "main"); err != nil {
		t.Fatal(err)
	}
	if !f.calls[len(f.calls)-1].has("BindReadOnlyPaths=" + l.Cfg.Mirror + ":/mirror") {
		t.Error("fast-forward without the mirror")
	}
	os.WriteFile(l.Cfg.bundle("GH-7"), []byte("old"), 0o600)
	if _, _, err := pop("remove", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.Cfg.bundle("GH-7")); !os.IsNotExist(err) {
		t.Error("remove left the bundle")
	}
}

func TestExportHandsOverOnlyARegularFile(t *testing.T) {
	l, f := testLauncher(t)
	os.Mkdir(l.Cfg.workspace("GH-3"), 0o700)
	export := func() (int, error) {
		code, _, err := handle(l, sandbox.Request{Step: sandbox.StepExport, WorkspaceKey: "GH-3"})
		return code, err
	}
	f.answer = func(argv []string, w io.Writer) int {
		os.WriteFile(filepath.Join(l.Cfg.handoff("GH-3"), sandbox.BundleName), []byte("bundle"), 0o600)
		io.WriteString(w, sha+" 1\n")
		return 0
	}
	if code, err := export(); err != nil || code != 0 {
		t.Fatalf("%d %v", code, err)
	}
	if !f.calls[0].has("BindPaths=" + l.Cfg.handoff("GH-3") + ":/handoff") {
		t.Error("no hand-off folder in the unit")
	}
	if b, _ := os.ReadFile(l.Cfg.bundle("GH-3")); string(b) != "bundle" {
		t.Errorf("bundle %q", b)
	}
	if _, err := os.Stat(l.Cfg.handoff("GH-3")); !os.IsNotExist(err) {
		t.Error("the hand-off folder is left")
	}
	// A link instead of a file is refused, and the old bundle is gone.
	f.answer = func(argv []string, w io.Writer) int {
		os.Symlink("/etc/shadow", filepath.Join(l.Cfg.handoff("GH-3"), sandbox.BundleName))
		return 0
	}
	if _, err := export(); err == nil {
		t.Error("a link was handed over")
	}
	if _, err := os.Lstat(l.Cfg.bundle("GH-3")); !os.IsNotExist(err) {
		t.Error("something was left where the service looks")
	}
}

func TestReadRequest(t *testing.T) {
	raw, _ := json.Marshal(sandbox.Request{Step: sandbox.StepVerify, WorkspaceKey: "GH-1", Argv: []string{"make"}})
	r := NewReader(strings.NewReader(string(raw) + "\nthe prompt"))
	req, err := ReadRequest(r)
	if err != nil || req.WorkspaceKey != "GH-1" {
		t.Fatalf("%+v %v", req, err)
	}
	if rest, _ := io.ReadAll(r); string(rest) != "the prompt" {
		t.Errorf("stdin %q", rest)
	}
	for _, in := range []string{"", "{", `{"step":"verify","user":"root"}` + "\n", strings.Repeat("x", maxRequest+10)} {
		if _, err := ReadRequest(NewReader(strings.NewReader(in))); !errors.Is(err, ErrRequest) {
			t.Errorf("%.40q: %v", in, err)
		}
	}
}

func TestConfig(t *testing.T) {
	l, _ := testLauncher(t)
	good := *l.Cfg
	for name, bad := range map[string]func(c *Config){
		"root user":       func(c *Config) { c.User = "root" },
		"same users":      func(c *Config) { c.ServiceUser = c.User },
		"relative path":   func(c *Config) { c.Mirror = "mirror" },
		"path with space": func(c *Config) { c.Mirror = "/a b" },
		"path with colon": func(c *Config) { c.HandoffRoot = "/a:/etc" },
		"unclean path":    func(c *Config) { c.WorkspaceRoot = "/var/../etc" },
		"memory":          func(c *Config) { c.MemoryMax = "4G\nUser=root" },
		"profile":         func(c *Config) { c.ProxySockets = map[string]string{"host": "/run/x.sock"} },
		"no timeout":      func(c *Config) { c.MaxTimeoutMS = 0 },
		"worker":          func(c *Config) { c.Worker = "Linux Box" },
	} {
		c := good
		bad(&c)
		if c.Check() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Load reads only a file that only root can change.
	dir := t.TempDir()
	p := filepath.Join(dir, "launch-linux.json")
	raw, _ := json.Marshal(good)
	os.WriteFile(p, raw, 0o644)
	if _, err := Load(p); err == nil && os.Getuid() != 0 {
		t.Error("a file that is not root's was loaded")
	}
	ownerCheck = func(string) error { return nil }
	defer func() { ownerCheck = rootOwned }()
	if c, err := Load(p); err != nil || c.Worker != "linux" {
		t.Errorf("%v", err)
	}
	os.WriteFile(p, []byte(strings.Replace(string(raw), `"worker"`, `"extra":1,"worker"`, 1)), 0o644)
	if _, err := Load(p); err == nil {
		t.Error("an unknown key was accepted")
	}
	if err := rootOwned(dir); err == nil && os.Getuid() != 0 {
		t.Error("a temporary folder counts as root's")
	}
}

func TestRelay(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "p.sock")
	up, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	go func() {
		c, err := up.Accept()
		if err == nil {
			io.Copy(c, c) // echo
			c.Close()
		}
	}()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go relay(ln, sock)
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(c, "CONNECT x:443")
	c.(*net.TCPConn).CloseWrite()
	got, _ := io.ReadAll(c)
	if string(got) != "CONNECT x:443" {
		t.Errorf("relayed %q", got)
	}
}

func TestInsideRunsTheCommand(t *testing.T) {
	if code := Inside([]string{"--", "sh", "-c", "exit 3"}, io.Discard); code != 3 {
		t.Errorf("exit %d", code)
	}
	if code := Inside([]string{"--", "no-such-command-here"}, io.Discard); code != 127 {
		t.Errorf("missing command: %d", code)
	}
	if code := Inside([]string{"--user", "root", "--", "sh"}, io.Discard); code != 125 {
		t.Errorf("unknown flag: %d", code)
	}
}

// The launcher runs as root: it may use the standard library and package
// sandbox only.
func TestOnlyTheStandardLibrary(t *testing.T) {
	for _, dir := range []string{".", "../sandbox", "../../cmd/maestro-launch"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			pf, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, im := range pf.Imports {
				p, _ := strconv.Unquote(im.Path.Value)
				first := strings.SplitN(p, "/", 2)[0]
				if strings.Contains(first, ".") && p != "github.com/iandeherdt/Maestro/internal/sandbox" &&
					p != "github.com/iandeherdt/Maestro/internal/launch" {
					t.Errorf("%s imports %s", f, p)
				}
			}
		}
	}
}

func TestLoginAndItsStatusHaveNoWorkspace(t *testing.T) {
	l, f := testLauncher(t)
	if _, err := l.Login(context.Background(), strings.NewReader(""), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, _, err := handle(l, sandbox.Request{Step: sandbox.StepLoginStatus, NetworkProfile: "agent"}); err != nil {
		t.Fatal(err)
	}
	login, status := f.calls[0], f.calls[1]
	for _, c := range []call{login, status} {
		if c.hasPrefix("--property=BindPaths=" + l.Cfg.WorkspaceRoot) {
			t.Errorf("a workspace is bound:\n%s", strings.Join(c.args, "\n"))
		}
		if !c.has("BindPaths="+l.Cfg.ClaudeState+":/claude") || !c.has("PrivateNetwork=yes") ||
			!c.has("BindReadOnlyPaths="+l.Cfg.ProxySockets["agent"]+":"+proxySocket) || !c.has("WorkingDirectory=/tmp") ||
			!c.has("PATH="+DefaultTools+":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin") {
			t.Errorf("unit:\n%s", strings.Join(c.args, "\n"))
		}
	}
	if !login.has("--pty") || login.has("--pipe") || !login.has("TERM=xterm-256color") || login.hasPrefix("--setenv=CI=") {
		t.Errorf("login unit:\n%s", strings.Join(login.args, "\n"))
	}
	if got := strings.Join(login.args[len(login.args)-4:], " "); got != DefaultTools+"/claude auth login --claudeai" {
		t.Errorf("login command %q", got)
	}
	if got := strings.Join(status.args[len(status.args)-4:], " "); got != DefaultTools+"/claude auth status --json" || !status.has("--pipe") {
		t.Errorf("status command %q", got)
	}
	if !strings.HasPrefix(login.unit, "maestro-linux-login-") || !strings.HasPrefix(status.unit, "maestro-linux-login-status-") {
		t.Errorf("units %s %s", login.unit, status.unit)
	}
	// The caller chooses nothing of these.
	for _, req := range []sandbox.Request{
		{Step: sandbox.StepLoginStatus, NetworkProfile: "agent", Argv: []string{"sh"}},
		{Step: sandbox.StepLoginStatus, NetworkProfile: "agent", WorkspaceKey: "GH-1"},
		{Step: sandbox.StepLoginStatus, NetworkProfile: "hooks"},
		{Step: sandbox.StepSelfTest, NetworkProfile: "none", Env: map[string]string{"A": "b"}},
		{Step: sandbox.StepSelfTest, NetworkProfile: "elsewhere"},
	} {
		if _, _, err := handle(l, req); !errors.Is(err, ErrRequest) {
			t.Errorf("%+v: %v", req, err)
		}
	}
}

func TestSelfTestRunsTheProbeInAThrowawayWorkspace(t *testing.T) {
	l, f := testLauncher(t)
	f.answer = func(argv []string, w io.Writer) int {
		if argv[0] == DefaultTools+"/maestro" {
			io.WriteString(w, `{"name":"runs as an ordinary user","ok":true}`+"\n")
		}
		return 0
	}
	_, out, err := handle(l, sandbox.Request{Step: sandbox.StepSelfTest, NetworkProfile: "hooks"})
	if err != nil || !strings.Contains(out, "ordinary user") {
		t.Fatalf("%q %v", out, err)
	}
	probe := f.calls[0]
	want := DefaultTools + "/maestro probe --profile hooks --canary " + sandbox.CanaryName + " --deny " + sandbox.ProbeDenyHost +
		" --pids 512 --memory " + strconv.Itoa(4<<30)
	if got := strings.Join(probe.args[len(probe.args)-12:], " "); got != want {
		t.Errorf("probe %q", got)
	}
	if !probe.hasPrefix("--property=BindPaths="+filepath.Join(l.Cfg.WorkspaceRoot, "selftest-")) ||
		!probe.has("BindReadOnlyPaths="+l.Cfg.ProxySockets["hooks"]+":"+proxySocket) || probe.hasPrefix("--property=BindPaths="+l.Cfg.ClaudeState) {
		t.Errorf("probe unit:\n%s", strings.Join(probe.args, "\n"))
	}
	if len(f.calls) != 2 || !strings.Contains(strings.Join(f.calls[1].args, " "), "-delete") {
		t.Errorf("the workspace is not emptied: %d calls", len(f.calls))
	}
	if left, _ := os.ReadDir(l.Cfg.WorkspaceRoot); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
	if n, ok := memoryBytes("512M"); !ok || n != 512<<20 {
		t.Errorf("memoryBytes %d", n)
	}
}

// On RHEL 8's systemd 239, properties it does not know are left out, or
// systemd-run refuses the unit; what covers them is added instead.
func TestUnitOnAnOlderSystemd(t *testing.T) {
	for _, tc := range []struct {
		systemd     int
		has, hasNot []string
		serviceType string
	}{
		{239, []string{"ProtectKernelTunables=yes", "SystemCallFilter=~syslog", "NoNewPrivileges=yes", "SystemCallErrorNumber=ENOSYS"},
			[]string{"ProtectKernelLogs=yes", "ProtectClock=yes", "ProtectHostname=yes", "RestrictSUIDSGID=yes", "OOMPolicy=kill", "LockPersonality=yes",
				"InaccessiblePaths=-/proc/kmsg"}, "simple"},
		{252, []string{"ProtectKernelLogs=yes", "ProtectClock=yes", "ProtectHostname=yes", "RestrictSUIDSGID=yes", "OOMPolicy=kill", "LockPersonality=yes"},
			[]string{"SystemCallFilter=~syslog"}, "exec"},
		{0, []string{"ProtectKernelLogs=yes", "OOMPolicy=kill"}, nil, "exec"},
	} {
		l, f := testLauncher(t)
		l.Systemd = tc.systemd
		os.Mkdir(l.Cfg.workspace("GH-12"), 0o700)
		if _, _, err := handle(l, sandbox.Request{Step: sandbox.StepAgent, WorkspaceKey: "GH-12", NetworkProfile: "agent",
			Argv: []string{"claude", "-p"}}); err != nil || len(f.calls) != 1 {
			t.Fatalf("systemd %d: %v", tc.systemd, err)
		}
		c := f.calls[0]
		for _, want := range tc.has {
			if !c.has(want) {
				t.Errorf("systemd %d: missing %q", tc.systemd, want)
			}
		}
		for _, not := range tc.hasNot {
			if c.has(not) {
				t.Errorf("systemd %d: has %q", tc.systemd, not)
			}
		}
		if !slices.Contains(c.args, "--service-type="+tc.serviceType) {
			t.Errorf("systemd %d: not --service-type=%s: %q", tc.systemd, tc.serviceType, c.args)
		}
	}
}

// The launcher removes and creates folders in the hand-off folder as root.
// In a folder another user can change, a link in its place would send that
// elsewhere, so such a folder is refused before anything is touched.
func TestExportRefusesAHandoffFolderThatIsNotRoots(t *testing.T) {
	l, f := testLauncher(t)
	l.OwnerCheck = nil    // the real check
	system := t.TempDir() // stands for a folder only root may change
	victim := filepath.Join(system, "profile.d")
	os.Mkdir(victim, 0o755)
	os.WriteFile(filepath.Join(victim, "keep.sh"), []byte("keep\n"), 0o644)
	os.RemoveAll(l.Cfg.HandoffRoot)
	if err := os.Symlink(system, l.Cfg.HandoffRoot); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(l.Cfg.workspace("profile.d"), 0o700)
	_, _, err := handle(l, sandbox.Request{Step: sandbox.StepExport, WorkspaceKey: "profile.d"})
	if err == nil || !strings.Contains(err.Error(), "maestro setup") {
		t.Fatalf("err = %v", err)
	}
	if len(f.calls) != 0 {
		t.Error("a unit was started")
	}
	if _, err := os.Stat(filepath.Join(victim, "keep.sh")); err != nil {
		t.Errorf("the folder behind the link was touched: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(system, "profile.d.bundle")); err == nil {
		t.Error("a bundle was left behind the link")
	}
}

func TestTheStepsGitConfigLetsThemCloneTheMirror(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox.gitconfig")
	if err := writeGitConfig(path); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{mirrorDir, repoDir} {
		cmd := exec.Command("git", "config", "--system", "--get-all", "safe.directory")
		cmd.Env = append(os.Environ(), "GIT_CONFIG_SYSTEM="+path)
		out, err := cmd.Output()
		if err != nil || !strings.Contains(string(out), dir+"\n") {
			t.Errorf("safe.directory %s: %q %v", dir, out, err)
		}
	}
	// A second run leaves a correct file alone.
	if err := writeGitConfig(path); err != nil {
		t.Fatal(err)
	}
	l, f := testLauncher(t)
	os.Mkdir(l.Cfg.workspace("GH-12"), 0o700)
	handle(l, sandbox.Request{Step: sandbox.StepAgent, WorkspaceKey: "GH-12", NetworkProfile: "agent", Argv: []string{"claude"}})
	if len(f.calls) != 1 || !f.calls[0].has("GIT_CONFIG_SYSTEM="+GitConfigPath) {
		t.Errorf("the step's environment has no GIT_CONFIG_SYSTEM: %v", f.calls)
	}
}
