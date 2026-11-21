// Package reload keeps the running configuration in step with the workflow
// file (SPEC section 6.2).
//
// The file is checked before every dispatch. A valid change is applied
// without a restart. An invalid file never replaces the last good
// configuration: the error is reported and dispatch waits until the file
// is fixed.
package reload

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"slices"

	"github.com/iandeherdt/Symphony-beads/internal/config"
	"github.com/iandeherdt/Symphony-beads/internal/prompt"
	"github.com/iandeherdt/Symphony-beads/internal/workflow"
)

// Loaded is one consistent view of the workflow file.
type Loaded struct {
	Config *config.Config
	Prompt *prompt.Builder
}

// Source watches one workflow file. It is not safe for concurrent use.
type Source struct {
	path    string
	opt     config.Options
	log     *slog.Logger
	current *Loaded
	hash    [sha256.Size]byte
	lastErr error
}

func build(path string, raw []byte, opt config.Options) (*Loaded, error) {
	def, err := workflow.Parse(raw)
	if err != nil {
		return nil, err
	}
	def.Path = path
	cfg, err := config.Build(def, opt)
	if err != nil {
		return nil, err
	}
	pb, err := prompt.New(def.PromptTemplate)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", prompt.RenderError, err)
	}
	return &Loaded{Config: cfg, Prompt: pb}, nil
}

func read(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, &workflow.Error{Class: workflow.ClassMissingFile, Msg: err.Error()}
	}
	return raw, nil
}

// Open loads the workflow at startup. Any error is fatal to startup.
func Open(path string, opt config.Options, log *slog.Logger) (*Source, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	raw, err := read(path)
	if err != nil {
		return nil, err
	}
	l, err := build(path, raw, opt)
	if err != nil {
		return nil, err
	}
	for _, w := range l.Config.Warnings {
		log.Warn("workflow", "warning", w)
	}
	return &Source{path: path, opt: opt, log: log, current: l, hash: sha256.Sum256(raw)}, nil
}

// Current returns the last good view.
func (s *Source) Current() *Loaded { return s.current }

// Check re-reads the file. It returns a new view when a valid change was
// applied, and nil otherwise. A non-nil error means the file is currently
// invalid; the last good view stays in force and the error repeats on
// every call until the file is fixed.
func (s *Source) Check() (*Loaded, error) {
	reject := func(err error) (*Loaded, error) {
		if s.lastErr == nil || s.lastErr.Error() != err.Error() {
			s.log.Error("workflow reload rejected, keeping the last good configuration", "error", err)
		}
		s.lastErr = err
		return nil, err
	}
	raw, err := read(s.path)
	if err != nil {
		s.hash = [sha256.Size]byte{}
		return reject(err)
	}
	sum := sha256.Sum256(raw)
	if sum == s.hash {
		return nil, s.lastErr
	}
	s.hash = sum
	l, err := build(s.path, raw, s.opt)
	if err != nil {
		return reject(err)
	}
	s.lastErr = nil
	for _, key := range keepRestartKeys(s.current.Config, l.Config) {
		s.log.Warn("workflow change ignored until restart", "key", key)
	}
	for _, w := range l.Config.Warnings {
		s.log.Warn("workflow", "warning", w)
	}
	s.current = l
	s.log.Info("workflow reloaded")
	return l, nil
}

// Preflight adapts Check to the orchestrator's per-tick hook.
func (s *Source) Preflight() (*config.Config, error) {
	l, err := s.Check()
	if l != nil {
		return l.Config, err
	}
	return nil, err
}

// keepRestartKeys copies the restart-required settings from the running
// configuration into the new one and returns the keys that had changed.
func keepRestartKeys(old, next *config.Config) []string {
	var ignored []string
	keep := func(key string, changed bool, restore func()) {
		if changed {
			ignored = append(ignored, key)
			restore()
		}
	}
	keep("workspace.root", old.Workspace.Root != next.Workspace.Root, func() { next.Workspace.Root = old.Workspace.Root })
	keep("workspace.repo", old.Workspace.Repo != next.Workspace.Repo, func() { next.Workspace.Repo = old.Workspace.Repo })
	keep("sandbox.backend", old.Sandbox.Backend != next.Sandbox.Backend, func() { next.Sandbox.Backend = old.Sandbox.Backend })
	keep("budget.state_dir", old.Budget.StateDir != next.Budget.StateDir, func() { next.Budget.StateDir = old.Budget.StateDir })
	keep("tracker.kind", old.Tracker.Kind != next.Tracker.Kind, func() {
		// The provider block belongs to the kind, so it stays as well.
		next.Tracker.Kind, next.Tracker.Provider = old.Tracker.Kind, old.Tracker.Provider
	})
	keep("worker.name", old.Worker.Name != next.Worker.Name, func() {
		// The routing label is derived from the name. Put the old one back,
		// or this worker would start taking another worker's issues.
		oldLabel, newLabel := "run:"+old.Worker.Name, "run:"+next.Worker.Name
		for i, l := range next.Tracker.RequiredLabels {
			if l == newLabel {
				next.Tracker.RequiredLabels[i] = oldLabel
			}
		}
		if slices.Contains(old.Tracker.RequiredLabels, oldLabel) && !slices.Contains(next.Tracker.RequiredLabels, oldLabel) {
			next.Tracker.RequiredLabels = append(next.Tracker.RequiredLabels, oldLabel)
		}
		next.Worker.Name = old.Worker.Name
	})
	portOf := func(c *config.Config) int64 {
		if c.Server.Port == nil {
			return -1
		}
		return *c.Server.Port
	}
	keep("server.port", portOf(old) != portOf(next), func() { next.Server.Port = old.Server.Port })
	return ignored
}
