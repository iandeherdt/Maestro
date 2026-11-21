// Package workspace manages per-issue workspaces (SPEC section 9).
//
// The manager runs in the control zone and never opens, stats or traverses
// a workspace. Every operation is a request to the sandbox launcher that
// names a workspace key. The key is the only thing derived from the issue,
// and it is restricted to [A-Za-z0-9._-].
package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/iandeherdt/Symphony-beads/internal/config"
	"github.com/iandeherdt/Symphony-beads/internal/issue"
	"github.com/iandeherdt/Symphony-beads/internal/sandbox"
)

// Error categories.
const (
	InvalidKey     = "workspace_invalid_key"
	PopulateFailed = "workspace_populate_failed"
	HookFailed     = "hook_failed"
	HookTimeout    = "hook_timeout"
)

// MaxLoggedOutput caps hook output in logs and errors.
const MaxLoggedOutput = 64 << 10

// populateTimeout bounds clone, probe, fast-forward and remove.
const populateTimeout = 10 * time.Minute

// Error is a workspace failure with a stable category.
type Error struct {
	Category string
	Msg      string
	// Output is the tail of what the failed step printed. Untrusted.
	Output string
}

func (e *Error) Error() string { return e.Category + ": " + e.Msg }

// CategoryOf returns the category of err: a workspace category,
// sandbox_launch_failed, or "".
func CategoryOf(err error) string {
	var we *Error
	if errors.As(err, &we) {
		return we.Category
	}
	var se *sandbox.Error
	if errors.As(err, &se) {
		return sandbox.LaunchFailed
	}
	return ""
}

// Workspace describes a prepared workspace (SPEC 4.1.4). It has no path:
// only the launcher knows where a workspace is.
type Workspace struct {
	Key        string
	Branch     string
	CreatedNow bool
	BaseSHA    string
}

// Hook names.
const (
	AfterCreate  = "after_create"
	BeforeRun    = "before_run"
	AfterRun     = "after_run"
	BeforeRemove = "before_remove"
)

// Manager creates, reuses and removes workspaces.
type Manager struct {
	Launcher     sandbox.Launcher
	Hooks        config.Hooks
	BranchPrefix string
	BaseBranch   string
	Log          *slog.Logger
}

func New(cfg *config.Config, l sandbox.Launcher, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{Launcher: l, Hooks: cfg.Hooks, BranchPrefix: cfg.Publish.BranchPrefix,
		BaseBranch: cfg.Workspace.BaseBranch, Log: log}
}

var shaRE = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

func key(identifier string) (string, error) {
	k, err := issue.WorkspaceKey(identifier)
	if err != nil {
		return "", &Error{Category: InvalidKey, Msg: fmt.Sprintf("identifier %q cannot name a workspace", identifier)}
	}
	return k, nil
}

func (m *Manager) populate(ctx context.Context, k string, p sandbox.Populate) (sandbox.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, populateTimeout)
	defer cancel()
	res, err := m.Launcher.Run(ctx, sandbox.Request{
		Step: sandbox.StepPopulate, WorkspaceKey: k, Populate: &p,
		NetworkProfile: sandbox.NetNone,
		Limits:         sandbox.Limits{TimeoutMS: populateTimeout.Milliseconds()},
	}, nil)
	if err != nil {
		return res, err
	}
	if res.ExitStatus != 0 {
		return res, &Error{Category: PopulateFailed,
			Msg:    fmt.Sprintf("%s exited with status %d", p.Op, res.ExitStatus),
			Output: tail(res.Stderr)}
	}
	return res, nil
}

// Ensure creates the workspace for an issue, or prepares the existing one
// for another attempt (SPEC 9.3).
func (m *Manager) Ensure(ctx context.Context, is issue.Issue) (Workspace, error) {
	k, err := key(is.Identifier)
	if err != nil {
		return Workspace{}, err
	}
	ws := Workspace{Key: k, Branch: m.BranchPrefix + k}

	res, err := m.populate(ctx, k, sandbox.Populate{Op: sandbox.OpProbe})
	if err != nil {
		return ws, err
	}
	fields := strings.Fields(string(res.Stdout))
	switch {
	case len(fields) == 1 && fields[0] == "missing":
		return m.create(ctx, ws)
	case len(fields) == 2 && fields[0] == "exists" && shaRE.MatchString(fields[1]):
		ws.BaseSHA = fields[1]
	default:
		return ws, &Error{Category: PopulateFailed, Msg: "probe gave an unexpected answer"}
	}

	// Reuse. Pick up work a previous process already published. This only
	// ever moves the branch forward; a reused workspace is never reset.
	if _, err := m.populate(ctx, k, sandbox.Populate{Op: sandbox.OpFastForward, Branch: ws.Branch}); err != nil {
		return ws, err
	}
	return ws, nil
}

func (m *Manager) create(ctx context.Context, ws Workspace) (Workspace, error) {
	ws.CreatedNow = true
	fail := func(err error) (Workspace, error) {
		// A partial workspace must not be mistaken for a usable one later.
		// The caller's context may already be over, so cleanup gets its own.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), populateTimeout)
		defer cancel()
		if _, rerr := m.populate(cctx, ws.Key, sandbox.Populate{Op: sandbox.OpRemove}); rerr != nil {
			m.Log.Error("partial workspace not removed", "workspace", ws.Key, "error", rerr)
		}
		return ws, err
	}
	res, err := m.populate(ctx, ws.Key, sandbox.Populate{Op: sandbox.OpClone, Branch: ws.Branch, BaseBranch: m.BaseBranch})
	if err != nil {
		return fail(err)
	}
	sha := strings.TrimSpace(string(res.Stdout))
	if !shaRE.MatchString(sha) {
		return fail(&Error{Category: PopulateFailed, Msg: "clone did not report a base commit"})
	}
	ws.BaseSHA = sha
	if err := m.RunHook(ctx, AfterCreate, ws.Key); err != nil {
		return fail(err)
	}
	return ws, nil
}

func (m *Manager) script(name string) string {
	switch name {
	case AfterCreate:
		return m.Hooks.AfterCreate
	case BeforeRun:
		return m.Hooks.BeforeRun
	case AfterRun:
		return m.Hooks.AfterRun
	case BeforeRemove:
		return m.Hooks.BeforeRemove
	}
	return ""
}

// RunHook runs a configured hook in the sandbox, with the workspace as its
// working directory. A hook that is not configured succeeds. The caller
// decides whether a failure matters (SPEC 9.4).
func (m *Manager) RunHook(ctx context.Context, name, workspaceKey string) error {
	script := m.script(name)
	if strings.TrimSpace(script) == "" {
		return nil
	}
	// Only the hooks that prepare a run may fetch dependencies.
	network := sandbox.NetNone
	if name == AfterCreate || name == BeforeRun {
		network = sandbox.NetHooks
	}
	timeout := time.Duration(m.Hooks.TimeoutMS) * time.Millisecond
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := m.Launcher.Run(hctx, sandbox.Request{
		Step: sandbox.StepHook, WorkspaceKey: workspaceKey,
		Argv:           []string{"sh", "-c", script},
		NetworkProfile: network,
		Limits:         sandbox.Limits{TimeoutMS: m.Hooks.TimeoutMS},
		Env:            map[string]string{"SYMPHONY_HOOK": name},
	}, nil)
	if hctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		return &Error{Category: HookTimeout, Msg: fmt.Sprintf("%s ran longer than %s", name, timeout)}
	}
	if err != nil {
		return err
	}
	if res.ExitStatus != 0 {
		return &Error{Category: HookFailed, Msg: fmt.Sprintf("%s exited with status %d", name, res.ExitStatus),
			Output: tail(append(res.Stdout, res.Stderr...))}
	}
	return nil
}

// AfterRunBestEffort runs after_run and only logs a failure.
func (m *Manager) AfterRunBestEffort(ctx context.Context, workspaceKey string) {
	if err := m.RunHook(ctx, AfterRun, workspaceKey); err != nil {
		m.Log.Warn("after_run hook failed, ignored", "workspace", workspaceKey, "error", err)
	}
}

// Remove deletes the workspace of an issue, if there is one. A failing
// before_remove hook does not stop the removal.
func (m *Manager) Remove(ctx context.Context, identifier string) error {
	k, err := key(identifier)
	if err != nil {
		return err
	}
	res, err := m.populate(ctx, k, sandbox.Populate{Op: sandbox.OpProbe})
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(res.Stdout)) == "missing" {
		return nil
	}
	if err := m.RunHook(ctx, BeforeRemove, k); err != nil {
		m.Log.Warn("before_remove hook failed, ignored", "workspace", k, "error", err)
	}
	_, err = m.populate(ctx, k, sandbox.Populate{Op: sandbox.OpRemove})
	return err
}

// tail keeps the end of step output, where the error usually is.
func tail(b []byte) string {
	if len(b) > MaxLoggedOutput {
		b = b[len(b)-MaxLoggedOutput:]
	}
	return strings.ToValidUTF8(string(b), "?")
}
