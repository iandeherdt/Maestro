package dockerbox

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"

	"github.com/iandeherdt/Maestro/internal/sandbox"
)

// The owner's own sessions in the sandbox: a shell, and Claude Code on a
// terminal. They run with the same isolation as the service's steps. A
// session is started by the owner at this machine's terminal and is never
// asked for by anything that runs in the sandbox.

// View chooses what an owner's session can touch.
type View string

const (
	// ViewChecks is what the repository's own scripts, hooks and verify
	// get: the workspace, the hooks network and the Unity licence. No
	// Claude Code login.
	ViewChecks View = "checks"
	// ViewAgent is what the agent's step gets: the workspace, the agent
	// network, the Claude Code login and the owner's skills. No Unity
	// licence.
	ViewAgent View = "agent"
)

// stepShell labels the containers of the owner's sessions.
const stepShell sandbox.Step = "shell"

// Session describes one interactive session.
type Session struct {
	// Key is the workspace. Empty is a scratch workspace that is removed
	// when the session ends.
	Key  string
	View View
	// Argv is the command. Empty is a login shell.
	Argv []string
	// Env is added to the environment, under the same rules as a step's.
	Env map[string]string
}

// shellArgv starts bash where the image has it, and sh otherwise.
var shellArgv = []string{"/bin/sh", "-c", `if command -v bash >/dev/null 2>&1; then exec bash -l; fi; exec sh -l`}

// Workspaces lists the keys of the workspaces that exist.
func (b *Box) Workspaces(ctx context.Context) ([]string, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	out, err := b.cli(ctx, "volume", "ls", "--quiet", "--filter", "label=maestro.worker="+b.Worker)
	if err != nil {
		return nil, fail("%v", err)
	}
	prefix := b.Volume("")
	var keys []string
	for _, v := range strings.Fields(out) {
		k, ok := strings.CutPrefix(v, prefix)
		if ok && sandbox.ValidKey(k) && !strings.HasPrefix(k, "selftest-") && !strings.HasPrefix(k, "scratch-") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// Container is a running container of a workspace.
type Container struct {
	Name string
	Step string
}

// Running lists the containers that run on a workspace now.
func (b *Box) Running(ctx context.Context, key string) ([]Container, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	if !sandbox.ValidKey(key) {
		return nil, fail("invalid workspace key")
	}
	out, err := b.cli(ctx, "ps", "--filter", "label=maestro.worker="+b.Worker,
		"--filter", "label=maestro.workspace="+key, "--format", `{{.Names}}	{{.Label "maestro.step"}}`)
	if err != nil {
		return nil, fail("%v", err)
	}
	return parseRunning(out, b.prefix()+"-"), nil
}

func parseRunning(out, prefix string) []Container {
	var cs []Container
	for _, l := range strings.Split(out, "\n") {
		name, step, _ := strings.Cut(strings.TrimSpace(l), "\t")
		if strings.HasPrefix(name, prefix) {
			cs = append(cs, Container{Name: name, Step: step})
		}
	}
	return cs
}

// Join opens a shell in a container that runs now, as the sandbox user
// and in the container's own working directory. It gets no more than the
// step already has.
func (b *Box) Join(ctx context.Context, name string, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := b.check(); err != nil {
		return err
	}
	if !strings.HasPrefix(name, b.prefix()+"-") || strings.ContainsAny(name, " \t\n") {
		return fail("not a container of this worker")
	}
	args := append([]string{"exec", "--interactive", "--tty", "--user", AgentUID + ":" + AgentUID,
		"--env", "TERM=xterm-256color", name}, shellArgv...)
	cmd := exec.CommandContext(ctx, b.docker(), args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}

// Interactive runs a session on the caller's terminal in a new container.
// The proxies must be up (Up). It returns the command's exit status.
func (b *Box) Interactive(ctx context.Context, sess Session, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	s, cleanup, err := b.session(ctx, sess)
	if err != nil {
		return 0, err
	}
	defer cleanup()
	name := b.prefix() + "-" + strings.ToLower(s.key) + "-" + string(s.step) + "-" + token()
	cmd := exec.Command(b.docker(), b.runArgs(name, s)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if err := cmd.Start(); err != nil {
		return 0, fail("docker could not be started: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		b.remove(name)
		<-done
		return 0, ctx.Err()
	}
	b.remove(name)
	if ee, ok := err.(*exec.ExitError); ok {
		if ee.ExitCode() == 125 {
			return 125, fail("the container could not be started")
		}
		return ee.ExitCode(), nil
	}
	if err != nil {
		return 0, fail("docker: %v", err)
	}
	return 0, nil
}

// session turns a Session into the spec of its container, with the same
// checks a step gets.
func (b *Box) session(ctx context.Context, sess Session) (spec, func(), error) {
	none := func() {}
	if err := b.check(); err != nil {
		return spec{}, none, err
	}
	profile := sandbox.NetHooks
	switch sess.View {
	case ViewChecks:
	case ViewAgent:
		profile = sandbox.NetAgent
	default:
		return spec{}, none, fail("unknown view")
	}
	env := b.baseEnv(profile)
	env["TERM"] = "xterm-256color"
	delete(env, "CI")
	for k, v := range sess.Env {
		if !sandbox.AllowedEnv(k, v) {
			return spec{}, none, fail("environment variable %q is not allowed in the sandbox", k)
		}
		if _, fixed := env[k]; fixed {
			continue
		}
		env[k] = v
	}
	argv := sess.Argv
	if len(argv) == 0 {
		argv = shellArgv
	}
	if argv[0] == "" || strings.HasPrefix(argv[0], "-") {
		return spec{}, none, fail("the session has no command")
	}
	s := spec{step: stepShell, key: sess.Key, argv: argv, workdir: repoDir, network: profile, env: env, terminal: true}
	if sess.View == ViewAgent {
		s.claude = true
		env["CLAUDE_CONFIG_DIR"] = claudeDir
	} else {
		s.licence = b.UnityLicenceDir != ""
	}
	if sess.Key == "" {
		s.key, s.workdir = "scratch-"+token(), workDir
		vol := b.Volume(s.key)
		if _, err := b.cli(ctx, "volume", "create", "--label", "maestro.worker="+b.Worker, vol); err != nil {
			return spec{}, none, fail("%v", err)
		}
		return s, func() {
			c, cancel := context.WithTimeout(context.Background(), killTimeout)
			defer cancel()
			b.cli(c, "volume", "rm", "--force", vol)
		}, nil
	}
	if !sandbox.ValidKey(sess.Key) {
		return spec{}, none, fail("invalid workspace key")
	}
	ok, err := b.volumeExists(ctx, b.Volume(sess.Key))
	if err != nil {
		return spec{}, none, fail("%v", err)
	}
	if !ok {
		return spec{}, none, fail("the workspace %s does not exist", sess.Key)
	}
	return s, none, nil
}

// String names a view for the person at the terminal.
func (v View) String() string {
	switch v {
	case ViewAgent:
		return "the agent's view: its Claude Code login and network, no Unity licence"
	case ViewChecks:
		return "the checks' view: the hooks network and the Unity licence, no Claude Code login"
	}
	return fmt.Sprintf("view %q", string(v))
}
