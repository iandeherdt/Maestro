// Package systemdbox is the service side of the systemd sandbox backend
// (SPEC 11, backend "systemd"). It sends each step to the root launcher,
// maestro-launch, through one sudo rule, and passes on what the step
// prints. The launcher decides every path, user and unit property; this
// side sends names only.
package systemdbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/iandeherdt/Maestro/internal/sandbox"
)

const (
	// Launcher is where setup installs the launcher.
	Launcher = "/usr/local/libexec/maestro-launch"
	// refused is the launcher's status for a request it refused or could
	// not run.
	refused = 125

	maxStdout = 1 << 20
	maxStderr = 64 << 10
	// stopTimeout is how long the launcher gets to stop the unit after
	// the step is cancelled, before it is killed.
	stopTimeout = 45 * time.Second
)

// Box implements sandbox.Launcher and sandbox.Streamer.
type Box struct {
	// Worker selects the launcher's configuration,
	// /etc/maestro/launch-<worker>.json.
	Worker string
	// Sudo and LauncherPath are the programs run. Defaults: sudo and
	// Launcher. Tests replace them.
	Sudo, LauncherPath string
}

func fail(format string, a ...any) error { return &sandbox.Error{Msg: fmt.Sprintf(format, a...)} }

func (b *Box) command(ctx context.Context) *exec.Cmd {
	sudo, launcher := b.Sudo, b.LauncherPath
	if sudo == "" {
		sudo = "sudo"
	}
	if launcher == "" {
		launcher = Launcher
	}
	// -n: never ask for a password; the rule allows exactly this command.
	cmd := exec.CommandContext(ctx, sudo, "-n", "--", launcher, b.Worker)
	// The launcher stops the unit when it is told to; it is killed only if
	// it does not finish in time.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = stopTimeout
	// Nothing of the service's environment is needed; sudo resets it
	// anyway.
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	return cmd
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
	if !sandbox.ValidWorker(b.Worker) {
		return sandbox.Result{}, fail("no valid worker name is configured")
	}
	// The launcher checks all of this again; checking here gives a clear
	// error before sudo runs.
	if !sandbox.ValidKey(req.WorkspaceKey) {
		return sandbox.Result{}, fail("invalid workspace key")
	}
	for k, v := range req.Env {
		if !sandbox.AllowedEnv(k, v) {
			return sandbox.Result{}, fail("environment variable %q is not allowed in the sandbox", k)
		}
	}
	line, err := json.Marshal(req)
	if err != nil {
		return sandbox.Result{}, fail("%v", err)
	}
	if req.Limits.TimeoutMS > 0 {
		// The unit has its own limit; this one also covers the launcher.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.Limits.TimeoutMS)*time.Millisecond+stopTimeout)
		defer cancel()
	}
	cmd := b.command(ctx)
	cmd.Stdin = io.MultiReader(bytes.NewReader(line), strings.NewReader("\n"), bytes.NewReader(stdin))
	outb := &capped{max: maxStdout}
	errb := &capped{max: maxStderr}
	if stdout != nil {
		cmd.Stdout = stdout
	} else {
		cmd.Stdout = outb
	}
	cmd.Stderr = errb
	err = cmd.Run()
	res := sandbox.Result{Stdout: outb.buf.Bytes(), Stderr: errb.buf.Bytes()}
	if ctx.Err() != nil {
		return sandbox.Result{}, ctx.Err()
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.ExitStatus = ee.ExitCode()
		if res.ExitStatus == refused {
			return res, fail("the launcher refused or could not run the step: %s", lastLine(res.Stderr))
		}
		if res.ExitStatus < 0 {
			return res, fail("the launcher was stopped by a signal")
		}
	default:
		return res, fail("sudo: %v", err)
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
