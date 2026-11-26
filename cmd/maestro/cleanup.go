package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/iandeherdt/Maestro/internal/cleanup"
	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/dockerbox"
	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/logging"
	"github.com/iandeherdt/Maestro/internal/mirror"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
	"github.com/iandeherdt/Maestro/internal/publish"
	"github.com/iandeherdt/Maestro/internal/tracker"
	"github.com/iandeherdt/Maestro/internal/tracker/github"
	"github.com/iandeherdt/Maestro/internal/workspace"
)

// Automatic cleanup: a first sweep a few minutes after the start, when
// the first ticks have settled what is running, then a few times a day.
const (
	cleanupFirst = 5 * time.Minute
	cleanupEvery = 6 * time.Hour
)

// sweeper is the cleanup of one installation. One sweep runs at a time.
type sweeper struct {
	cfg      func() *config.Config
	trk      tracker.Tracker
	pub      *publish.Publisher
	mir      *mirror.Mirror
	mirrorMu *sync.Mutex
	spaces   orchestrator.Workspaces
	dbox     *dockerbox.Box
	auditDir string
	busy     func(identifier string) bool
	log      *slog.Logger
	// disk is the latest reading of the engine's free space.
	disk diskState
	mu   sync.Mutex
}

// cacheKeep is how long the build cache of a build stays when the disk is
// not nearly full.
const cacheKeep = 7 * 24 * time.Hour

// diskEvery is how often the service reads the engine's free space.
const diskEvery = 15 * time.Minute

// diskState is the latest reading of the container engine's free space.
type diskState struct {
	mu    sync.Mutex
	known bool
	free  int64
}

func (d *diskState) set(free int64) (wasLow bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	wasLow = d.known && d.free < dockerbox.LowDisk
	d.known, d.free = true, free
	return wasLow
}

func (d *diskState) low() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.known && d.free < dockerbox.LowDisk
}

// note is what a blocked ticket says about the disk: nothing while there
// is room, so that a note about it always means something.
func (d *diskState) note() string {
	if !d.low() {
		return ""
	}
	d.mu.Lock()
	free := d.free
	d.mu.Unlock()
	return lowDiskText(free)
}

// lowDiskText says that the engine's disk is nearly full, and what to do.
func lowDiskText(free int64) string {
	return fmt.Sprintf("The container engine on this machine is nearly out of disk space (%s free), so builds, "+
		"the sandbox and CI that runs here fail with \"no space left on device\". "+
		"`docker system df` shows what takes the room; `maestro cleanup` and `docker builder prune -a` free it.",
		cleanup.Size(free))
}

// watchDisk reads the engine's free space now and then. When it gets low,
// the service says so once, in the log and to the owner, and the build
// cache goes at once; it says so again only after there was room again.
func (s *sweeper) watchDisk(ctx context.Context, tell func(string)) {
	if s.dbox == nil {
		return
	}
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		t.Reset(diskEvery)
		c, cancel := context.WithTimeout(ctx, 2*time.Minute)
		free, err := s.dbox.FreeSpace(c)
		cancel()
		if err != nil && !errors.Is(err, dockerbox.ErrNoSpace) {
			s.log.Debug("free disk space not read", "error", err)
			continue
		}
		wasLow := s.disk.set(free)
		if free >= dockerbox.LowDisk || wasLow {
			continue
		}
		s.log.Warn("the container engine is nearly out of disk space", "free", cleanup.Size(free))
		if tell != nil {
			tell(lowDiskText(free))
		}
		c, cancel = context.WithTimeout(ctx, 30*time.Minute)
		if freed, err := s.dbox.PruneBuildCache(c, 0); err != nil {
			s.log.Warn("build cache not pruned", "error", err)
		} else if freed > 0 {
			s.log.Info("build cache pruned", "freed", cleanup.Size(freed))
		}
		cancel()
	}
}

// tidyAfterBuild removes what a new sandbox image replaced: the old image,
// and the build cache that the new build did not use within a day. Each
// rebuild otherwise leaves gigabytes behind, which on a Mac fill the
// engine's disk until images fail to unpack.
func tidyAfterBuild(ctx context.Context, box *dockerbox.Box, out io.Writer) {
	var freed int64
	if imgs, err := box.StaleImages(ctx); err == nil {
		for _, img := range imgs {
			_ = box.RemoveImage(ctx, img.ID)
		}
	}
	n, err := box.PruneBuildCache(ctx, 24*time.Hour)
	freed += n
	if err != nil {
		fmt.Fprintf(out, "The old build cache could not be removed: %s\n", plain(err.Error(), 300))
		return
	}
	if freed > 0 {
		fmt.Fprintf(out, "Removed %s of build cache that the new image does not need.\n", cleanup.Size(freed))
	}
}

func (s *sweeper) deps() cleanup.Deps {
	cfg := s.cfg()
	d := cleanup.Deps{
		AuditDir: s.auditDir,
		Busy:     s.busy,
		Log:      s.log,
		Finished: func(is issue.Issue) bool {
			for _, t := range cfg.Tracker.TerminalStates {
				if issue.NormalizeState(t) == issue.NormalizeState(is.State) {
					return true
				}
			}
			return false
		},
	}
	if l, ok := s.trk.(tracker.Lookup); ok {
		d.Lookup = l.LookupIdentifier
	}
	if s.pub != nil && s.mir != nil {
		d.Branches = s.mir.Branches
		d.BranchPrefix = cfg.Publish.BranchPrefix
		d.BaseBranch = cfg.Workspace.BaseBranch
		d.KeepBranches = []string{publish.ScreenshotBranch(cfg.Publish.BranchPrefix)}
		d.OpenChange = s.pub.OpenChange
		d.DeleteBranch = s.pub.DeleteBranch
	}
	if s.mir != nil {
		d.MirrorKeys = s.mir.WorkspaceKeys
		d.ForgetMirror = s.mir.ForgetWorkspace
		d.CompactMirror = func(ctx context.Context) error {
			if s.mirrorMu != nil {
				s.mirrorMu.Lock()
				defer s.mirrorMu.Unlock()
			}
			return s.mir.Compact(ctx)
		}
	}
	if s.spaces != nil {
		d.RemoveWorkspace = s.spaces.Remove
		if s.dbox != nil {
			d.Workspaces = s.dbox.WorkspaceKeys
		}
	}
	if s.dbox != nil {
		d.StaleImages = func(ctx context.Context) ([]cleanup.Image, error) {
			list, err := s.dbox.StaleImages(ctx)
			out := make([]cleanup.Image, 0, len(list))
			for _, img := range list {
				name := "unnamed sandbox image " + shortID(img.ID)
				if img.Size != "" {
					name += " (" + img.Size + ")"
				}
				out = append(out, cleanup.Image{ID: img.ID, Name: name})
			}
			return out, err
		}
		d.PruneBuildCache = func(ctx context.Context) (int64, error) {
			// Normally the cache of the last week stays, so rebuilds are
			// quick. With the disk nearly full, all of it goes.
			keep := cacheKeep
			if s.disk.low() {
				keep = 0
			}
			return s.dbox.PruneBuildCache(ctx, keep)
		}
		d.RemoveImage = func(ctx context.Context, id string) error {
			err := s.dbox.RemoveImage(ctx, id)
			if errors.Is(err, dockerbox.ErrImageInUse) {
				return cleanup.ErrKept
			}
			return err
		}
	}
	return d
}

// sweep runs one cleanup.
func (s *sweeper) sweep(ctx context.Context, dry bool) cleanup.Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cleanup.Run(ctx, s.deps(), dry)
}

// keepClean sweeps a few minutes after the start and then every few hours.
func (s *sweeper) keepClean(ctx context.Context) {
	t := time.NewTimer(cleanupFirst)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c, cancel := context.WithTimeout(ctx, 30*time.Minute)
		r := s.sweep(c, false)
		cancel()
		if len(r.Removed) > 0 || len(r.Problems) > 0 {
			s.log.Info("cleanup done", "removed", len(r.Removed)-r.Failed(), "failed", r.Failed(),
				"kept", len(r.Kept), "problems", len(r.Problems))
		}
		for _, p := range r.Problems {
			s.log.Warn("cleanup: " + p)
		}
		t.Reset(cleanupEvery)
	}
}

// busyIn says which tickets the orchestrator is working on, from its
// latest published state.
func busyIn(orch *orchestrator.Orchestrator) func(string) bool {
	return func(identifier string) bool {
		s := orch.Latest()
		for _, r := range s.Running {
			if r.Identifier == identifier {
				return true
			}
		}
		for _, r := range s.Retrying {
			if r.Identifier == identifier {
				return true
			}
		}
		for _, r := range s.AwaitingChecks {
			if r.Identifier == identifier {
				return true
			}
		}
		return false
	}
}

func shortID(id string) string {
	if len(id) > 7 && id[:7] == "sha256:" {
		id = id[7:]
	}
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

// runCleanup is `maestro cleanup`. A running service does the sweep
// itself, because it knows what it is working on; otherwise this command
// does it.
func runCleanup(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	d := fs.String("dir", "", "where setup keeps its state (default: the user's configuration directory)")
	f := fs.String("file", "", "the settings file (default: the one setup wrote)")
	dry := fs.Bool("dry-run", false, "only say what would be removed")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: maestro cleanup [--dry-run] [--dir DIR] [--file WORKFLOW.md]")
		return 2
	}
	ctx, stop := interruptible()
	defer stop()
	log := logging.New(io.Discard, slog.LevelInfo)
	in, err := open(*d, *f, log)
	if err != nil {
		return report(stderr, err)
	}
	var rep cleanup.Report
	path := "/api/v1/cleanup"
	if *dry {
		path += "?dry_run=1"
	}
	if alive(ctx, in.cfg.Budget.StateDir) {
		if err := reachFor(ctx, cleanupTimeoutCLI, in.cfg.Budget.StateDir, http.MethodPost, path, nil, &rep); err != nil {
			return report(stderr, err)
		}
	} else {
		if rep, err = cleanupHere(ctx, in, gitHub, log, *dry); err != nil {
			return report(stderr, err)
		}
	}
	writeCleanup(stdout, rep)
	if rep.Failed() > 0 {
		return 1
	}
	return 0
}

// cleanupTimeoutCLI is how long the command waits for the service's sweep.
const cleanupTimeoutCLI = 11 * time.Minute

// cleanupHere sweeps without the service. Nothing runs, so nothing is busy.
func cleanupHere(ctx context.Context, in *install, gh endpoints, log *slog.Logger, dry bool) (cleanup.Report, error) {
	cfg := in.cfg
	if _, err := exec.LookPath("git"); err != nil {
		return cleanup.Report{}, explain("the `git` command was not found on this machine. Install git and try again.")
	}
	if err := in.docker(ctx); err != nil {
		return cleanup.Report{}, err
	}
	cs, err := in.codeSide(ctx, gh, log)
	if err != nil {
		return cleanup.Report{}, err
	}
	var trk tracker.Tracker
	if cfg.Tracker.Kind == "linear" {
		la, _, err := in.linear(ctx, log)
		if err != nil {
			return cleanup.Report{}, err
		}
		trk = la
	} else {
		if cs.tickets == nil {
			return cleanup.Report{}, explain("the GitHub board needs the GitHub App, which this installation does not use")
		}
		ga, err := github.New(cfg, github.Options{Token: cs.tickets.Token, Log: log})
		if err != nil {
			return cleanup.Report{}, err
		}
		trk = ga
	}
	conf := func() *config.Config { return cfg }
	mir := &mirror.Mirror{Dir: in.mirrorDir, OwnerUID: -1, MaxBundleBytes: cfg.Publish.MaxBundleBytes, AllowFileRemote: gh.fileRemote}
	pub := &publish.Publisher{Config: conf, Mirror: mir, HandoffDir: in.handoffDir, Token: cs.code.Token,
		BotName: in.bot, BotEmail: in.botEmail(), BotLogin: in.bot, APIURL: gh.api, GitURL: gh.git}
	s := &sweeper{cfg: conf, trk: trk, pub: pub, mir: mir, spaces: workspace.New(cfg, in.box, log),
		dbox: in.dbox, auditDir: in.auditDir, log: log}
	return s.sweep(ctx, dry), nil
}

// writeCleanup prints a sweep's report for a person.
func writeCleanup(w io.Writer, r cleanup.Report) {
	verb := "Removed"
	if r.DryRun {
		verb = "Would remove"
	}
	if len(r.Removed) == 0 {
		fmt.Fprintln(w, "Nothing to clean up.")
	} else {
		fmt.Fprintf(w, "%s:\n", verb)
		var freed int64
		for _, it := range r.Removed {
			line := fmt.Sprintf("  %-9s %s: %s", it.Kind, plain(it.Name, 120), plain(it.Reason, 200))
			if it.Error != "" {
				line += " (failed: " + plain(it.Error, 300) + ")"
			} else {
				freed += it.Bytes
			}
			fmt.Fprintln(w, line)
		}
		if freed > 0 {
			fmt.Fprintf(w, "That frees %s.\n", cleanup.Size(freed))
		}
	}
	if len(r.Kept) > 0 {
		fmt.Fprintln(w, "Kept:")
		for _, it := range r.Kept {
			fmt.Fprintf(w, "  %-9s %s: %s\n", it.Kind, plain(it.Name, 120), plain(it.Reason, 300))
		}
	}
	for _, p := range r.Problems {
		fmt.Fprintln(w, "Note:", plain(p, 400))
	}
	if r.DryRun && len(r.Removed) > 0 {
		fmt.Fprintln(w, "Run it without --dry-run to remove these.")
	}
}
