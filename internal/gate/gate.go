// Package gate runs the steps between the agent's last turn and a pull
// request (SPEC 14 and 15.1): export, verify and publish. Export and verify
// run in the sandbox through the launcher; publish runs in the control
// zone. None of them is run by the agent.
package gate

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/hostcheck"
	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
	"github.com/iandeherdt/Maestro/internal/sandbox"
	"github.com/iandeherdt/Maestro/internal/worker"
	"github.com/iandeherdt/Maestro/internal/workspace"
)

// DirtiedTree is the reason given when the verify command changed files.
const DirtiedTree = "verify_dirtied_tree"

const exportTimeout = 2 * time.Minute

// Publisher is the publish gate.
type Publisher interface {
	Publish(ctx context.Context, is issue.Issue, ws workspace.Workspace, v worker.VerifyResult) (*orchestrator.PublishRecord, *worker.Rejection, error)
}

// HostChecks runs the unity pack's checks on this machine (unity.editor).
type HostChecks interface {
	Unity(ctx context.Context, editor, key, head, script string) (hostcheck.Result, error)
	Forget(key string)
}

// Gates implements worker.Gates.
type Gates struct {
	Config    func() *config.Config
	Launcher  sandbox.Launcher
	Publisher Publisher
	// Host runs the checks that do not run in the sandbox. Nil: there
	// are none on this machine.
	Host HostChecks

	mu    sync.Mutex
	heads map[string]string // workspace key -> head of the last export
}

var _ worker.Gates = (*Gates)(nil)

var exportRE = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64}) (\d{1,6})$`)

// export runs the export step. The launcher commits what is uncommitted,
// writes the bundle, and prints the head and the number of new commits.
// Both are produced in the sandbox, so only the exact shape is accepted;
// the bundle itself is checked again on import.
func (g *Gates) export(ctx context.Context, key string) (head string, commits int, err error) {
	ectx, cancel := context.WithTimeout(ctx, exportTimeout)
	defer cancel()
	res, err := g.Launcher.Run(ectx, sandbox.Request{
		Step: sandbox.StepExport, WorkspaceKey: key, NetworkProfile: sandbox.NetNone,
		Limits: sandbox.Limits{TimeoutMS: exportTimeout.Milliseconds()},
	}, nil)
	if err != nil {
		return "", 0, err
	}
	if res.ExitStatus != 0 {
		return "", 0, fmt.Errorf("the export step exited with status %d", res.ExitStatus)
	}
	m := exportRE.FindStringSubmatch(strings.TrimSpace(string(res.Stdout)))
	if m == nil {
		return "", 0, errors.New("the export step gave an answer that is not a commit and a count")
	}
	commits, _ = strconv.Atoi(m[2])
	g.mu.Lock()
	if g.heads == nil {
		g.heads = map[string]string{}
	}
	g.heads[key] = m[1]
	g.mu.Unlock()
	return m[1], commits, nil
}

// Export commits and bundles the workspace (SPEC 15.1).
func (g *Gates) Export(ctx context.Context, ws workspace.Workspace) (worker.Export, error) {
	_, commits, err := g.export(ctx, ws.Key)
	if err != nil {
		return worker.Export{}, err
	}
	return worker.Export{NoChanges: commits == 0}, nil
}

// Verify runs the operator's verify command against exactly what would be
// published (SPEC 14). A command that fails, runs too long or changes
// files does not pass.
func (g *Gates) Verify(ctx context.Context, ws workspace.Workspace) (worker.VerifyResult, error) {
	cfg := g.Config().Verify
	g.mu.Lock()
	before := g.heads[ws.Key]
	g.mu.Unlock()
	if before == "" {
		var err error
		if before, _, err = g.export(ctx, ws.Key); err != nil {
			return worker.VerifyResult{}, err
		}
	}
	network := sandbox.NetNone
	if cfg.Network {
		network = sandbox.NetHooks
	}
	timeout := time.Duration(cfg.TimeoutMS) * time.Millisecond
	vctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := g.Launcher.Run(vctx, sandbox.Request{
		Step: sandbox.StepVerify, WorkspaceKey: ws.Key,
		Argv:           []string{"sh", "-c", cfg.Command},
		NetworkProfile: network,
		Limits:         sandbox.Limits{TimeoutMS: cfg.TimeoutMS},
	}, nil)
	if vctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		return worker.VerifyResult{ExitStatus: -1,
			OutputTail: fmt.Sprintf("The verify command ran longer than %s and was stopped.", timeout)}, nil
	}
	if err != nil {
		return worker.VerifyResult{}, err
	}
	out := worker.VerifyResult{Passed: res.ExitStatus == 0, ExitStatus: res.ExitStatus,
		OutputTail: tail(append(res.Stdout, res.Stderr...), int(cfg.OutputTailBytes))}

	// Verify is expected to check, not to fix. Whatever it changed is
	// committed by this second export, so the next verify sees it, and the
	// agent is told.
	after, _, err := g.export(ctx, ws.Key)
	if err != nil {
		return worker.VerifyResult{}, err
	}
	if after != before {
		out.Passed = false
		out.OutputTail = DirtiedTree + ": the verify command changed files in the repository. " +
			"Commit the result of formatters and generators before finishing.\n" + out.OutputTail
	}
	if u := g.Config().Unity; out.Passed && u.Verify != "" {
		if g.Host == nil {
			return worker.VerifyResult{}, errors.New("unity.editor is set, but this service cannot run checks outside the sandbox")
		}
		hctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		res, err := g.Host.Unity(hctx, u.Editor, ws.Key, after, u.Verify)
		if hctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			return worker.VerifyResult{ExitStatus: -1, OutputTail: tail(append(res.Output,
				fmt.Sprintf("\nThe Unity checks ran longer than %s and were stopped.", timeout)...), int(cfg.OutputTailBytes))}, nil
		}
		if err != nil {
			return worker.VerifyResult{}, fmt.Errorf("unity checks on this machine: %w", err)
		}
		both := append([]byte(out.OutputTail), res.Output...)
		out = worker.VerifyResult{Passed: res.ExitStatus == 0, ExitStatus: res.ExitStatus,
			OutputTail: tail(both, int(cfg.OutputTailBytes))}
	}
	if out.Passed && g.Config().Screenshots.On() {
		out.Screenshots, out.ScreenshotNote = g.screenshots(ctx, ws.Key)
	}
	return out, nil
}

// Publish hands over to the publish gate.
func (g *Gates) Publish(ctx context.Context, is issue.Issue, ws workspace.Workspace, v worker.VerifyResult) (*orchestrator.PublishRecord, *worker.Rejection, error) {
	return g.Publisher.Publish(ctx, is, ws, v)
}

// Forget drops what is remembered about a workspace that was removed.
func (g *Gates) Forget(workspaceKey string) {
	g.mu.Lock()
	delete(g.heads, workspaceKey)
	g.mu.Unlock()
	if g.Host != nil {
		g.Host.Forget(workspaceKey)
	}
}

// tail returns the last max bytes of b as valid text.
func tail(b []byte, max int) string {
	if max > 0 && len(b) > max {
		b = b[len(b)-max:]
		for len(b) > 0 && !utf8.RuneStart(b[0]) {
			b = b[1:]
		}
	}
	return strings.ToValidUTF8(string(b), "?")
}
