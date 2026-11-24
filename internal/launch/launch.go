package launch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/iandeherdt/Maestro/internal/sandbox"
)

// Launcher runs requests for one worker.
type Launcher struct {
	Cfg *Config
	// Self is this program's path, run inside each unit. Default: the
	// configuration's Relay, or the running executable.
	Self string
	// SystemdRun runs systemd-run with args and returns the exit status of
	// the unit. Default: the real one; tests replace it.
	SystemdRun func(ctx context.Context, unit string, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	// Systemd is the version of the machine's systemd, which decides the
	// properties a unit may have. Default: read once from systemctl; 0
	// when it cannot be read, taken as new.
	Systemd int
	// Lookup and Chown work on the machine's users; tests replace them.
	Lookup func(name string) (uid, gid int, err error)
	Chown  func(path string, uid, gid int) error
}

// ErrRequest is a request the launcher refuses.
var ErrRequest = errors.New("refused")

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrRequest, fmt.Sprintf(format, a...))
}

// maxRequest bounds the request line.
const maxRequest = 1 << 20

// ReadRequest reads the request: one line of JSON, before the step's own
// standard input.
func ReadRequest(r *bufio.Reader) (sandbox.Request, error) {
	var req sandbox.Request
	line, err := r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return req, refuse("the request is too long")
	}
	if err != nil && !(errors.Is(err, io.EOF) && len(line) > 0) {
		return req, refuse("no request")
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, refuse("the request is not valid JSON: %v", err)
	}
	return req, nil
}

// NewReader is a reader big enough for any request line.
func NewReader(r io.Reader) *bufio.Reader { return bufio.NewReaderSize(r, maxRequest) }

func (l *Launcher) self() string {
	switch {
	case l.Self != "":
		return l.Self
	case l.Cfg.Relay != "":
		return l.Cfg.Relay
	}
	p, err := os.Executable()
	if err != nil {
		return "/usr/local/libexec/maestro-launch"
	}
	return p
}

// IDs returns the uid and gid of a user, refusing root.
func (l *Launcher) IDs(name string) (int, int, error) {
	if l.Lookup != nil {
		return l.Lookup(name)
	}
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil || uid == 0 {
		return 0, 0, fmt.Errorf("the user %s cannot be used", name)
	}
	return uid, gid, nil
}

func (l *Launcher) fchown(f *os.File, uid, gid int) error {
	if l.Chown != nil {
		return l.Chown(f.Name(), uid, gid)
	}
	return f.Chown(uid, gid)
}

func (l *Launcher) chown(path string, uid, gid int) error {
	if l.Chown != nil {
		return l.Chown(path, uid, gid)
	}
	return os.Lchown(path, uid, gid)
}

// Handle runs one request and returns the step's exit status. An error
// means the step was refused or could not be run.
func (l *Launcher) Handle(ctx context.Context, req sandbox.Request, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	switch req.Step {
	case sandbox.StepLoginStatus, sandbox.StepSelfTest:
		// The launcher chooses everything these run.
		if req.WorkspaceKey != "" || len(req.Argv) != 0 || len(req.Env) != 0 || req.Populate != nil || req.Limits.TimeoutMS != 0 {
			return 0, refuse("%s takes a network profile only", req.Step)
		}
		if req.Step == sandbox.StepLoginStatus {
			if req.NetworkProfile != sandbox.NetAgent {
				return 0, refuse("login-status uses the agent network profile")
			}
			return l.loginStatus(ctx, stdout, stderr)
		}
		return l.selfTest(ctx, req.NetworkProfile, stdout, stderr)
	}
	if !sandbox.ValidKey(req.WorkspaceKey) {
		return 0, refuse("invalid workspace key")
	}
	network := ""
	switch req.NetworkProfile {
	case "", sandbox.NetNone:
	case sandbox.NetAgent, sandbox.NetHooks:
		network = req.NetworkProfile
	default:
		return 0, refuse("unknown network profile")
	}
	if req.Limits.TimeoutMS < 0 {
		return 0, refuse("a negative time limit")
	}
	s := spec{step: req.Step, key: req.WorkspaceKey, workdir: repoDir, timeout: req.Limits.TimeoutMS}
	switch req.Step {
	case sandbox.StepPopulate:
		if req.Populate == nil || len(req.Argv) != 0 || network != "" || len(req.Env) != 0 {
			return 0, refuse("a populate step takes an operation, no command, no network and no environment")
		}
		return l.populate(ctx, s, *req.Populate, stdout, stderr)
	case sandbox.StepExport:
		if len(req.Argv) != 0 || network != "" || len(req.Env) != 0 || req.Populate != nil {
			return 0, refuse("an export step takes no command, no network and no environment")
		}
		return l.export(ctx, s, stdout, stderr)
	case sandbox.StepAgent:
		if network != sandbox.NetAgent {
			return 0, refuse("an agent step uses the agent network profile")
		}
		s.claude = true
	case sandbox.StepHook, sandbox.StepVerify:
		// Repository scripts never see the login and never reach the hosts
		// the agent may reach.
		if network == sandbox.NetAgent {
			return 0, refuse("only an agent step uses the agent network profile")
		}
		s.licence = true
	default:
		return 0, refuse("unknown step")
	}
	if req.Populate != nil {
		return 0, refuse("only a populate step takes an operation")
	}
	if len(req.Argv) == 0 || req.Argv[0] == "" || strings.HasPrefix(req.Argv[0], "-") {
		return 0, refuse("the step has no command")
	}
	for _, a := range req.Argv {
		if strings.ContainsRune(a, 0) {
			return 0, refuse("the command contains a NUL byte")
		}
	}
	if !l.exists(s.key) {
		return 0, refuse("the workspace does not exist")
	}
	s.network = network
	u, err := l.Cfg.build(s, req.Argv, req.Env)
	if err != nil {
		return 0, refuse("%v", err)
	}
	return l.run(ctx, u, stdin, stdout, stderr)
}

// exists reports whether the workspace folder is there and is a folder.
func (l *Launcher) exists(key string) bool {
	fi, err := os.Lstat(l.Cfg.workspace(key))
	return err == nil && fi.IsDir()
}

func (l *Launcher) run(ctx context.Context, u *unit, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	run := l.SystemdRun
	if run == nil {
		run = systemdRun
	}
	return run(ctx, u.name, u.args(l.self(), l.systemd()), stdin, stdout, stderr)
}

var systemdVersionRE = regexp.MustCompile(`^systemd (\d+)`)

// systemd returns the version of the machine's systemd.
func (l *Launcher) systemd() int {
	if l.Systemd == 0 && l.SystemdRun == nil {
		cmd := exec.Command("/usr/bin/systemctl", "--version")
		cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
		if out, err := cmd.Output(); err == nil {
			if m := systemdVersionRE.FindSubmatch(out); m != nil {
				l.Systemd, _ = strconv.Atoi(string(m[1]))
			}
		}
	}
	return l.Systemd
}

// systemdRun runs systemd-run and, when ctx ends first, stops the unit,
// which kills every process in it.
func systemdRun(ctx context.Context, unit string, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := exec.Command("/usr/bin/systemd-run", args...)
	// The canary is for the self-test: no step may see this environment.
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", sandbox.CanaryName + "=" + unitToken()}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return status(err)
	case <-ctx.Done():
		stop := exec.Command("/usr/bin/systemctl", "stop", "--", unit+".service")
		stop.Env = cmd.Env
		stop.Run()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			cmd.Process.Kill()
			<-done
		}
		return 0, ctx.Err()
	}
}

func status(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 0, err
}

// capture runs a unit and keeps at most max bytes of its output.
func (l *Launcher) capture(ctx context.Context, u *unit, stderr io.Writer, max int) (int, []byte, error) {
	var out limited
	out.max = max
	code, err := l.run(ctx, u, nil, &out, stderr)
	return code, out.buf.Bytes(), err
}

type limited struct {
	buf bytes.Buffer
	max int
}

func (w *limited) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
		} else {
			w.buf.Write(p)
		}
	}
	return len(p), nil
}

// populate creates, inspects, updates and removes workspaces. Only names
// come from the request; the commands are fixed.
func (l *Launcher) populate(ctx context.Context, s spec, p sandbox.Populate, stdout, stderr io.Writer) (int, error) {
	c := l.Cfg
	dir := c.workspace(s.key)
	s.workdir = workDir
	switch p.Op {
	case sandbox.OpProbe:
		if !l.exists(s.key) {
			fmt.Fprintln(stdout, "missing")
			return 0, nil
		}
		u, err := c.build(s, []string{"/bin/sh", "-c", sandbox.ProbeScript}, nil)
		if err != nil {
			return 0, refuse("%v", err)
		}
		code, out, err := l.capture(ctx, u, stderr, 1024)
		if err != nil || code != 0 {
			return code, err
		}
		// Printed in the sandbox, so only a commit name is passed on.
		if base := strings.TrimSpace(string(out)); sandbox.ValidCommit(base) {
			fmt.Fprintln(stdout, "exists "+base)
			return 0, nil
		}
		// A folder without a finished clone is a leftover, not a workspace.
		if code, err := l.remove(ctx, s, stderr); err != nil || code != 0 {
			return code, err
		}
		fmt.Fprintln(stdout, "missing")
		return 0, nil

	case sandbox.OpClone:
		if !sandbox.ValidBranch(p.Branch) || !sandbox.ValidBranch(p.BaseBranch) {
			return 0, refuse("invalid branch name")
		}
		if _, err := os.Lstat(dir); err == nil {
			return 0, refuse("the workspace already exists")
		}
		uid, gid, err := l.IDs(c.User)
		if err != nil {
			return 0, err
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			return 0, err
		}
		if err := l.chown(dir, uid, gid); err != nil {
			os.Remove(dir)
			return 0, err
		}
		s.mirror = true
		u, err := c.build(s, []string{"/bin/sh", "-c", sandbox.CloneScript, "clone", p.Branch, p.BaseBranch, c.BotName, c.BotEmail}, nil)
		if err != nil {
			os.Remove(dir)
			return 0, refuse("%v", err)
		}
		code, out, err := l.capture(ctx, u, stderr, 1024)
		base := strings.TrimSpace(string(out))
		if err == nil && code == 0 && !sandbox.ValidCommit(base) {
			err = errors.New("the clone did not report a commit")
		}
		if err != nil || code != 0 {
			// A partial workspace must not be found later.
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cancel()
			l.remove(cctx, s, io.Discard)
			return code, err
		}
		fmt.Fprintln(stdout, base)
		return 0, nil

	case sandbox.OpFastForward:
		if !sandbox.ValidBranch(p.Branch) || p.BaseBranch != "" && !sandbox.ValidBranch(p.BaseBranch) {
			return 0, refuse("invalid branch name")
		}
		if !l.exists(s.key) {
			return 0, refuse("the workspace does not exist")
		}
		s.mirror, s.workdir = true, workDir
		u, err := c.build(s, []string{"/bin/sh", "-c", sandbox.FastForwardScript, "fast-forward", p.Branch, p.BaseBranch}, nil)
		if err != nil {
			return 0, refuse("%v", err)
		}
		return l.run(ctx, u, nil, stdout, stderr)

	case sandbox.OpRemove:
		if p.Branch != "" || p.BaseBranch != "" {
			return 0, refuse("remove takes no branch")
		}
		code, err := l.remove(ctx, s, stderr)
		os.RemoveAll(c.handoff(s.key))
		os.Remove(c.bundle(s.key))
		return code, err
	}
	return 0, refuse("unknown populate operation")
}

// remove empties the workspace from inside the sandbox, as the sandbox
// user, and then removes the empty folder. Root never walks a tree the
// sandbox wrote.
func (l *Launcher) remove(ctx context.Context, s spec, stderr io.Writer) (int, error) {
	dir := l.Cfg.workspace(s.key)
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !fi.IsDir() {
		// Only root can put something here; leave it for a person.
		return 0, fmt.Errorf("%s is not a folder", dir)
	}
	s.workdir, s.mirror, s.claude, s.handoff, s.licence, s.network = workDir, false, false, false, false, ""
	u, err := l.Cfg.build(s, []string{"/usr/bin/find", workDir, "-mindepth", "1", "-delete"}, nil)
	if err != nil {
		return 0, refuse("%v", err)
	}
	code, err := l.run(ctx, u, nil, io.Discard, stderr)
	if err != nil || code != 0 {
		return code, err
	}
	return 0, os.Remove(dir)
}

// export runs the export script and hands the bundle to the service user.
// The bundle is written by the sandbox into a folder that holds nothing
// else; it is taken only if it is a regular file.
func (l *Launcher) export(ctx context.Context, s spec, stdout, stderr io.Writer) (int, error) {
	c := l.Cfg
	if !l.exists(s.key) {
		return 0, refuse("the workspace does not exist")
	}
	uid, gid, err := l.IDs(c.User)
	if err != nil {
		return 0, err
	}
	suid, sgid, err := l.IDs(c.ServiceUser)
	if err != nil {
		return 0, err
	}
	dir, final := c.handoff(s.key), c.bundle(s.key)
	os.Remove(final)
	if err := os.RemoveAll(dir); err != nil {
		return 0, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	if err := l.chown(dir, uid, gid); err != nil {
		return 0, err
	}
	s.handoff = true
	u, err := c.build(s, []string{"/bin/sh", "-c", sandbox.ExportScript, "export", c.BotName, c.BotEmail}, nil)
	if err != nil {
		return 0, refuse("%v", err)
	}
	code, err := l.run(ctx, u, nil, stdout, stderr)
	if err != nil || code != 0 {
		return code, err
	}
	produced := dir + "/" + sandbox.BundleName
	// Opened without following a link, and changed through the open
	// file, so no path can redirect what root does.
	f, err := os.OpenFile(produced, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil // nothing to hand over: no new commits
	}
	if err != nil {
		return 0, errors.New("the export left something that is not a regular file")
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if !fi.Mode().IsRegular() {
		return 0, errors.New("the export left something that is not a regular file")
	}
	if err := l.fchown(f, suid, sgid); err != nil {
		return 0, err
	}
	if err := f.Chmod(0o600); err != nil {
		return 0, err
	}
	// The unit has ended and every process in it is gone, so nothing can
	// swap the file between the open and the rename.
	return 0, os.Rename(produced, final)
}

// Time limits of the steps the launcher makes up itself.
const (
	loginTimeoutMS    = 15 * 60 * 1000
	statusTimeoutMS   = 60 * 1000
	selfTestTimeoutMS = 2 * 60 * 1000
)

// claudeSpec is Claude Code with the agent's state and network and no
// workspace, for the login and its status.
func claudeSpec(step sandbox.Step, timeout int64) spec {
	return spec{step: step, noWorkspace: true, claude: true, network: sandbox.NetAgent, workdir: "/tmp", timeout: timeout}
}

// Login runs Claude Code's sign-in on the caller's terminal
// (`maestro-launch <worker> login`).
func (l *Launcher) Login(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	s := claudeSpec("login", loginTimeoutMS)
	s.pty = true
	u, err := l.Cfg.build(s, []string{l.Cfg.tools() + "/claude", "auth", "login", "--claudeai"}, nil)
	if err != nil {
		return 0, refuse("%v", err)
	}
	u.env["TERM"] = "xterm-256color"
	delete(u.env, "CI")
	return l.run(ctx, u, stdin, stdout, stderr)
}

// loginStatus prints `claude auth status --json`.
func (l *Launcher) loginStatus(ctx context.Context, stdout, stderr io.Writer) (int, error) {
	u, err := l.Cfg.build(claudeSpec(sandbox.StepLoginStatus, statusTimeoutMS),
		[]string{l.Cfg.tools() + "/claude", "auth", "status", "--json"}, nil)
	if err != nil {
		return 0, refuse("%v", err)
	}
	return l.run(ctx, u, nil, stdout, stderr)
}

// selfTest runs the probe in a workspace made for it, which is removed
// again.
func (l *Launcher) selfTest(ctx context.Context, profile string, stdout, stderr io.Writer) (int, error) {
	network := ""
	switch profile {
	case sandbox.NetNone:
	case sandbox.NetAgent, sandbox.NetHooks:
		network = profile
	default:
		return 0, refuse("unknown network profile")
	}
	c := l.Cfg
	s := spec{step: sandbox.StepSelfTest, key: "selftest-" + unitToken(), network: network, workdir: workDir, timeout: selfTestTimeoutMS}
	uid, gid, err := l.IDs(c.User)
	if err != nil {
		return 0, err
	}
	dir := c.workspace(s.key)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return 0, err
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		l.remove(cctx, s, io.Discard)
	}()
	if err := l.chown(dir, uid, gid); err != nil {
		return 0, err
	}
	argv := []string{c.tools() + "/maestro", "probe", "--profile", profile,
		"--canary", sandbox.CanaryName, "--deny", sandbox.ProbeDenyHost}
	if c.TasksMax > 0 {
		argv = append(argv, "--pids", strconv.Itoa(c.TasksMax))
	}
	if n, ok := memoryBytes(c.MemoryMax); ok {
		argv = append(argv, "--memory", strconv.FormatInt(n, 10))
	}
	u, err := c.build(s, argv, nil)
	if err != nil {
		return 0, refuse("%v", err)
	}
	return l.run(ctx, u, nil, stdout, stderr)
}

// memoryBytes turns systemd's "4G" into bytes.
func memoryBytes(s string) (int64, bool) {
	if !memRE.MatchString(s) {
		return 0, false
	}
	n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
	if err != nil {
		return 0, false
	}
	return n << map[byte]uint{'K': 10, 'M': 20, 'G': 30}[s[len(s)-1]], true
}
