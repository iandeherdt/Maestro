// Package background installs the service so that it runs without a
// terminal and starts again after a login or a restart: a launchd agent on
// macOS, a systemd user unit on Linux.
//
// Both run as the person who installs them, not as root. The service needs
// nothing more: the sandbox is Docker's.
package background

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Unit describes the installed service.
type Unit struct {
	GOOS string
	// Worker names the installation; it is part of the unit's name.
	Worker string
	// Binary, SetupDir and WorkflowFile are absolute paths.
	Binary, SetupDir, WorkflowFile string
	// LogFile receives the service's output (macOS; systemd keeps a journal).
	LogFile string
	// Home is the user's home directory and UID their numeric ID.
	Home string
	UID  int
	// Run executes a command. Tests replace it.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

var workerRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

func (u *Unit) check() error {
	if !workerRE.MatchString(u.Worker) {
		return errors.New("the worker name cannot be used in a service name")
	}
	for _, p := range []string{u.Binary, u.SetupDir, u.WorkflowFile, u.Home} {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, "\x00\n\r") {
			return fmt.Errorf("%q is not a usable absolute path", p)
		}
	}
	if u.GOOS == "darwin" && !filepath.IsAbs(u.LogFile) {
		return errors.New("the log file must be an absolute path")
	}
	if u.GOOS != "darwin" && u.GOOS != "linux" {
		return errors.New("running in the background is supported on macOS and Linux")
	}
	return nil
}

func (u *Unit) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if u.Run != nil {
		return u.Run(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Label is the launchd label, and the systemd unit name without ".service".
func (u *Unit) Label() string {
	if u.GOOS == "darwin" {
		return "com.maestro." + u.Worker
	}
	return "maestro-" + u.Worker
}

// oldLabel is the label from when the command was called symphony. A
// service installed under it is replaced or removed like the current one.
func (u *Unit) oldLabel() string {
	if u.GOOS == "darwin" {
		return "com.symphony-beads." + u.Worker
	}
	return "symphony-" + u.Worker
}

func (u *Unit) pathOf(label string) string {
	if u.GOOS == "darwin" {
		return filepath.Join(u.Home, "Library", "LaunchAgents", label+".plist")
	}
	return filepath.Join(u.Home, ".config", "systemd", "user", label+".service")
}

// dropOld stops and removes a service installed under the old name. It
// reports whether there was one.
func (u *Unit) dropOld(ctx context.Context) (bool, error) {
	old := u.pathOf(u.oldLabel())
	if _, err := os.Stat(old); err != nil {
		return false, nil
	}
	if u.GOOS == "darwin" {
		u.run(ctx, "launchctl", "bootout", u.domain()+"/"+u.oldLabel())
	} else {
		u.run(ctx, "systemctl", "--user", "disable", "--now", u.oldLabel()+".service")
	}
	if err := os.Remove(old); err != nil && !os.IsNotExist(err) {
		return true, err
	}
	return true, nil
}

// Path is the file that describes the service to the system.
func (u *Unit) Path() string {
	if u.GOOS == "darwin" {
		return filepath.Join(u.Home, "Library", "LaunchAgents", u.Label()+".plist")
	}
	return filepath.Join(u.Home, ".config", "systemd", "user", u.Label()+".service")
}

func esc(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// path is the PATH the service gets. A background service does not have
// the shell's PATH, and it has to find docker and git.
const path = "/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin:/Applications/Docker.app/Contents/Resources/bin"

// quote writes one word of a systemd command line.
func quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "%", "%%")
	s = strings.ReplaceAll(s, "$", "$$")
	return `"` + s + `"`
}

// Content is the text of the file at Path.
func (u *Unit) Content() (string, error) {
	if err := u.check(); err != nil {
		return "", err
	}
	args := []string{u.Binary, "run", "--dir", u.SetupDir, "--file", u.WorkflowFile}
	if u.GOOS == "linux" {
		words := make([]string, len(args))
		for i, a := range args {
			words[i] = quote(a)
		}
		return "[Unit]\nDescription=Maestro (" + u.Worker + ")\nAfter=network-online.target docker.service\n\n" +
			"[Service]\nExecStart=" + strings.Join(words, " ") + "\n" +
			"Restart=always\nRestartSec=30\nEnvironment=PATH=" + strings.TrimSuffix(path, ":/Applications/Docker.app/Contents/Resources/bin") + "\n\n" +
			"[Install]\nWantedBy=default.target\n", nil
	}
	var a strings.Builder
	for _, s := range args {
		a.WriteString("    <string>" + esc(s) + "</string>\n")
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + esc(u.Label()) + `</string>
  <key>ProgramArguments</key>
  <array>
` + a.String() + `  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>` + path + `</string>
    <key>HOME</key>
    <string>` + esc(u.Home) + `</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>30</integer>
  <key>ProcessType</key>
  <string>Background</string>
  <key>StandardOutPath</key>
  <string>` + esc(u.LogFile) + `</string>
  <key>StandardErrorPath</key>
  <string>` + esc(u.LogFile) + `</string>
</dict>
</plist>
`, nil
}

func (u *Unit) domain() string { return "gui/" + strconv.Itoa(u.UID) }

// Installed reports whether the unit's file exists, under this name or
// the old one.
func (u *Unit) Installed() bool {
	if _, err := os.Stat(u.Path()); err == nil {
		return true
	}
	_, err := os.Stat(u.pathOf(u.oldLabel()))
	return err == nil
}

// Install writes the file and starts the service. An installed service is
// replaced, so that a new binary path or settings file takes effect.
func (u *Unit) Install(ctx context.Context) error {
	text, err := u.Content()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(u.Path()), 0o755); err != nil {
		return err
	}
	if _, err := u.dropOld(ctx); err != nil {
		return err
	}
	if u.GOOS == "darwin" {
		if err := os.MkdirAll(filepath.Dir(u.LogFile), 0o700); err != nil {
			return err
		}
		// Not loaded is fine here.
		u.run(ctx, "launchctl", "bootout", u.domain()+"/"+u.Label())
	}
	if err := os.WriteFile(u.Path(), []byte(text), 0o644); err != nil {
		return err
	}
	var steps [][]string
	if u.GOOS == "darwin" {
		steps = [][]string{{"launchctl", "bootstrap", u.domain(), u.Path()}}
	} else {
		steps = [][]string{{"systemctl", "--user", "daemon-reload"},
			{"systemctl", "--user", "enable", u.Label() + ".service"},
			{"systemctl", "--user", "restart", u.Label() + ".service"}}
	}
	for _, s := range steps {
		if out, err := u.run(ctx, s[0], s[1:]...); err != nil {
			return fmt.Errorf("%s: %w: %s", strings.Join(s, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// Uninstall stops the service and removes the file. Nothing else is
// removed: settings, workspaces and the login stay.
func (u *Unit) Uninstall(ctx context.Context) error {
	if err := u.check(); err != nil {
		return err
	}
	if _, err := u.dropOld(ctx); err != nil {
		return err
	}
	if u.GOOS == "darwin" {
		u.run(ctx, "launchctl", "bootout", u.domain()+"/"+u.Label())
	} else {
		u.run(ctx, "systemctl", "--user", "disable", "--now", u.Label()+".service")
	}
	if err := os.Remove(u.Path()); err != nil && !os.IsNotExist(err) {
		return err
	}
	if u.GOOS == "linux" {
		u.run(ctx, "systemctl", "--user", "daemon-reload")
	}
	return nil
}

// Restart restarts every installed service of this user whose file runs
// binary, so that a replaced binary takes effect. It returns the labels it
// restarted. run is for tests; nil runs the command.
func Restart(ctx context.Context, goos, home string, uid int, binary string,
	run func(ctx context.Context, name string, args ...string) ([]byte, error)) ([]string, error) {
	u := &Unit{GOOS: goos, Home: home, UID: uid, Run: run}
	var pattern string
	switch goos {
	case "darwin":
		pattern = filepath.Join(home, "Library", "LaunchAgents", "com.maestro.*.plist")
	case "linux":
		pattern = filepath.Join(home, ".config", "systemd", "user", "maestro-*.service")
	default:
		return nil, nil
	}
	files, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	var done []string
	for _, f := range files {
		text, err := os.ReadFile(f)
		want := quote(binary)
		if goos == "darwin" {
			want = esc(binary)
		}
		if err != nil || !strings.Contains(string(text), want) {
			continue
		}
		label := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(f), ".plist"), ".service")
		var argv []string
		if goos == "darwin" {
			argv = []string{"launchctl", "kickstart", "-k", u.domain() + "/" + label}
		} else {
			argv = []string{"systemctl", "--user", "restart", label + ".service"}
		}
		if out, err := u.run(ctx, argv[0], argv[1:]...); err != nil {
			return done, fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
		}
		done = append(done, label)
	}
	return done, nil
}
