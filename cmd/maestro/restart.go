package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const restartUsage = `usage:
  maestro restart [--wait DURATION]   restart once the running agents are done
  maestro restart --now               restart at once; a running turn is cut off

A graceful restart first stops new tickets from starting, waits until no agent
is working any more, restarts the background service, and lets it pick up
tickets again. A ticket that waits for its pull request's checks is not an
agent at work: the restarted service goes on watching it. --wait is how long it
waits before it gives up and leaves the service as it was (default 2h, such as
45m or 3h). Ctrl-C also gives up without restarting.

--now restarts straight away. A turn that was running is cut off; its
workspace and commits stay, and the ticket is tried again.`

// drainPoll is how often a graceful restart looks at the running agents.
var drainPoll = 5 * time.Second

// pauseForRestart is the pause reason a graceful restart sets, and clears
// again once the service is back.
const pauseForRestart = "restarting: waiting for the running agents to finish"

func runRestart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "where setup keeps its state (default: the user's configuration directory)")
	file := fs.String("file", "", "the settings file (default: the one setup wrote)")
	now := fs.Bool("now", false, "restart at once, cutting off a running turn")
	wait := fs.Duration("wait", 2*time.Hour, "how long to wait for the running agents")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || *wait <= 0 {
		fmt.Fprintln(stderr, restartUsage)
		return 2
	}
	in, err := open(*dir, *file, nil)
	if err != nil {
		return report(stderr, err)
	}
	u, err := unitFor(in)
	if err != nil {
		return report(stderr, err)
	}
	if !u.Installed() {
		return report(stderr, explain("No background service is installed for this project. A `maestro run` in a terminal is restarted by stopping it with Ctrl-C and starting it again."))
	}
	ctx, stop := interruptible()
	defer stop()
	stateDirs := []string{in.cfg.Budget.StateDir}
	var resume []string
	if !*now {
		if resume, err = drain(ctx, stateDirs, *wait, stdout); err != nil {
			return report(stderr, err)
		}
	}
	pids := servicePIDs(stateDirs)
	if err := u.Restart(ctx); err != nil {
		resumeAll(resume, stdout)
		return report(stderr, err)
	}
	if *now {
		fmt.Fprintln(stdout, "Restarted. A turn that was running was cut off; the ticket is tried again.")
	} else {
		fmt.Fprintln(stdout, "Restarted.")
	}
	resumeAfterRestart(ctx, resume, pids, stdout)
	return 0
}

// drain stops new work in the services that answer and waits until none
// of them has an agent at work. It returns the services it paused, which
// the caller resumes once they are back. When it gives up, it resumes them
// itself and says why.
func drain(ctx context.Context, stateDirs []string, wait time.Duration, out io.Writer) ([]string, error) {
	var paused, up []string
	for _, d := range stateDirs {
		var s statusJSON
		if reach(ctx, d, http.MethodGet, "/api/v1/state", nil, &s) != nil {
			continue // not running: nothing to wait for
		}
		up = append(up, d)
		if s.Paused {
			continue // the owner's pause stays as it is
		}
		body, _ := json.Marshal(map[string]string{"reason": pauseForRestart})
		if err := reach(ctx, d, http.MethodPost, "/api/v1/pause", strings.NewReader(string(body)), nil); err != nil {
			resumeAll(paused, out)
			return nil, err
		}
		paused = append(paused, d)
	}
	deadline := time.Now().Add(wait)
	shown := ""
	for {
		var names []string
		for _, d := range up {
			var s statusJSON
			if reach(ctx, d, http.MethodGet, "/api/v1/state", nil, &s) != nil {
				continue // stopped meanwhile
			}
			for _, r := range s.Running {
				names = append(names, fmt.Sprintf("%s (%s, turn %d)", plain(r.Identifier, 30), plain(r.Phase, 30), r.TurnCount))
			}
		}
		if len(names) == 0 {
			return paused, nil
		}
		if now := strings.Join(names, ", "); now != shown {
			if shown == "" {
				fmt.Fprintln(out, "No new tickets are started. Waiting for the agents at work to finish (Ctrl-C to give up, or `maestro restart --now`):")
			}
			fmt.Fprintln(out, "  "+now)
			shown = now
		}
		if time.Now().After(deadline) {
			resumeAll(paused, out)
			return nil, explain("The agents were still at work after %s, so nothing was restarted and tickets start again as before. "+
				"Wait longer with `maestro restart --wait 4h`, or restart at once with `maestro restart --now`.", wait)
		}
		select {
		case <-ctx.Done():
			resumeAll(paused, out)
			return nil, explain("Given up: nothing was restarted, and tickets start again as before.")
		case <-time.After(drainPoll):
		}
	}
}

// resumeAll lifts the restart's pause where it set one. It runs after the
// caller's context may have ended, so it has its own.
func resumeAll(paused []string, out io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, d := range paused {
		if err := reach(ctx, d, http.MethodPost, "/api/v1/resume", nil, nil); err != nil {
			fmt.Fprintln(out, "The service could not be told to start tickets again. Run: maestro resume")
		}
	}
}

// servicePIDs records which process serves each project now, so that the
// pause is lifted in the new process and not in the old one on its way out.
func servicePIDs(stateDirs []string) map[string]int {
	pids := map[string]int{}
	for _, d := range stateDirs {
		if raw, err := os.ReadFile(filepath.Join(d, serviceFile)); err == nil {
			var info serviceInfo
			if json.Unmarshal(raw, &info) == nil {
				pids[d] = info.PID
			}
		}
	}
	return pids
}

// resumeAfterRestart waits until each restarted service answers and lifts
// the restart's pause there. The pause is kept across restarts, so without
// this the service would come back paused. A service this restart did not
// touch, such as a `maestro run` in a terminal, keeps its process: after a
// while it is resumed all the same.
func resumeAfterRestart(ctx context.Context, paused []string, before map[string]int, out io.Writer) {
	const tries = 60
	for _, d := range paused {
		ok := false
		for i := 0; i < tries && !ok && ctx.Err() == nil; i++ {
			fresh := servicePIDs([]string{d})[d] != before[d] || i >= tries/2
			var s statusJSON
			if fresh && reach(ctx, d, http.MethodGet, "/api/v1/state", nil, &s) == nil {
				if !s.Paused || s.PauseReason != pauseForRestart {
					ok = true // the owner changed it meanwhile; leave it
					break
				}
				ok = reach(ctx, d, http.MethodPost, "/api/v1/resume", nil, nil) == nil
			}
			if !ok {
				select {
				case <-ctx.Done():
				case <-time.After(drainPoll / 5 * 2):
				}
			}
		}
		if !ok {
			fmt.Fprintln(out, "The service did not answer after the restart, so it may still be paused. Check with `maestro status` and start tickets again with `maestro resume`.")
			continue
		}
		fmt.Fprintln(out, "The service is back and picks up tickets again.")
	}
}

// serviceStateDirs lists where every project on this machine keeps its
// state, so that an update can let each service finish its work.
func serviceStateDirs() []string {
	ps, err := listProjects()
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range ps {
		in, err := open(p.Dir, "", nil)
		if err != nil {
			continue
		}
		out = append(out, in.cfg.Budget.StateDir)
	}
	return out
}
