package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
	"github.com/iandeherdt/Maestro/internal/sandbox"
	"github.com/iandeherdt/Maestro/internal/sandbox/sandboxtest"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

// world is a scripted sandbox with a set of existing workspaces.
type world struct {
	exists   map[string]bool
	failOp   string // populate op that exits 1
	failHook string // hook that exits 1
	hangHook string // hook that never returns
	cloneOut string
}

func (w *world) handle(ctx context.Context, req sandbox.Request) (sandbox.Result, error) {
	if req.Step == sandbox.StepHook {
		switch req.Env["MAESTRO_HOOK"] {
		case w.failHook:
			return sandbox.Result{ExitStatus: 1, Stderr: []byte("npm ERR! @everyone")}, nil
		case w.hangHook:
			<-ctx.Done()
			return sandbox.Result{}, ctx.Err()
		}
		return sandbox.Result{}, nil
	}
	p := req.Populate
	if p.Op == w.failOp {
		return sandbox.Result{ExitStatus: 1, Stderr: []byte("fatal: no space left")}, nil
	}
	switch p.Op {
	case sandbox.OpProbe:
		if w.exists[req.WorkspaceKey] {
			return sandbox.Result{Stdout: []byte("exists " + sha + "\n")}, nil
		}
		return sandbox.Result{Stdout: []byte("missing\n")}, nil
	case sandbox.OpClone:
		w.exists[req.WorkspaceKey] = true
		out := w.cloneOut
		if out == "" {
			out = sha + "\n"
		}
		return sandbox.Result{Stdout: []byte(out)}, nil
	case sandbox.OpRemove:
		delete(w.exists, req.WorkspaceKey)
	}
	return sandbox.Result{}, nil
}

func setup(hooks config.Hooks) (*Manager, *sandboxtest.Fake, *world) {
	w := &world{exists: map[string]bool{}}
	f := &sandboxtest.Fake{Handler: w.handle}
	if hooks.TimeoutMS == 0 {
		hooks.TimeoutMS = 60000
	}
	cfg := &config.Config{Hooks: hooks,
		Publish:   config.Publish{BranchPrefix: "maestro/"},
		Workspace: config.Workspace{BaseBranch: "main"}}
	return New(cfg, f, nil), f, w
}

func gh(n string) issue.Issue { return issue.Issue{ID: n, Identifier: n} }

func TestCreateThenReuse(t *testing.T) {
	m, f, _ := setup(config.Hooks{AfterCreate: "npm ci"})
	ws, err := m.Ensure(context.Background(), gh("GH-7"))
	if err != nil {
		t.Fatal(err)
	}
	if !ws.CreatedNow || ws.Key != "GH-7" || ws.Branch != "maestro/GH-7" || ws.BaseSHA != sha {
		t.Fatalf("workspace = %+v", ws)
	}
	if got := f.Trace(); !reflect.DeepEqual(got, []string{"populate:probe", "populate:clone", "hook:after_create"}) {
		t.Fatalf("trace = %v", got)
	}
	clone := f.Calls()[1].Req
	if clone.Populate.Branch != "maestro/GH-7" || clone.Populate.BaseBranch != "main" || clone.NetworkProfile != sandbox.NetNone {
		t.Fatalf("clone request = %+v", clone)
	}

	ws, err = m.Ensure(context.Background(), gh("GH-7"))
	if err != nil {
		t.Fatal(err)
	}
	if ws.CreatedNow || ws.BaseSHA != sha {
		t.Fatalf("reused workspace = %+v", ws)
	}
	// after_create runs only on creation. Nothing destructive on reuse.
	if got := f.Trace()[3:]; !reflect.DeepEqual(got, []string{"populate:probe", "populate:fast-forward"}) {
		t.Fatalf("reuse trace = %v", got)
	}
}

func TestRequestsCarryNoPathsAndFixedHookShape(t *testing.T) {
	m, f, _ := setup(config.Hooks{AfterCreate: "make deps", BeforeRun: "make gen", AfterRun: "make clean", BeforeRemove: "true"})
	ctx := context.Background()
	if _, err := m.Ensure(ctx, gh("team/ABC 1")); err != nil {
		t.Fatal(err)
	}
	ws, _ := m.Ensure(ctx, gh("team/ABC 1"))
	if err := m.RunHook(ctx, BeforeRun, ws.Key); err != nil {
		t.Fatal(err)
	}
	m.AfterRunBestEffort(ctx, ws.Key)
	if err := m.Remove(ctx, "team/ABC 1"); err != nil {
		t.Fatal(err)
	}
	wantNet := map[string]string{AfterCreate: sandbox.NetHooks, BeforeRun: sandbox.NetHooks,
		AfterRun: sandbox.NetNone, BeforeRemove: sandbox.NetNone}
	seen := map[string]bool{}
	for _, c := range f.Calls() {
		raw, _ := json.Marshal(c.Req)
		if strings.Contains(string(raw), "/var") || strings.Contains(string(raw), "team/ABC") {
			t.Errorf("request carries a path or a raw identifier: %s", raw)
		}
		if strings.ContainsAny(c.Req.WorkspaceKey, "/ ") || c.Req.WorkspaceKey != ws.Key {
			t.Errorf("workspace key = %q", c.Req.WorkspaceKey)
		}
		if c.Req.Step != sandbox.StepHook {
			if c.Req.Argv != nil || c.Req.NetworkProfile != sandbox.NetNone {
				t.Errorf("populate request = %+v", c.Req)
			}
			continue
		}
		name := c.Req.Env["MAESTRO_HOOK"]
		seen[name] = true
		if len(c.Req.Argv) != 3 || c.Req.Argv[0] != "sh" || c.Req.Argv[1] != "-c" {
			t.Errorf("%s argv = %v", name, c.Req.Argv)
		}
		if c.Req.NetworkProfile != wantNet[name] {
			t.Errorf("%s network = %q", name, c.Req.NetworkProfile)
		}
		if c.Req.Limits.TimeoutMS != 60000 || len(c.Req.Env) != 1 {
			t.Errorf("%s limits/env = %+v %v", name, c.Req.Limits, c.Req.Env)
		}
	}
	if len(seen) != 4 {
		t.Errorf("hooks run = %v", seen)
	}
}

func TestInvalidIdentifierMakesNoSandboxCall(t *testing.T) {
	m, f, _ := setup(config.Hooks{})
	for _, id := range []string{"", ".", "..", "-rf", ".git", ".github"} {
		if _, err := m.Ensure(context.Background(), gh(id)); CategoryOf(err) != InvalidKey {
			t.Errorf("Ensure(%q) = %v", id, err)
		}
		if err := m.Remove(context.Background(), id); CategoryOf(err) != InvalidKey {
			t.Errorf("Remove(%q) = %v", id, err)
		}
	}
	if len(f.Calls()) != 0 {
		t.Fatalf("sandbox was called: %v", f.Trace())
	}
}

func TestCloneFailureRemovesPartialWorkspace(t *testing.T) {
	m, f, w := setup(config.Hooks{AfterCreate: "npm ci"})
	w.failOp = sandbox.OpClone
	_, err := m.Ensure(context.Background(), gh("GH-1"))
	if CategoryOf(err) != PopulateFailed {
		t.Fatalf("err = %v", err)
	}
	if got := f.Trace(); !reflect.DeepEqual(got, []string{"populate:probe", "populate:clone", "populate:remove"}) {
		t.Fatalf("trace = %v", got)
	}
}

func TestCloneWithBadBaseCommitIsRejected(t *testing.T) {
	for _, out := range []string{"\n", "HEAD\n", sha + "\n; rm -rf /\n", strings.ToUpper(sha)} {
		m, f, w := setup(config.Hooks{})
		w.cloneOut = out
		if _, err := m.Ensure(context.Background(), gh("GH-1")); CategoryOf(err) != PopulateFailed {
			t.Errorf("output %q: err = %v", out, err)
		}
		if tr := f.Trace(); tr[len(tr)-1] != "populate:remove" {
			t.Errorf("output %q: partial workspace kept: %v", out, tr)
		}
	}
}

func TestAfterCreateFailureIsFatalAndCleansUp(t *testing.T) {
	m, f, w := setup(config.Hooks{AfterCreate: "npm ci"})
	w.failHook = AfterCreate
	_, err := m.Ensure(context.Background(), gh("GH-1"))
	var we *Error
	if !errors.As(err, &we) || we.Category != HookFailed || !strings.Contains(we.Output, "npm ERR!") {
		t.Fatalf("err = %#v", err)
	}
	if w.exists["GH-1"] || f.Trace()[3] != "populate:remove" {
		t.Fatalf("workspace kept after a failed after_create: %v", f.Trace())
	}
}

func TestReuseFailureNeverRemoves(t *testing.T) {
	m, f, w := setup(config.Hooks{})
	w.exists["GH-1"] = true
	w.failOp = sandbox.OpFastForward
	if _, err := m.Ensure(context.Background(), gh("GH-1")); CategoryOf(err) != PopulateFailed {
		t.Fatalf("err = %v", err)
	}
	for _, s := range f.Trace() {
		if s == "populate:remove" || s == "populate:clone" {
			t.Fatalf("destructive step on a reused workspace: %v", f.Trace())
		}
	}
}

func TestUnexpectedProbeAnswer(t *testing.T) {
	m, f, _ := setup(config.Hooks{})
	f.Handler = func(context.Context, sandbox.Request) (sandbox.Result, error) {
		return sandbox.Result{Stdout: []byte("exists ../../etc")}, nil
	}
	if _, err := m.Ensure(context.Background(), gh("GH-1")); CategoryOf(err) != PopulateFailed {
		t.Fatalf("err = %v", err)
	}
	if len(f.Calls()) != 1 {
		t.Fatalf("continued after a bad probe: %v", f.Trace())
	}
}

func TestHookTimeout(t *testing.T) {
	m, _, w := setup(config.Hooks{BeforeRun: "sleep 999", TimeoutMS: 20})
	w.hangHook = BeforeRun
	if err := m.RunHook(context.Background(), BeforeRun, "GH-1"); CategoryOf(err) != HookTimeout {
		t.Fatalf("err = %v", err)
	}
}

func TestUnconfiguredHookIsSkipped(t *testing.T) {
	m, f, _ := setup(config.Hooks{BeforeRun: "  \n"})
	if err := m.RunHook(context.Background(), BeforeRun, "GH-1"); err != nil || len(f.Calls()) != 0 {
		t.Fatalf("err = %v, calls = %v", err, f.Trace())
	}
}

func TestBestEffortHooksDoNotFail(t *testing.T) {
	m, f, w := setup(config.Hooks{AfterRun: "false", BeforeRemove: "false"})
	w.exists["GH-1"] = true
	w.failHook = AfterRun
	m.AfterRunBestEffort(context.Background(), "GH-1") // must not panic or report
	w.failHook = BeforeRemove
	if err := m.Remove(context.Background(), "GH-1"); err != nil {
		t.Fatalf("remove failed because before_remove failed: %v", err)
	}
	if w.exists["GH-1"] {
		t.Fatalf("workspace not removed: %v", f.Trace())
	}
}

func TestRemoveMissingWorkspaceDoesNothing(t *testing.T) {
	m, f, _ := setup(config.Hooks{BeforeRemove: "true"})
	if err := m.Remove(context.Background(), "GH-9"); err != nil {
		t.Fatal(err)
	}
	if got := f.Trace(); !reflect.DeepEqual(got, []string{"populate:probe"}) {
		t.Fatalf("trace = %v", got)
	}
}

func TestLauncherErrorIsReported(t *testing.T) {
	m, f, _ := setup(config.Hooks{})
	f.Handler = func(context.Context, sandbox.Request) (sandbox.Result, error) {
		return sandbox.Result{}, &sandbox.Error{Msg: "launcher rejected the request"}
	}
	if _, err := m.Ensure(context.Background(), gh("GH-1")); CategoryOf(err) != sandbox.LaunchFailed {
		t.Fatalf("err = %v", err)
	}
}

func TestOutputTailIsCapped(t *testing.T) {
	out := tail([]byte(strings.Repeat("a", MaxLoggedOutput) + "the end\xff"))
	if len(out) > MaxLoggedOutput+1 || !strings.HasSuffix(out, "the end?") {
		t.Fatalf("tail len %d, suffix %q", len(out), out[len(out)-10:])
	}
}

// The scheduler removes workspaces through this interface.
var _ orchestrator.Workspaces = (*Manager)(nil)

func TestErrorShowsTheLastLineOfTheOutput(t *testing.T) {
	e := &Error{Category: PopulateFailed, Msg: "clone exited with status 128",
		Output: "Cloning\nfatal: 'origin/main' is not a commit\x1b[31m\n\n"}
	if got := e.Error(); got != "workspace_populate_failed: clone exited with status 128: fatal: 'origin/main' is not a commit [31m" {
		t.Errorf("%q", got)
	}
	if got := (&Error{Category: HookFailed, Msg: "x"}).Error(); got != "workspace_hook_failed: x" && got != HookFailed+": x" {
		t.Errorf("%q", got)
	}
	long := (&Error{Category: HookFailed, Msg: "x", Output: strings.Repeat("é", 1000)}).Error()
	if n := len([]rune(long)); n > 350 {
		t.Errorf("the line was not shortened: %d characters", n)
	}
}
