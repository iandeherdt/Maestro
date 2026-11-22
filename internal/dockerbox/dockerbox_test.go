package dockerbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/sandbox"
)

// These tests run real containers. They are skipped where there is no
// Docker daemon.

const testDockerfile = `FROM docker.io/alpine/git:latest
RUN addgroup -g 10001 agent \
 && adduser -D -u 10001 -G agent agent \
 && mkdir -p /workspace /claude /handoff \
 && chown agent:agent /workspace /claude /handoff \
 && git config --system --add safe.directory /mirror \
 && git config --system --add safe.directory /workspace/repo
COPY maestro /usr/local/bin/maestro
`

// engine is the container command the tests run against: docker, or podman
// with MAESTRO_TEST_ENGINE=podman.
var engine = func() string {
	if e := os.Getenv("MAESTRO_TEST_ENGINE"); e != "" {
		return e
	}
	return "docker"
}()

var (
	ctx      = context.Background()
	shared   *Box
	sharedOK bool
	stateDir string
)

func TestMain(m *testing.M) {
	code := func() int {
		if exec.Command(engine, "info").Run() != nil {
			fmt.Println("no Docker daemon: container tests are skipped")
			return m.Run()
		}
		dir, err := os.MkdirTemp("", "dockerbox-test-")
		if err != nil {
			panic(err)
		}
		defer os.RemoveAll(dir)
		stateDir = dir
		bin := filepath.Join(dir, "maestro")
		build := exec.Command("go", "build", "-o", bin, "../../cmd/maestro")
		build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
		if out, err := build.CombinedOutput(); err != nil {
			panic(string(out))
		}
		shared = &Box{Docker: engine, Podman: engine == "podman", Worker: "t" + token()[:6], Image: "maestro-test-sandbox:latest",
			MirrorDir: filepath.Join(dir, "mirror.git"), HandoffDir: filepath.Join(dir, "handoff"),
			Memory: "512m", CPUs: "1.0", PIDs: 128, BotName: "sym[bot]", BotEmail: "sym@example.com"}
		var out bytes.Buffer
		if err := shared.BuildImage(ctx, Build{Dockerfile: testDockerfile, Binary: bin, Out: &out}); err != nil {
			panic(err.Error() + "\n" + out.String())
		}
		seed(dir)
		allow := filepath.Join(dir, "proxy")
		for _, p := range Profiles {
			if err := WriteAllow(allow, p, []string{"example.com"}); err != nil {
				panic(err)
			}
		}
		if err := shared.Up(ctx, allow); err != nil {
			panic(err)
		}
		defer shared.Purge(ctx)
		sharedOK = true
		return m.Run()
	}()
	os.Exit(code)
}

func git(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=o", "GIT_AUTHOR_EMAIL=o@example.com", "GIT_COMMITTER_NAME=o", "GIT_COMMITTER_EMAIL=o@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("git %v: %v\n%s", args, err, out))
	}
	return strings.TrimSpace(string(out))
}

var baseSHA string

// seed makes the mirror: a bare repository with one commit on main.
func seed(dir string) {
	src := filepath.Join(dir, "src")
	os.MkdirAll(src, 0o755)
	git(src, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(src, "README.md"), []byte("hello\n"), 0o644)
	git(src, "add", "-A")
	git(src, "commit", "-q", "-m", "base")
	baseSHA = git(src, "rev-parse", "HEAD")
	git(dir, "clone", "-q", "--bare", src, filepath.Join(dir, "mirror.git"))
	os.MkdirAll(filepath.Join(dir, "handoff"), 0o755)
}

func box(t *testing.T) *Box {
	t.Helper()
	if !sharedOK {
		t.Skip("no Docker daemon")
	}
	return shared
}

// workspace clones a fresh workspace and removes it when the test ends.
func workspace(t *testing.T, b *Box) string {
	t.Helper()
	key := "GH-" + token()[:8]
	res, err := b.Run(ctx, sandbox.Request{Step: sandbox.StepPopulate, WorkspaceKey: key,
		Populate: &sandbox.Populate{Op: sandbox.OpClone, Branch: "maestro/" + key, BaseBranch: "main"}}, nil)
	if err != nil || res.ExitStatus != 0 {
		t.Fatalf("clone: %v, status %d, %s", err, res.ExitStatus, res.Stderr)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != baseSHA {
		t.Fatalf("clone reported %q, want %s", got, baseSHA)
	}
	t.Cleanup(func() {
		b.Run(ctx, sandbox.Request{Step: sandbox.StepPopulate, WorkspaceKey: key, Populate: &sandbox.Populate{Op: sandbox.OpRemove}}, nil)
	})
	return key
}

// sh runs a shell command as a hook step and returns its output.
func sh(t *testing.T, b *Box, key, network, script string) (string, int) {
	t.Helper()
	res, err := b.Run(ctx, sandbox.Request{Step: sandbox.StepHook, WorkspaceKey: key, NetworkProfile: network,
		Argv: []string{"sh", "-c", script}, Limits: sandbox.Limits{TimeoutMS: 60000}}, nil)
	if err != nil {
		t.Fatalf("%q: %v", script, err)
	}
	return strings.TrimSpace(string(res.Stdout) + string(res.Stderr)), res.ExitStatus
}

func populate(b *Box, key string, p sandbox.Populate) (sandbox.Result, error) {
	return b.Run(ctx, sandbox.Request{Step: sandbox.StepPopulate, WorkspaceKey: key, Populate: &p}, nil)
}

func TestWorkspaceLifecycle(t *testing.T) {
	b := box(t)
	key := "GH-" + token()[:8]
	if res, err := populate(b, key, sandbox.Populate{Op: sandbox.OpProbe}); err != nil || strings.TrimSpace(string(res.Stdout)) != "missing" {
		t.Fatalf("probe before: %q, %v", res.Stdout, err)
	}
	key = workspace(t, b)
	if res, err := populate(b, key, sandbox.Populate{Op: sandbox.OpProbe}); err != nil || strings.TrimSpace(string(res.Stdout)) != "exists "+baseSHA {
		t.Fatalf("probe after: %q, %v", res.Stdout, err)
	}
	out, _ := sh(t, b, key, sandbox.NetNone, `git branch --show-current; git config user.name; git config user.email; git remote get-url origin; pwd; id -u`)
	if out != "maestro/"+key+"\nsym[bot]\nsym@example.com\n/mirror\n/workspace/repo\n10001" {
		t.Fatalf("workspace state:\n%s", out)
	}
	// A second clone of the same workspace is refused.
	if _, err := populate(b, key, sandbox.Populate{Op: sandbox.OpClone, Branch: "maestro/x", BaseBranch: "main"}); err == nil {
		t.Fatal("an existing workspace was cloned over")
	}
	// A failed clone leaves nothing behind.
	bad := "GH-" + token()[:8]
	if res, err := populate(b, bad, sandbox.Populate{Op: sandbox.OpClone, Branch: "maestro/" + bad, BaseBranch: "no-such-branch"}); err == nil && res.ExitStatus == 0 {
		t.Fatal("a clone of a missing branch succeeded")
	}
	if res, _ := populate(b, bad, sandbox.Populate{Op: sandbox.OpProbe}); strings.TrimSpace(string(res.Stdout)) != "missing" {
		t.Fatalf("leftover after a failed clone: %q", res.Stdout)
	}
	// Removal, also of something that is not there.
	for i := 0; i < 2; i++ {
		if _, err := populate(b, key, sandbox.Populate{Op: sandbox.OpRemove}); err != nil {
			t.Fatal(err)
		}
	}
	if res, _ := populate(b, key, sandbox.Populate{Op: sandbox.OpProbe}); strings.TrimSpace(string(res.Stdout)) != "missing" {
		t.Fatalf("probe after removal: %q", res.Stdout)
	}
	// A step in a workspace that does not exist does not create it.
	if _, err := b.Run(ctx, sandbox.Request{Step: sandbox.StepHook, WorkspaceKey: key, Argv: []string{"true"}}, nil); err == nil {
		t.Fatal("a step ran in a missing workspace")
	}
	if ok, _ := b.volumeExists(ctx, b.Volume(key)); ok {
		t.Fatal("the step created a volume")
	}
}

func TestExportProducesABundleTheMirrorCanRead(t *testing.T) {
	b := box(t)
	key := workspace(t, b)
	export := func() string {
		res, err := b.Run(ctx, sandbox.Request{Step: sandbox.StepExport, WorkspaceKey: key}, nil)
		if err != nil || res.ExitStatus != 0 {
			t.Fatalf("export: %v, status %d, %s", err, res.ExitStatus, res.Stderr)
		}
		return strings.TrimSpace(string(res.Stdout))
	}
	final := filepath.Join(b.HandoffDir, key+".bundle")
	if got := export(); got != baseSHA+" 0" {
		t.Fatalf("clean export: %q", got)
	}
	if _, err := os.Stat(final); err == nil {
		t.Fatal("a bundle exists for a workspace without changes")
	}
	// One committed change and one uncommitted change.
	sh(t, b, key, sandbox.NetNone, `echo a > a.txt && git add a.txt && git commit -q -m a && echo b > b.txt`)
	fields := strings.Fields(export())
	if len(fields) != 2 || fields[1] != "2" || !shaRE.MatchString(fields[0]) {
		t.Fatalf("export: %v", fields)
	}
	heads := git(b.MirrorDir, "bundle", "list-heads", final)
	if !strings.HasPrefix(heads, fields[0]) {
		t.Fatalf("bundle heads %q, exported head %s", heads, fields[0])
	}
	if out, _ := sh(t, b, key, sandbox.NetNone, `git status --porcelain; git log -1 --format=%s`); out != "maestro: uncommitted changes at end of turn" {
		t.Fatalf("after export: %q", out)
	}
	// Exporting again without changes keeps the count and rewrites the bundle.
	if got := export(); got != fields[0]+" 2" {
		t.Fatalf("second export: %q", got)
	}
	// A hook planted by the agent does not run during export.
	sh(t, b, key, sandbox.NetNone, `printf '#!/bin/sh\ntouch /workspace/hook-ran\n' > .git/hooks/pre-commit && chmod +x .git/hooks/pre-commit && echo c > c.txt`)
	export()
	if out, _ := sh(t, b, key, sandbox.NetNone, `ls /workspace`); strings.Contains(out, "hook-ran") {
		t.Fatal("a repository hook ran during export")
	}
}

func TestFastForward(t *testing.T) {
	b := box(t)
	key := workspace(t, b)
	branch := "maestro/" + key
	ff := func() {
		t.Helper()
		if res, err := populate(b, key, sandbox.Populate{Op: sandbox.OpFastForward, Branch: branch}); err != nil || res.ExitStatus != 0 {
			t.Fatalf("fast-forward: %v, status %d, %s", err, res.ExitStatus, res.Stderr)
		}
	}
	// The branch is not in the mirror yet: nothing happens.
	ff()
	// A commit published by an earlier process appears in the mirror.
	src := filepath.Join(stateDir, "src")
	git(src, "checkout", "-q", "-b", branch, baseSHA)
	os.WriteFile(filepath.Join(src, "published.txt"), []byte("x\n"), 0o644)
	git(src, "add", "-A")
	git(src, "commit", "-q", "-m", "published")
	ahead := git(src, "rev-parse", "HEAD")
	git(src, "push", "-q", b.MirrorDir, branch)
	defer git(b.MirrorDir, "update-ref", "-d", "refs/heads/"+branch)
	ff()
	if out, _ := sh(t, b, key, sandbox.NetNone, `git rev-parse HEAD`); out != ahead {
		t.Fatalf("head %s, want %s", out, ahead)
	}
	// The workspace moves on; the mirror's older head does not pull it back.
	sh(t, b, key, sandbox.NetNone, `echo more > more.txt && git add -A && git commit -q -m more`)
	local, _ := sh(t, b, key, sandbox.NetNone, `git rev-parse HEAD`)
	ff()
	if out, _ := sh(t, b, key, sandbox.NetNone, `git rev-parse HEAD`); out != local {
		t.Fatal("the workspace was moved back")
	}
}

func TestIsolation(t *testing.T) {
	b := box(t)
	key := workspace(t, b)
	other := workspace(t, b)
	secret := filepath.Join(stateDir, "secret.txt")
	os.WriteFile(secret, []byte("control zone"), 0o644)
	sh(t, b, other, sandbox.NetNone, `echo other > /workspace/repo/other.txt`)

	for name, script := range map[string]string{
		"the mirror is not mounted outside populate": `test ! -e /mirror`,
		"the login is not mounted in a hook":         `! grep -q " /claude " /proc/mounts && test -z "$(ls -A /claude)"`,
		"the hand-off directory is not mounted":      `test -z "$(ls -A /handoff)"`,
		"the other workspace is not visible":         `test ! -e /workspace/repo/other.txt && ! ls / | grep -q GH-`,
		"the host's files are not visible":           `test ! -e ` + secret + ` && test ! -e ` + stateDir,
		"no docker socket":                           `test ! -e /var/run/docker.sock`,
		"not root":                                   `test "$(id -u)" = 10001`,
		"no capabilities":                            `grep -q '^CapEff:\s*0000000000000000$' /proc/self/status && grep -q '^CapBnd:\s*0000000000000000$' /proc/self/status`,
		"no new privileges":                          `grep -q '^NoNewPrivs:\s*1$' /proc/self/status`,
		"the service's environment is not inherited": `test -z "${GH_TOKEN:-}${HTTPS_PROXY:-}${USER:-}" && test "$HOME" = /home/agent && test "$CI" = 1`,
		"no network interface":                       `! ls /sys/class/net | grep -qv '^lo$'`,
		"no name resolution":                         `! nslookup example.com >/dev/null 2>&1`,
		"no direct connection":                       `! nc -w 3 1.1.1.1 443 </dev/null >/dev/null 2>&1`,
	} {
		if out, code := sh(t, b, key, sandbox.NetNone, script); code != 0 {
			t.Errorf("%s: failed (status %d) %s", name, code, out)
		}
	}
}

func TestNetworkGoesThroughTheProxyOnly(t *testing.T) {
	b := box(t)
	key := workspace(t, b)
	proxy := b.ProxyName(sandbox.NetHooks)
	connect := func(target string) string {
		return `printf 'CONNECT ` + target + ` HTTP/1.1\r\nHost: ` + target + `\r\n\r\n' | nc -w 10 ` + proxy + ` 3128 | head -1`
	}
	for name, tc := range map[string]struct{ script, want string }{
		"the proxy is set in the environment": {`echo "$HTTPS_PROXY"`, "http://" + proxy + ":3128"},
		"a host that is not allowed":          {connect("github.com:443"), " 403 "},
		"an allowed host on another port":     {connect("example.com:22"), " 403 "},
		"a LAN address":                       {connect("192.168.1.1:443"), " 403 "},
		"plain HTTP through the proxy":        {`printf 'GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n' | nc -w 10 ` + proxy + ` 3128 | head -1`, " 405 "},
		"no direct connection":                {`nc -w 3 1.1.1.1 443 </dev/null >/dev/null 2>&1 && echo reached || echo blocked`, "blocked"},
		"no direct name resolution":           {`nslookup example.com >/dev/null 2>&1 && echo resolved || echo blocked`, "blocked"},
		"the host is not reachable":           {`nc -w 3 host.docker.internal 22 </dev/null >/dev/null 2>&1 && echo reached || echo blocked`, "blocked"},
	} {
		if out, _ := sh(t, b, key, sandbox.NetHooks, tc.script); !strings.Contains(out, tc.want) {
			t.Errorf("%s: %q, want %q", name, out, tc.want)
		}
	}
	// The allowlist is read per request.
	allow := filepath.Join(stateDir, "proxy")
	if err := WriteAllow(allow, sandbox.NetHooks, []string{"example.com", "github.com"}); err != nil {
		t.Fatal(err)
	}
	defer WriteAllow(allow, sandbox.NetHooks, []string{"example.com"})
	if out, _ := sh(t, b, key, sandbox.NetHooks, connect("github.com:443")); strings.Contains(out, " 403 ") {
		t.Errorf("after the allowlist changed: %q", out)
	}
	// With internet access, an allowed host is tunnelled. Without it (a
	// build machine behind its own proxy), the proxy still answers.
	out, _ := sh(t, b, key, sandbox.NetHooks, connect("example.com:443"))
	if os.Getenv("MAESTRO_TEST_INTERNET") != "" && !strings.Contains(out, " 200 ") {
		t.Errorf("an allowed host was not tunnelled: %q", out)
	}
	if !strings.Contains(out, " 200 ") && !strings.Contains(out, " 502 ") {
		t.Errorf("an allowed host: %q", out)
	}
}

func TestAgentStepGetsTheLoginAndItsOwnNetwork(t *testing.T) {
	b := box(t)
	key := workspace(t, b)
	var out bytes.Buffer
	res, err := b.Stream(ctx, sandbox.Request{Step: sandbox.StepAgent, WorkspaceKey: key, NetworkProfile: sandbox.NetAgent,
		Env:  map[string]string{"NODE_ENV": "test"},
		Argv: []string{"sh", "-c", `cat; echo "|$CLAUDE_CONFIG_DIR|$NODE_ENV|$HTTPS_PROXY"; touch /claude/login && echo saved; echo problem >&2; exit 3`}},
		[]byte("the prompt"), &out)
	if err != nil || res.ExitStatus != 3 || len(res.Stdout) != 0 || strings.TrimSpace(string(res.Stderr)) != "problem" {
		t.Fatalf("%v, %+v", err, res)
	}
	want := "the prompt|/claude|test|http://" + b.ProxyName(sandbox.NetAgent) + ":3128\nsaved\n"
	if out.String() != want {
		t.Fatalf("stream:\n%q\nwant\n%q", out.String(), want)
	}
	// The login persists for the next agent step and is hidden from verify.
	if o, _ := sh(t, b, key, sandbox.NetNone, `ls /claude`); o != "" {
		t.Fatalf("a hook sees the login: %q", o)
	}
	res, err = b.Run(ctx, sandbox.Request{Step: sandbox.StepAgent, WorkspaceKey: key, NetworkProfile: sandbox.NetAgent, Argv: []string{"ls", "/claude"}}, nil)
	if err != nil || strings.TrimSpace(string(res.Stdout)) != "login" {
		t.Fatalf("%v, %q", err, res.Stdout)
	}
}

// The owner's skills and agent files are where Claude Code looks for them,
// the agent cannot change them, and no other step sees them.
func TestAgentStepGetsTheSkillsReadOnly(t *testing.T) {
	shared := box(t)
	b := *shared
	dir := t.TempDir()
	os.Chmod(dir, 0o755)
	b.SkillsDir, b.AgentsDir = filepath.Join(dir, "skills"), filepath.Join(dir, "agents")
	os.MkdirAll(filepath.Join(b.SkillsDir, "house-style"), 0o755)
	os.MkdirAll(b.AgentsDir, 0o755)
	os.WriteFile(filepath.Join(b.SkillsDir, "house-style", "SKILL.md"), []byte("use tabs\n"), 0o644)
	os.WriteFile(filepath.Join(b.AgentsDir, "reviewer.md"), []byte("review\n"), 0o644)
	key := workspace(t, &b)
	res, err := b.Run(ctx, sandbox.Request{Step: sandbox.StepAgent, WorkspaceKey: key, NetworkProfile: sandbox.NetAgent,
		Argv: []string{"sh", "-c", `cat /claude/skills/house-style/SKILL.md /claude/agents/reviewer.md
echo x > /claude/skills/planted.md 2>/dev/null && echo planted
echo x >> /claude/skills/house-style/SKILL.md 2>/dev/null && echo changed
mkdir /claude/agents/mine 2>/dev/null && echo made
echo ok > /claude/login-check && echo volume-writable`}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Stdout); got != "use tabs\nreview\nvolume-writable\n" {
		t.Fatalf("stdout %q, stderr %q", got, res.Stderr)
	}
	if raw, _ := os.ReadFile(filepath.Join(b.SkillsDir, "house-style", "SKILL.md")); string(raw) != "use tabs\n" {
		t.Errorf("the skill on the machine was changed: %q", raw)
	}
	if o, _ := sh(t, &b, key, sandbox.NetNone, `ls /claude 2>/dev/null`); o != "" {
		t.Errorf("a hook sees the skills: %q", o)
	}
}

func TestUnityLicenceReachesHooksAndVerifyButNotTheAgent(t *testing.T) {
	shared := box(t)
	b := *shared
	dir := t.TempDir()
	os.Chmod(dir, 0o755)
	b.UnityLicenceDir = filepath.Join(dir, "unity-licence")
	os.MkdirAll(filepath.Join(b.UnityLicenceDir, ".local", "share", "unity3d", "Unity"), 0o755)
	lic := filepath.Join(b.UnityLicenceDir, ".local", "share", "unity3d", "Unity", "Unity_lic.ulf")
	os.WriteFile(lic, []byte("licence\n"), 0o644)
	key := workspace(t, &b)
	read := `exec 2>/dev/null
cat /unity-licence/.local/share/unity3d/Unity/Unity_lic.ulf 2>/dev/null || echo none
echo x > /unity-licence/planted 2>/dev/null && echo planted
echo x >> /unity-licence/.local/share/unity3d/Unity/Unity_lic.ulf 2>/dev/null && echo changed
true`
	if o, _ := sh(t, &b, key, sandbox.NetNone, read); o != "licence" {
		t.Errorf("hook: %q", o)
	}
	res, err := b.Run(ctx, sandbox.Request{Step: sandbox.StepVerify, WorkspaceKey: key,
		Argv: []string{"sh", "-c", read}}, nil)
	if err != nil || strings.TrimSpace(string(res.Stdout)) != "licence" {
		t.Errorf("verify: %v, stdout %q, stderr %q", err, res.Stdout, res.Stderr)
	}
	res, err = b.Run(ctx, sandbox.Request{Step: sandbox.StepAgent, WorkspaceKey: key, NetworkProfile: sandbox.NetAgent,
		Argv: []string{"sh", "-c", `ls -A /unity-licence 2>/dev/null; echo end`}}, nil)
	if err != nil || string(res.Stdout) != "end\n" {
		t.Errorf("the agent step sees the licence: %v, stdout %q", err, res.Stdout)
	}
	if raw, _ := os.ReadFile(lic); string(raw) != "licence\n" {
		t.Errorf("the licence on the machine was changed: %q", raw)
	}
}

func TestStepIsKilledWhenItsTimeIsUp(t *testing.T) {
	b := box(t)
	key := workspace(t, b)
	start := time.Now()
	_, err := b.Run(ctx, sandbox.Request{Step: sandbox.StepVerify, WorkspaceKey: key,
		Argv: []string{"sh", "-c", "sleep 300 & sleep 300"}, Limits: sandbox.Limits{TimeoutMS: 1500}}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 40*time.Second {
		t.Fatalf("%v after %s", err, time.Since(start))
	}
	// Nothing of the step is left running.
	time.Sleep(500 * time.Millisecond)
	out, _ := b.cli(ctx, "ps", "--quiet", "--filter", "label=maestro.step=verify", "--filter", "label=maestro.worker="+b.Worker)
	if out != "" {
		t.Fatalf("containers left: %s", out)
	}
	// Cancelling from outside does the same.
	c, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(time.Second); cancel() }()
	if _, err := b.Run(c, sandbox.Request{Step: sandbox.StepHook, WorkspaceKey: key, Argv: []string{"sleep", "300"}}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
}

func TestLimits(t *testing.T) {
	b := box(t)
	key := workspace(t, b)
	// More processes than the limit allows.
	out, _ := sh(t, b, key, sandbox.NetNone, `i=0; while [ $i -lt 400 ]; do sleep 30 & i=$((i+1)); done 2>&1 | grep -ci "can't fork\|resource" || true`)
	if out == "0" || out == "" {
		t.Errorf("400 processes were started under a limit of %d: %q", b.PIDs, out)
	}
	// The memory limit the kernel enforces on the container.
	out, _ = sh(t, b, key, sandbox.NetNone, `cat /sys/fs/cgroup/memory.max 2>/dev/null || cat /sys/fs/cgroup/memory/memory.limit_in_bytes`)
	if strings.TrimSpace(out) != "536870912" {
		t.Errorf("memory limit = %q, want 512 MB", out)
	}
}

func TestRequestsThatAreRefused(t *testing.T) {
	b := box(t)
	key := workspace(t, b)
	argv := []string{"true"}
	for name, req := range map[string]sandbox.Request{
		"key with a slash":              {Step: sandbox.StepHook, WorkspaceKey: "../x", Argv: argv},
		"key with dots":                 {Step: sandbox.StepHook, WorkspaceKey: "a..b", Argv: argv},
		"empty key":                     {Step: sandbox.StepHook, Argv: argv},
		"unknown step":                  {Step: "shell", WorkspaceKey: key, Argv: argv},
		"unknown network":               {Step: sandbox.StepHook, WorkspaceKey: key, Argv: argv, NetworkProfile: "host"},
		"hook on the agent network":     {Step: sandbox.StepHook, WorkspaceKey: key, Argv: argv, NetworkProfile: sandbox.NetAgent},
		"verify on the agent network":   {Step: sandbox.StepVerify, WorkspaceKey: key, Argv: argv, NetworkProfile: sandbox.NetAgent},
		"agent without its network":     {Step: sandbox.StepAgent, WorkspaceKey: key, Argv: argv},
		"no command":                    {Step: sandbox.StepHook, WorkspaceKey: key},
		"command that is a docker flag": {Step: sandbox.StepHook, WorkspaceKey: key, Argv: []string{"--privileged"}},
		"export with a command":         {Step: sandbox.StepExport, WorkspaceKey: key, Argv: argv},
		"populate with a command":       {Step: sandbox.StepPopulate, WorkspaceKey: key, Argv: argv, Populate: &sandbox.Populate{Op: sandbox.OpProbe}},
		"populate with network":         {Step: sandbox.StepPopulate, WorkspaceKey: key, NetworkProfile: sandbox.NetHooks, Populate: &sandbox.Populate{Op: sandbox.OpProbe}},
		"populate without an operation": {Step: sandbox.StepPopulate, WorkspaceKey: key},
		"unknown operation":             {Step: sandbox.StepPopulate, WorkspaceKey: key, Populate: &sandbox.Populate{Op: "exec"}},
		"branch that is a flag":         {Step: sandbox.StepPopulate, WorkspaceKey: "GH-new", Populate: &sandbox.Populate{Op: sandbox.OpClone, Branch: "--upload-pack=x", BaseBranch: "main"}},
		"branch with dots":              {Step: sandbox.StepPopulate, WorkspaceKey: "GH-new", Populate: &sandbox.Populate{Op: sandbox.OpClone, Branch: "a/../b", BaseBranch: "main"}},
		"credential in the environment": {Step: sandbox.StepHook, WorkspaceKey: key, Argv: argv, Env: map[string]string{"GITHUB_TOKEN": "x"}},
		"api key in the environment":    {Step: sandbox.StepHook, WorkspaceKey: key, Argv: argv, Env: map[string]string{"anthropic_api_key": "x"}},
		"overriding the proxy":          {Step: sandbox.StepHook, WorkspaceKey: key, Argv: argv, NetworkProfile: sandbox.NetHooks, Env: map[string]string{"HTTPS_PROXY": "http://evil"}},
		"overriding HOME":               {Step: sandbox.StepHook, WorkspaceKey: key, Argv: argv, Env: map[string]string{"HOME": "/"}},
		"bad variable name":             {Step: sandbox.StepHook, WorkspaceKey: key, Argv: argv, Env: map[string]string{"A=B": "x"}},
		"newline in a value":            {Step: sandbox.StepHook, WorkspaceKey: key, Argv: argv, Env: map[string]string{"A": "x\ny"}},
	} {
		_, err := b.Run(ctx, req, nil)
		var se *sandbox.Error
		if !errors.As(err, &se) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if ok, _ := b.volumeExists(ctx, b.Volume("GH-new")); ok {
		t.Error("a refused clone created a volume")
	}
	// A command that does not exist is the step's failure, not the launcher's.
	res, err := b.Run(ctx, sandbox.Request{Step: sandbox.StepHook, WorkspaceKey: key, Argv: []string{"no-such-program"}}, nil)
	if err != nil || res.ExitStatus == 0 {
		t.Errorf("missing program: %v, status %d", err, res.ExitStatus)
	}
}

func TestBoxConfigurationIsChecked(t *testing.T) {
	for name, b := range map[string]*Box{
		"worker with a space":  {Worker: "a b", Image: "i", MirrorDir: "/m", HandoffDir: "/h"},
		"no image":             {Worker: "w", MirrorDir: "/m", HandoffDir: "/h"},
		"image that is a flag": {Worker: "w", Image: "--privileged", MirrorDir: "/m", HandoffDir: "/h"},
		"relative mirror":      {Worker: "w", Image: "i", MirrorDir: "m", HandoffDir: "/h"},
		"comma in a path":      {Worker: "w", Image: "i", MirrorDir: "/m,dst=/etc", HandoffDir: "/h"},
		"relative licence dir": {Worker: "w", Image: "i", MirrorDir: "/m", HandoffDir: "/h", UnityLicenceDir: "lic"},
		"comma in licence dir": {Worker: "w", Image: "i", MirrorDir: "/m", HandoffDir: "/h", UnityLicenceDir: "/l,dst=/etc"},
	} {
		if _, err := b.Run(ctx, sandbox.Request{Step: sandbox.StepHook, WorkspaceKey: "k", Argv: []string{"true"}}, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAllowFile(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAllow(dir, "agent", []string{"a.example", "b.example"}); err != nil {
		t.Fatal(err)
	}
	if got := ReadAllow(filepath.Join(dir, "agent.json")); len(got) != 2 || got[1] != "b.example" {
		t.Fatalf("%v", got)
	}
	WriteAllow(dir, "agent", nil)
	if got := ReadAllow(filepath.Join(dir, "agent.json")); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{"), 0o644)
	if ReadAllow(filepath.Join(dir, "broken.json")) != nil || ReadAllow(filepath.Join(dir, "none.json")) != nil {
		t.Fatal("a broken or missing file allowed something")
	}
}

// TestRealImage builds the image people get, which needs the internet, and
// checks what the service relies on in it.
func TestRealImage(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_INTERNET") == "" {
		t.Skip("set MAESTRO_TEST_INTERNET=1 to build the real image")
	}
	b := *box(t)
	b.Image = "maestro-test-real:latest"
	var out bytes.Buffer
	if err := b.BuildImage(ctx, Build{Binary: filepath.Join(stateDir, "maestro"), Out: &out}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	run := func(args ...string) string {
		t.Helper()
		o, err := exec.Command(engine, append([]string{"run", "--rm", "--user", AgentUID, "--cap-drop", "ALL",
			"--network", "none", "--entrypoint", "", b.Image}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, o)
		}
		return strings.TrimSpace(string(o))
	}
	if got := run("claude", "--version"); !strings.HasPrefix(got, ClaudeVersion+" ") {
		t.Errorf("claude --version = %q, want %s", got, ClaudeVersion)
	}
	if got := run("maestro", "version"); !strings.HasPrefix(got, "maestro ") {
		t.Errorf("maestro version = %q", got)
	}
	if got := run("sh", "-c", "git --version && id -u && test -w /workspace -a -w /claude -a -w /handoff && echo writable"); !strings.Contains(got, "10001\nwritable") {
		t.Errorf("image contents: %q", got)
	}
	// Logged out, Claude Code says so in the way the login check reads.
	cmd := exec.Command(engine, "run", "--rm", "--user", AgentUID, "--network", "none", "--entrypoint", "",
		"--env", "CLAUDE_CONFIG_DIR=/claude", "--env", "HOME=/home/agent", b.Image, "claude", "auth", "status", "--json")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	if yes, known := loginAnswer(stdout.Bytes(), err); yes || !known {
		t.Errorf("claude auth status without a login: logged in %v, known %v, %v\n%s", yes, known, err, stdout.String())
	}
}

func TestLoginAnswer(t *testing.T) {
	exit := func(code int) error { return exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run() }
	for name, tc := range map[string]struct {
		out        string
		err        error
		yes, known bool
	}{
		"json yes":            {`{"loggedIn": true, "authMethod": "claude.ai"}`, nil, true, true},
		"json no, status 1":   {`{"loggedIn": false}`, exit(1), false, true},
		"json no, status 0":   {`{"loggedIn": false}`, nil, false, true},
		"no json, status 0":   {"Logged in", nil, true, true},
		"no json, status 1":   {"Not logged in", exit(1), false, true},
		"no json, status 127": {"", exit(127), false, false},
		"json without answer": {`{}`, exit(2), false, false},
	} {
		if yes, known := loginAnswer([]byte(tc.out), tc.err); yes != tc.yes || known != tc.known {
			t.Errorf("%s: logged in %v, known %v", name, yes, known)
		}
	}
}
