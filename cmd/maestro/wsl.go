package main

import (
	"context"
	"os/exec"
	"os/user"
	"runtime"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/setup"
	"github.com/iandeherdt/Maestro/internal/wsl"
)

// wslDockerHint says how WSL gets a container engine.
const wslDockerHint = "In WSL, either start Docker Desktop on Windows and turn on Settings, Resources, WSL integration " +
	"for this distribution, or install Docker Engine or Podman inside the distribution. Then try again."

// wslHost tells a WSL distribution apart; tests replace it.
var wslHost = wsl.Host{}

// runWindows runs a Windows program from inside WSL; tests replace it.
var runWindows = func(ctx context.Context, argv []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
}

func inWSL() bool { return runtime.GOOS == "linux" && wslHost.Version() > 0 }

// wslReady says what stops the service from running in this WSL
// distribution, before anything is written.
func (in *install) wslReady() []string {
	if !inWSL() {
		return nil
	}
	return setup.WSLProblems(wslHost, absAll(in.state.Dir(), in.cfg.Budget.StateDir, in.cfg.Workspace.Root)...)
}

// wslAfterInstall makes the installed service survive in WSL: systemd has
// to start it without a login, and Windows has to start the distribution
// and keep it running.
func wslAfterInstall(ctx context.Context, ui *setup.UI, worker string) {
	if !inWSL() {
		return
	}
	ui.Say("")
	ui.Say("This is WSL. Windows stops the distribution soon after its last terminal closes, and does")
	ui.Say("not start it when Windows starts, so two more things keep the service running:")
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	if wslHost.Lingering(name) {
		ui.Say("  1. systemd starts the service without a login. That is already on.")
	} else {
		ui.Say("  1. Let systemd start the service without a login. Run, in this terminal:")
		ui.Say("       sudo loginctl enable-linger $USER")
	}
	argv := wsl.KeepAlive(wslHost.Distro(), worker)
	if argv == nil {
		ui.Say("  2. A Windows task that starts this distribution at login. Its name (%q) cannot be", wslHost.Distro())
		ui.Say("     put in a command safely, so create one in Task Scheduler that runs at log on:")
		ui.Say("       wsl.exe -d <distribution> --exec /bin/sleep infinity")
		return
	}
	line := wsl.Shell(argv)
	ui.Say("  2. A Windows task, %q, that starts this distribution without a window when you log", wsl.TaskName(worker))
	ui.Say("     in to Windows and keeps it running:")
	ui.Say("       %s", line)
	if yes, err := ui.Confirm("Create that Windows task now?"); err != nil || !yes {
		ui.Say("  Not created. To do it later, paste the line above into PowerShell or Command Prompt.")
		return
	}
	if out, err := runWindows(ctx, argv); err != nil {
		ui.Say("  It could not be created from here (%s).", oneLine(string(out), err))
		ui.Say("  Paste the line above into PowerShell or Command Prompt on Windows instead.")
		return
	}
	ui.Say("  The task is created. Until you next log in to Windows, keep a WSL terminal open,")
	ui.Say("  or start it now in a Windows terminal: schtasks.exe /Run /TN \"%s\"", wsl.TaskName(worker))
}

// wslAfterUninstall removes the Windows task again, if there is one.
func wslAfterUninstall(ctx context.Context, say func(string), worker string) {
	if !inWSL() {
		return
	}
	if _, err := runWindows(ctx, wsl.RemoveKeepAlive(worker)); err == nil {
		say("The Windows task " + `"` + wsl.TaskName(worker) + `"` + " that kept WSL running is removed.")
		return
	}
	say("If you created the Windows task \"" + wsl.TaskName(worker) + "\", remove it in a Windows terminal: " +
		wsl.Shell(wsl.RemoveKeepAlive(worker)))
}

func oneLine(out string, err error) string {
	out = strings.Join(strings.Fields(out), " ")
	if out == "" {
		out = err.Error()
	}
	if len(out) > 200 {
		out = out[:200] + "..."
	}
	return out
}
