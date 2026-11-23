package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/iandeherdt/Maestro/internal/sandbox"
	"github.com/iandeherdt/Maestro/internal/systemdbox"
)

// sandboxBox is what the service needs of a sandbox backend: the
// container one (dockerbox) or systemd (systemdbox).
type sandboxBox interface {
	sandbox.Launcher
	sandbox.Streamer
	Up(ctx context.Context, allowDir string) error
	Down(ctx context.Context) error
	ProxyRunning(ctx context.Context, profile string) bool
	Login(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error
	LoggedIn(ctx context.Context, command string) (bool, error)
	SelfTest(ctx context.Context) (sandbox.SelfTestReport, error)
}

// launcherHelp is said when the root launcher of the systemd backend is
// not there or not allowed.
const launcherHelp = "The systemd sandbox runs every step through a small root launcher, %s, which this user may\n" +
	"start through one sudo rule. %s\n" +
	"Install it, with the sandbox user and the rule, with: %s"

// systemdReady checks that the launcher is installed and that sudo lets
// this user run it without a password, without running it.
func (in *install) systemdReady(ctx context.Context) error {
	if _, err := os.Stat(systemdbox.Launcher); err != nil {
		return explain(launcherHelp, systemdbox.Launcher, "It is not installed.", mention("maestro setup"))
	}
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// -l with a command only says whether the rule allows it.
	if err := exec.CommandContext(c, "sudo", "-n", "-l", systemdbox.Launcher, in.cfg.Worker.Name).Run(); err != nil {
		return explain(launcherHelp, systemdbox.Launcher, "sudo does not allow it for this user without a password.", mention("maestro setup"))
	}
	return nil
}
