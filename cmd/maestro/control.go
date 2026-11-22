package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/background"
	"github.com/iandeherdt/Maestro/internal/httpapi"
	"github.com/iandeherdt/Maestro/internal/setup"
)

// serviceFile is where a running service says how to reach it. It holds a
// loopback address and a process ID, nothing secret.
const serviceFile = "service.json"

type serviceInfo struct {
	Addr string `json:"addr"`
	PID  int    `json:"pid"`
}

func writeServiceInfo(stateDir string, addr net.Addr) error {
	raw, _ := json.Marshal(serviceInfo{Addr: addr.String(), PID: os.Getpid()})
	return os.WriteFile(filepath.Join(stateDir, serviceFile), raw, 0o600)
}

// notRunning is the answer when no service can be reached.
const notRunning = "Maestro is not running on this machine. Start it with: maestro run"

// reach calls the running service. Only a loopback address from the
// service's own file is ever contacted.
func reach(ctx context.Context, stateDir, method, path string, body io.Reader, out any) error {
	raw, err := os.ReadFile(filepath.Join(stateDir, serviceFile))
	if err != nil {
		return explain(notRunning)
	}
	var info serviceInfo
	if json.Unmarshal(raw, &info) != nil {
		return explain(notRunning)
	}
	host, _, err := net.SplitHostPort(info.Addr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		return explain(notRunning)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+info.Addr+path, body)
	if err != nil {
		return err
	}
	req.Header.Set(httpapi.LocalHeader, "1")
	hc := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := hc.Do(req)
	if err != nil {
		// A file left behind by a service that was killed.
		return explain(notRunning)
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("the service answered %d", res.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out)
	}
	return nil
}

// plain makes text from the service safe to print: what an agent wrote can
// be part of it, and a terminal obeys control characters.
func plain(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return ' '
		}
		return r
	}, s)
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return strings.TrimSpace(s)
}

type statusJSON struct {
	Worker  string `json:"worker"`
	Running []struct {
		Identifier  string  `json:"issue_identifier"`
		Phase       string  `json:"phase"`
		Attempt     int     `json:"attempt"`
		TurnCount   int     `json:"turn_count"`
		LastMessage string  `json:"last_message"`
		StartedAt   *string `json:"started_at"`
	} `json:"running"`
	Retrying []struct {
		Identifier string  `json:"issue_identifier"`
		Kind       string  `json:"kind"`
		DueAt      *string `json:"due_at"`
		Error      string  `json:"error"`
	} `json:"retrying"`
	Awaiting []struct {
		Identifier string `json:"issue_identifier"`
		PRNumber   int    `json:"pr_number"`
	} `json:"awaiting_checks"`
	Budget struct {
		Runs    int64 `json:"runs_today"`
		Tokens  int64 `json:"tokens_today"`
		Seconds int64 `json:"agent_seconds_today"`
	} `json:"budget"`
	Paused      bool    `json:"paused"`
	PauseReason string  `json:"pause_reason"`
	QuotaUntil  *string `json:"quota_blocked_until"`
	ConfigError *string `json:"config_error"`
}

func since(stamp *string, now time.Time) string {
	if stamp == nil {
		return ""
	}
	t, err := time.Parse(time.RFC3339, *stamp)
	if err != nil {
		return ""
	}
	d := now.Sub(t).Round(time.Second)
	if d < 0 {
		return "in " + (-d).String()
	}
	return d.String() + " ago"
}

func printStatus(w io.Writer, s statusJSON, now time.Time) {
	fmt.Fprintf(w, "Maestro is running (worker %q).\n", plain(s.Worker, 40))
	switch {
	case s.Paused:
		fmt.Fprintf(w, "PAUSED: %s\n  No new tickets are started. Continue with: maestro resume\n", plain(s.PauseReason, 200))
	case s.QuotaUntil != nil:
		fmt.Fprintf(w, "Waiting for the Claude usage limit to reset (%s).\n", since(s.QuotaUntil, now))
	}
	if s.ConfigError != nil {
		fmt.Fprintf(w, "The settings file has an error, so no new tickets are started:\n  %s\n", plain(*s.ConfigError, 500))
	}
	if len(s.Running)+len(s.Retrying)+len(s.Awaiting) == 0 {
		fmt.Fprintln(w, "Nothing is being worked on.")
	}
	for _, r := range s.Running {
		fmt.Fprintf(w, "  %-8s working (%s, turn %d, started %s)\n", plain(r.Identifier, 30), plain(r.Phase, 30), r.TurnCount, since(r.StartedAt, now))
		if m := plain(r.LastMessage, 120); m != "" {
			fmt.Fprintf(w, "           last: %s\n", m)
		}
	}
	for _, r := range s.Awaiting {
		fmt.Fprintf(w, "  %-8s pull request #%d is open, waiting for its checks\n", plain(r.Identifier, 30), r.PRNumber)
	}
	for _, r := range s.Retrying {
		why := plain(r.Error, 80)
		if why == "" {
			why = plain(r.Kind, 30)
		}
		fmt.Fprintf(w, "  %-8s will be tried again %s (%s)\n", plain(r.Identifier, 30), since(r.DueAt, now), why)
	}
	fmt.Fprintf(w, "Today: %d agent run(s), %d tokens, %s of agent time.\n", s.Budget.Runs, s.Budget.Tokens,
		(time.Duration(s.Budget.Seconds) * time.Second).String())
}

// runControl is `maestro status`, `pause` and `resume`.
func runControl(cmd string, args []string, stdout, stderr io.Writer) int {
	// For pause, the words after the flags are the reason, so the settings
	// file can only be named with --file there.
	dir, explicit, rest, ok := parseWords(cmd, args, stderr, cmd == "pause")
	if !ok {
		return 2
	}
	reason := strings.Join(rest, " ")
	in, err := open(dir, explicit, nil)
	if err != nil {
		return report(stderr, err)
	}
	ctx, stop := interruptible()
	defer stop()
	stateDir := in.cfg.Budget.StateDir
	switch cmd {
	case "status":
		var s statusJSON
		if err := reach(ctx, stateDir, http.MethodGet, "/api/v1/state", nil, &s); err != nil {
			return report(stderr, err)
		}
		printStatus(stdout, s, time.Now())
	case "pause":
		body, _ := json.Marshal(map[string]string{"reason": reason})
		if err := reach(ctx, stateDir, http.MethodPost, "/api/v1/pause", strings.NewReader(string(body)), nil); err != nil {
			return report(stderr, err)
		}
		fmt.Fprintln(stdout, "Paused. Tickets that are running finish; no new ones are started.")
		fmt.Fprintln(stdout, "The pause stays, also after a restart, until you run: maestro resume")
	case "resume":
		if err := reach(ctx, stateDir, http.MethodPost, "/api/v1/resume", nil, nil); err != nil {
			return report(stderr, err)
		}
		fmt.Fprintln(stdout, "Resumed. The service is looking for tickets again.")
	}
	return 0
}

// alive reports whether a service for this installation answers.
func alive(ctx context.Context, stateDir string) bool {
	var s statusJSON
	return reach(ctx, stateDir, http.MethodGet, "/api/v1/state", nil, &s) == nil
}

// unitFor describes the background service of an installation.
func unitFor(in *install) (*background.Unit, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	file, err := filepath.Abs(in.path)
	if err != nil {
		return nil, err
	}
	return &background.Unit{GOOS: runtime.GOOS, Worker: in.cfg.Worker.Name, Binary: self, SetupDir: in.state.Dir(),
		WorkflowFile: file, LogFile: filepath.Join(in.cfg.Budget.StateDir, "service.log"), Home: home, UID: os.Getuid()}, nil
}

// runBackground is `maestro service install` and `maestro service uninstall`.
func runBackground(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "install" && args[0] != "uninstall") {
		fmt.Fprintln(stderr, "usage: maestro service install|uninstall [--dir DIR] [--file WORKFLOW.md]")
		return 2
	}
	dir, explicit, ok := parse("service "+args[0], args[1:], stderr)
	if !ok {
		return 2
	}
	in, err := open(dir, explicit, nil)
	if err != nil {
		return report(stderr, err)
	}
	u, err := unitFor(in)
	if err != nil {
		return report(stderr, err)
	}
	ctx, stop := interruptible()
	defer stop()
	ui := setup.Terminal(stdin, stdout)
	if args[0] == "uninstall" {
		if !u.Installed() {
			fmt.Fprintln(stdout, "No background service is installed.")
			return 0
		}
		if err := u.Uninstall(ctx); err != nil {
			return report(stderr, err)
		}
		fmt.Fprintf(stdout, "The background service is stopped and %s is removed.\n", u.Path())
		fmt.Fprintln(stdout, "Your settings, the workspaces and the Claude login are untouched. `maestro run` still works.")
		return 0
	}
	if err := in.docker(ctx); err != nil {
		return report(stderr, err)
	}
	if err := in.image(ctx); err != nil {
		return report(stderr, err)
	}
	if !u.Installed() && alive(ctx, in.cfg.Budget.StateDir) {
		fmt.Fprintln(stderr, "Maestro is running in a terminal right now. Stop it there with Ctrl-C, then run this command again.")
		return 1
	}
	text, err := u.Content()
	if err != nil {
		return report(stderr, err)
	}
	ui.Say("This makes Maestro run in the background, as you, without a terminal.")
	ui.Say("It starts when you log in and is started again if it stops. The agent still works in Docker,")
	ui.Say("so Docker has to be running; until it is, the service retries every 30 seconds.")
	ui.Say("")
	ui.Say("This file will be written to %s:", u.Path())
	ui.Say("")
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		ui.Say("    %s", line)
	}
	ui.Say("")
	if yes, err := ui.Confirm("Write it and start the service?"); err != nil || !yes {
		fmt.Fprintln(stderr, "Nothing was changed.")
		return 3
	}
	if err := u.Install(ctx); err != nil {
		return report(stderr, err)
	}
	// Give it a moment to come up, then say what is true.
	up := false
	for i := 0; i < 40 && !up && ctx.Err() == nil; i++ {
		time.Sleep(500 * time.Millisecond)
		up = alive(ctx, in.cfg.Budget.StateDir)
	}
	if up {
		ui.Say("The service is running in the background.")
	} else {
		ui.Say("The service is installed but has not come up yet. Look at its log for the reason.")
	}
	if runtime.GOOS == "darwin" {
		ui.Say("  Its log:        tail -f %q", u.LogFile)
	} else {
		ui.Say("  Its log:        journalctl --user -u %s -f", u.Label())
		ui.Say("  To keep it running while you are logged out: sudo loginctl enable-linger $USER")
	}
	ui.Say("  What it does:   maestro status")
	ui.Say("  To remove it:   maestro service uninstall")
	return 0
}
