// Package dockerbox is the sandbox backend that runs every step in a
// throwaway Linux container (SPEC 11, backend "docker").
//
// What a step can touch is decided here, from the step's name alone:
//
//   - Files: the one workspace volume of the issue. The mirror is read-only
//     and only present while populating. The Claude Code login is present
//     for agent steps only. The hand-off directory of the issue is present
//     for the export step only. Nothing of the host is mounted.
//   - Network: none, or an internal Docker network with no route out, on
//     which the only other machine is the allowlisting proxy.
//   - Privileges: an unprivileged user, no capabilities, no new privileges.
//   - Lifetime: the container is removed when the step ends or its context
//     does, which kills everything in it.
package dockerbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/sandbox"
)

const (
	// AgentUID is the user every step runs as inside the container.
	AgentUID = "10001"
	// ProxyPort is where the proxy listens inside its container.
	ProxyPort = "3128"

	workDir    = "/workspace"
	repoDir    = "/workspace/repo"
	mirrorDir  = "/mirror"
	licenceDir = "/unity-licence"
	claudeDir  = "/claude"
	handoffDir = "/handoff"
	bundleName = "out.bundle"

	maxStdout   = 1 << 20
	maxStderr   = 64 << 10
	killTimeout = 30 * time.Second
)

// Box implements sandbox.Launcher and sandbox.Streamer with the docker
// command line client.
type Box struct {
	// Docker is the client binary. Default "docker".
	Docker string
	// Podman says that the client is podman, which takes the same commands
	// with a few differences that are handled where they matter.
	Podman bool
	// Worker names this installation. It is part of every container,
	// volume and network name, so two workers never share anything.
	Worker string
	// Image is the sandbox image.
	Image string
	// MirrorDir and HandoffDir are directories of the control zone.
	MirrorDir, HandoffDir string
	// SkillsDir and AgentsDir hold the skills and agent files the owner
	// added. The agent gets them read-only, in the place Claude Code looks
	// for them. Mounting them, also when empty, means that what an agent
	// writes there itself is never seen by a later run.
	SkillsDir, AgentsDir string
	// UnityLicenceDir holds an activated Unity licence (`maestro unity
	// activate`). It is mounted read-only into the steps that run the
	// repository's own scripts, hooks and verify, so Unity can open the
	// project there. It is never mounted into the agent's step. Empty
	// means no licence.
	UnityLicenceDir string
	// Memory ("6g"), CPUs ("3.0") and PIDs bound every step.
	Memory, CPUs string
	PIDs         int64
	// BotName and BotEmail are set as the commit identity of a new
	// workspace.
	BotName, BotEmail string
	// Platform, such as "linux/amd64", is the platform every container and
	// the image are made for. Empty is the machine's own. A pack whose
	// tools exist for one platform only sets it; on another machine the
	// containers then run emulated, more slowly.
	Platform string
}

var (
	workerRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	identRE  = regexp.MustCompile(`^[^\x00-\x1f"\\]{1,100}$`)
)

func (b *Box) docker() string {
	switch {
	case b.Docker != "":
		return b.Docker
	case b.Podman:
		return "podman"
	}
	return "docker"
}

// engineFlags are run and create flags only one of the clients needs.
func (b *Box) engineFlags() []string {
	var f []string
	if b.Podman {
		// Podman copies the proxy variables of whoever starts it into
		// the container unless told not to.
		f = append(f, "--http-proxy=false")
		// The sandbox image is built here and never comes from a
		// registry. Without this, Podman treats a short name it cannot
		// match locally (with --platform, for one) as a pull and asks at
		// the terminal which registry to take it from.
		f = append(f, "--pull=never")
	}
	if b.Platform != "" {
		f = append(f, "--platform", b.Platform)
	}
	return f
}

// bind is a bind mount of a host directory. With podman the directory is
// relabelled, or SELinux (RHEL's default) keeps the container out of it.
func (b *Box) bind(src, dst string, readonly bool) string {
	m := "type=bind,src=" + src + ",dst=" + dst
	if readonly {
		m += ",readonly"
	}
	if b.Podman {
		m += ",relabel=shared"
	}
	return m
}

func (b *Box) prefix() string { return "maestro-" + b.Worker }

// Volume is the name of an issue's workspace volume.
func (b *Box) Volume(key string) string { return b.prefix() + "-ws-" + key }

// ClaudeVolume holds the agent's Claude Code state, including its login.
func (b *Box) ClaudeVolume() string { return b.prefix() + "-claude" }

// Network is the internal network of a profile ("agent" or "hooks").
func (b *Box) Network(profile string) string { return b.prefix() + "-" + profile }

// ProxyName is the container name of a profile's proxy.
func (b *Box) ProxyName(profile string) string { return b.prefix() + "-proxy-" + profile }

func fail(format string, a ...any) error { return &sandbox.Error{Msg: fmt.Sprintf(format, a...)} }

type capped struct {
	buf bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// cli runs a docker management command and returns its standard output.
func (b *Box) cli(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, b.docker(), args...)
	var out, errb capped
	out.max, errb.max = maxStdout, 4096
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.buf.String(), fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(errb.buf.String()))
	}
	return strings.TrimSpace(out.buf.String()), nil
}

func (b *Box) volumeExists(ctx context.Context, name string) (bool, error) {
	out, err := b.cli(ctx, "volume", "ls", "--quiet", "--filter", "name=^"+regexp.QuoteMeta(name)+"$")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == name, nil
}

func token() string {
	var r [6]byte
	rand.Read(r[:])
	return hex.EncodeToString(r[:])
}

// spec is what one container gets.
type spec struct {
	step    sandbox.Step
	key     string
	argv    []string
	workdir string
	network string // "" means none
	mirror  bool
	claude  bool
	handoff string // host directory, or ""
	licence bool   // mount UnityLicenceDir
	env     map[string]string
}

func (b *Box) check() error {
	switch {
	case !workerRE.MatchString(b.Worker):
		return fail("the worker name cannot be used in container names")
	case b.Image == "" || strings.HasPrefix(b.Image, "-"):
		return fail("no sandbox image is configured")
	case !filepath.IsAbs(b.MirrorDir) || !filepath.IsAbs(b.HandoffDir):
		return fail("the mirror and hand-off directories must be absolute paths")
	case strings.ContainsAny(b.MirrorDir+b.HandoffDir, ",\n"):
		return fail("the mirror and hand-off paths must not contain commas")
	case b.UnityLicenceDir != "" && (!filepath.IsAbs(b.UnityLicenceDir) || strings.ContainsAny(b.UnityLicenceDir, ",\n")):
		return fail("the Unity licence directory must be an absolute path without commas")
	}
	return nil
}

// baseEnv is the whole environment of a step. Nothing is inherited from
// the service (SPEC 10.1).
func (b *Box) baseEnv(profile string) map[string]string {
	env := map[string]string{
		"HOME": "/home/agent", "PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG": "C.UTF-8", "TZ": "UTC", "CI": "1", "TERM": "dumb", "NO_PROXY": "",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
	}
	if profile != "" {
		proxy := "http://" + b.ProxyName(profile) + ":" + ProxyPort
		env["HTTPS_PROXY"], env["HTTP_PROXY"] = proxy, proxy
		env["https_proxy"], env["http_proxy"] = proxy, proxy
	}
	return env
}

func (b *Box) runArgs(name string, s spec) []string {
	args := []string{"run", "--rm", "--interactive", "--name", name,
		"--label", "maestro.worker=" + b.Worker, "--label", "maestro.step=" + string(s.step),
		"--user", AgentUID + ":" + AgentUID,
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--hostname", "sandbox", "--init",
		"--entrypoint", "",
		"--mount", "type=volume,src=" + b.Volume(s.key) + ",dst=" + workDir,
		"--workdir", s.workdir,
	}
	args = append(args, b.engineFlags()...)
	if b.Memory != "" {
		args = append(args, "--memory", b.Memory, "--memory-swap", b.Memory)
	}
	if b.CPUs != "" {
		args = append(args, "--cpus", b.CPUs)
	}
	if b.PIDs > 0 {
		args = append(args, "--pids-limit", strconv.FormatInt(b.PIDs, 10))
	}
	if s.network == "" {
		args = append(args, "--network", "none")
	} else {
		args = append(args, "--network", b.Network(s.network))
	}
	if s.mirror {
		args = append(args, "--mount", b.bind(b.MirrorDir, mirrorDir, true))
	}
	if s.claude {
		args = append(args, "--mount", "type=volume,src="+b.ClaudeVolume()+",dst="+claudeDir)
		if b.SkillsDir != "" {
			args = append(args, "--mount", b.bind(b.SkillsDir, claudeDir+"/skills", true))
		}
		if b.AgentsDir != "" {
			args = append(args, "--mount", b.bind(b.AgentsDir, claudeDir+"/agents", true))
		}
	}
	if s.handoff != "" {
		args = append(args, "--mount", b.bind(s.handoff, handoffDir, false))
	}
	if s.licence {
		args = append(args, "--mount", b.bind(b.UnityLicenceDir, licenceDir, true))
	}
	keys := make([]string, 0, len(s.env))
	for k := range s.env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--env", k+"="+s.env[k])
	}
	args = append(args, b.Image)
	return append(args, s.argv...)
}

// start runs one container to its end. When ctx ends first the container
// is removed, which kills every process in it.
func (b *Box) start(ctx context.Context, s spec, stdin []byte, stdout io.Writer) (sandbox.Result, error) {
	name := b.prefix() + "-" + strings.ToLower(s.key) + "-" + string(s.step) + "-" + token()
	cmd := exec.Command(b.docker(), b.runArgs(name, s)...)
	cmd.Stdin = bytes.NewReader(stdin)
	var errb capped
	errb.max = maxStderr
	var outb capped
	outb.max = maxStdout
	if stdout != nil {
		cmd.Stdout = stdout
	} else {
		cmd.Stdout = &outb
	}
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return sandbox.Result{}, fail("docker could not be started: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		b.remove(name)
		<-done
		return sandbox.Result{}, ctx.Err()
	}
	res := sandbox.Result{Stdout: outb.buf.Bytes(), Stderr: errb.buf.Bytes()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.ExitStatus = ee.ExitCode()
		// 125 is docker's own failure: the step never ran.
		if res.ExitStatus == 125 {
			return res, fail("the container could not be started: %s", lastLine(res.Stderr))
		}
		if res.ExitStatus < 0 {
			return res, fail("docker was stopped by a signal")
		}
	default:
		return res, fail("docker: %v", err)
	}
	return res, nil
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	l := lines[len(lines)-1]
	if len(l) > 300 {
		l = l[:300]
	}
	return l
}

// remove force-removes a container, with a context of its own because the
// caller's is usually over.
func (b *Box) remove(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), killTimeout)
	defer cancel()
	exec.CommandContext(ctx, b.docker(), "rm", "--force", name).Run()
}

// Run implements sandbox.Launcher.
func (b *Box) Run(ctx context.Context, req sandbox.Request, stdin []byte) (sandbox.Result, error) {
	return b.launch(ctx, req, stdin, nil)
}

// Stream implements sandbox.Streamer.
func (b *Box) Stream(ctx context.Context, req sandbox.Request, stdin []byte, stdout io.Writer) (sandbox.Result, error) {
	return b.launch(ctx, req, stdin, stdout)
}

func (b *Box) launch(ctx context.Context, req sandbox.Request, stdin []byte, stdout io.Writer) (sandbox.Result, error) {
	if err := b.check(); err != nil {
		return sandbox.Result{}, err
	}
	if !sandbox.ValidKey(req.WorkspaceKey) {
		return sandbox.Result{}, fail("invalid workspace key")
	}
	profile := ""
	switch req.NetworkProfile {
	case "", sandbox.NetNone:
	case sandbox.NetAgent, sandbox.NetHooks:
		profile = req.NetworkProfile
	default:
		return sandbox.Result{}, fail("unknown network profile")
	}
	env := b.baseEnv(profile)
	for k, v := range req.Env {
		if !sandbox.AllowedEnv(k, v) {
			return sandbox.Result{}, fail("environment variable %q is not allowed in the sandbox", k)
		}
		if _, fixed := env[k]; fixed {
			return sandbox.Result{}, fail("environment variable %q is set by the sandbox", k)
		}
		env[k] = v
	}
	if req.Limits.TimeoutMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.Limits.TimeoutMS)*time.Millisecond)
		defer cancel()
	}
	s := spec{step: req.Step, key: req.WorkspaceKey, workdir: repoDir, env: env}

	switch req.Step {
	case sandbox.StepPopulate:
		if req.Populate == nil || len(req.Argv) != 0 || profile != "" {
			return sandbox.Result{}, fail("a populate step takes an operation, no command and no network")
		}
		return b.populate(ctx, s, *req.Populate)
	case sandbox.StepExport:
		if len(req.Argv) != 0 || profile != "" {
			return sandbox.Result{}, fail("an export step takes no command and no network")
		}
		return b.export(ctx, s)
	case sandbox.StepAgent:
		if profile != sandbox.NetAgent {
			return sandbox.Result{}, fail("an agent step uses the agent network profile")
		}
		s.claude = true
		s.env["CLAUDE_CONFIG_DIR"] = claudeDir
	case sandbox.StepHook, sandbox.StepVerify:
		// Repository scripts never see the login and never reach the
		// hosts the agent may reach.
		if profile == sandbox.NetAgent {
			return sandbox.Result{}, fail("only an agent step uses the agent network profile")
		}
		s.licence = b.UnityLicenceDir != ""
	default:
		return sandbox.Result{}, fail("unknown step")
	}
	if len(req.Argv) == 0 || req.Argv[0] == "" || strings.HasPrefix(req.Argv[0], "-") {
		return sandbox.Result{}, fail("the step has no command")
	}
	s.argv, s.network = req.Argv, profile
	ok, err := b.volumeExists(ctx, b.Volume(s.key))
	if err != nil {
		return sandbox.Result{}, fail("%v", err)
	}
	if !ok {
		// docker would create an empty volume by itself otherwise.
		return sandbox.Result{}, fail("the workspace does not exist")
	}
	return b.start(ctx, s, stdin, stdout)
}

func (b *Box) populate(ctx context.Context, s spec, p sandbox.Populate) (sandbox.Result, error) {
	vol := b.Volume(s.key)
	exists, err := b.volumeExists(ctx, vol)
	if err != nil {
		return sandbox.Result{}, fail("%v", err)
	}
	s.workdir = workDir
	switch p.Op {
	case sandbox.OpProbe:
		if !exists {
			return sandbox.Result{Stdout: []byte("missing\n")}, nil
		}
		s.argv = []string{"sh", "-c", sandbox.ProbeScript}
		res, err := b.start(ctx, s, nil, nil)
		if err != nil || res.ExitStatus != 0 {
			return res, err
		}
		// Printed in the sandbox, so only a commit name is passed on.
		if base := strings.TrimSpace(string(res.Stdout)); sandbox.ValidCommit(base) {
			return sandbox.Result{Stdout: []byte("exists " + base + "\n")}, nil
		}
		// A volume without a finished clone is a leftover, not a workspace.
		if err := b.removeVolume(ctx, s.key); err != nil {
			return sandbox.Result{}, err
		}
		return sandbox.Result{Stdout: []byte("missing\n")}, nil

	case sandbox.OpClone:
		if !sandbox.ValidBranch(p.Branch) || !sandbox.ValidBranch(p.BaseBranch) {
			return sandbox.Result{}, fail("invalid branch name")
		}
		if !identRE.MatchString(b.BotName) || !identRE.MatchString(b.BotEmail) {
			return sandbox.Result{}, fail("no commit identity is configured")
		}
		if exists {
			return sandbox.Result{}, fail("the workspace already exists")
		}
		if _, err := b.cli(ctx, "volume", "create", "--label", "maestro.worker="+b.Worker, vol); err != nil {
			return sandbox.Result{}, fail("%v", err)
		}
		s.mirror = true
		s.argv = []string{"sh", "-c", sandbox.CloneScript, "clone", p.Branch, p.BaseBranch, b.BotName, b.BotEmail}
		res, err := b.start(ctx, s, nil, nil)
		if err == nil && res.ExitStatus == 0 && !sandbox.ValidCommit(strings.TrimSpace(string(res.Stdout))) {
			err = fail("the clone did not report a commit")
		}
		if err != nil || res.ExitStatus != 0 {
			// A partial workspace must not be found later.
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killTimeout)
			defer cancel()
			b.removeVolume(cctx, s.key)
		}
		return res, err

	case sandbox.OpFastForward:
		if !sandbox.ValidBranch(p.Branch) {
			return sandbox.Result{}, fail("invalid branch name")
		}
		if !exists {
			return sandbox.Result{}, fail("the workspace does not exist")
		}
		s.mirror = true
		if p.BaseBranch != "" && !sandbox.ValidBranch(p.BaseBranch) {
			return sandbox.Result{}, fail("invalid base branch name")
		}
		s.argv = []string{"sh", "-c", sandbox.FastForwardScript, "fast-forward", p.Branch, p.BaseBranch}
		return b.start(ctx, s, nil, nil)

	case sandbox.OpRemove:
		if err := b.removeVolume(ctx, s.key); err != nil {
			return sandbox.Result{}, err
		}
		os.RemoveAll(filepath.Join(b.HandoffDir, s.key))
		os.Remove(filepath.Join(b.HandoffDir, s.key+".bundle"))
		return sandbox.Result{}, nil
	}
	return sandbox.Result{}, fail("unknown populate operation")
}

// removeVolume deletes a workspace. Containers still using it are removed
// first. A workspace that does not exist is not an error.
func (b *Box) removeVolume(ctx context.Context, key string) error {
	vol := b.Volume(key)
	ids, err := b.cli(ctx, "ps", "--all", "--quiet", "--filter", "volume="+vol)
	if err != nil {
		return fail("%v", err)
	}
	for _, id := range strings.Fields(ids) {
		if _, err := b.cli(ctx, "rm", "--force", id); err != nil {
			return fail("%v", err)
		}
	}
	exists, err := b.volumeExists(ctx, vol)
	if err != nil {
		return fail("%v", err)
	}
	if !exists {
		return nil
	}
	if _, err := b.cli(ctx, "volume", "rm", "--force", vol); err != nil {
		return fail("%v", err)
	}
	return nil
}

// export runs the export step and moves the bundle to where the import
// step looks for it. The bundle is written by the sandbox into a directory
// that holds nothing else, and is opened later without following links.
func (b *Box) export(ctx context.Context, s spec) (sandbox.Result, error) {
	exists, err := b.volumeExists(ctx, b.Volume(s.key))
	if err != nil {
		return sandbox.Result{}, fail("%v", err)
	}
	if !exists {
		return sandbox.Result{}, fail("the workspace does not exist")
	}
	dir := filepath.Join(b.HandoffDir, s.key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return sandbox.Result{}, fail("hand-off directory: %v", err)
	}
	// The sandbox user is not this user, so the directory is opened up.
	// It contains only what the sandbox itself puts there.
	if err := os.Chmod(dir, 0o777); err != nil {
		return sandbox.Result{}, fail("hand-off directory: %v", err)
	}
	final := filepath.Join(b.HandoffDir, s.key+".bundle")
	os.Remove(final)
	s.handoff = dir
	if !identRE.MatchString(b.BotName) || !identRE.MatchString(b.BotEmail) {
		return sandbox.Result{}, fail("no commit identity is configured")
	}
	s.argv = []string{"sh", "-c", sandbox.ExportScript, "export", b.BotName, b.BotEmail}
	res, err := b.start(ctx, s, nil, nil)
	if err != nil || res.ExitStatus != 0 {
		return res, err
	}
	produced := filepath.Join(dir, bundleName)
	if _, err := os.Lstat(produced); err == nil {
		// A rename moves a link as a link; the import refuses anything
		// that is not a regular file.
		if err := os.Rename(produced, final); err != nil {
			return res, fail("hand-off: %v", err)
		}
	}
	return res, nil
}
