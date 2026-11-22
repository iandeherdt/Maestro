package gate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/hostcheck"
	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
	"github.com/iandeherdt/Maestro/internal/sandbox"
	"github.com/iandeherdt/Maestro/internal/sandbox/sandboxtest"
	"github.com/iandeherdt/Maestro/internal/worker"
	"github.com/iandeherdt/Maestro/internal/workspace"
)

const (
	headA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	headB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var (
	ctx = context.Background()
	ws  = workspace.Workspace{Key: "GH-1", Branch: "maestro/GH-1"}
)

type rig struct {
	g       *Gates
	sb      *sandboxtest.Fake
	cfg     *config.Config
	exports []string // answers of successive export steps
	verify  sandbox.Result
	hang    bool
}

func newRig(exports ...string) *rig {
	r := &rig{cfg: &config.Config{Verify: config.Verify{Command: "make check", TimeoutMS: 60000, OutputTailBytes: 20}}, exports: exports}
	n := 0
	r.sb = &sandboxtest.Fake{Handler: func(c context.Context, req sandbox.Request) (sandbox.Result, error) {
		switch req.Step {
		case sandbox.StepExport:
			out := r.exports[min(n, len(r.exports)-1)]
			n++
			return sandbox.Result{Stdout: []byte(out + "\n")}, nil
		case sandbox.StepVerify:
			if r.hang {
				<-c.Done()
				return sandbox.Result{}, c.Err()
			}
			return r.verify, nil
		}
		return sandbox.Result{}, errors.New("unexpected step")
	}}
	r.g = &Gates{Config: func() *config.Config { return r.cfg }, Launcher: r.sb}
	return r
}

func TestExport(t *testing.T) {
	for answer, want := range map[string]string{
		headA + " 3":                  "changes",
		headA + " 0":                  "none",
		strings.ToUpper(headA) + " 1": "error",
		headA:                         "error",
		headA + " -1":                 "error",
		headA + " 1\nextra":           "error",
		"":                            "error",
	} {
		r := newRig(answer)
		exp, err := r.g.Export(ctx, ws)
		got := "changes"
		if err != nil {
			got = "error"
		} else if exp.NoChanges {
			got = "none"
		}
		if got != want {
			t.Errorf("%q: %s, want %s", answer, got, want)
		}
	}
	r := newRig(headA + " 1")
	r.g.Export(ctx, ws)
	c := r.sb.Calls()[0].Req
	if c.Step != sandbox.StepExport || c.WorkspaceKey != "GH-1" || c.NetworkProfile != sandbox.NetNone || len(c.Argv) != 0 || len(c.Env) != 0 {
		t.Fatalf("request = %+v", c)
	}
	// A failing export step is an error, not "no changes".
	r.sb.Handler = func(context.Context, sandbox.Request) (sandbox.Result, error) {
		return sandbox.Result{ExitStatus: 1, Stdout: []byte(headA + " 0")}, nil
	}
	if _, err := r.g.Export(ctx, ws); err == nil {
		t.Fatal("a failed export was accepted")
	}
}

func TestVerifyPassesAndFails(t *testing.T) {
	r := newRig(headA + " 2")
	r.g.Export(ctx, ws)
	v, err := r.g.Verify(ctx, ws)
	if err != nil || !v.Passed || v.ExitStatus != 0 {
		t.Fatalf("%+v, %v", v, err)
	}
	c := r.sb.Calls()[1].Req
	if c.Step != sandbox.StepVerify || strings.Join(c.Argv, "|") != "sh|-c|make check" || c.NetworkProfile != sandbox.NetNone || c.Limits.TimeoutMS != 60000 {
		t.Fatalf("request = %+v", c)
	}
	if got := strings.Join(r.sb.Trace(), " "); got != "export verify export" {
		t.Fatalf("steps = %s", got)
	}

	r.verify = sandbox.Result{ExitStatus: 2, Stdout: []byte("0123456789 lots of output é"), Stderr: []byte("FAIL TestLogin")}
	r.cfg.Verify.Network = true
	v, err = r.g.Verify(ctx, ws)
	if err != nil || v.Passed || v.ExitStatus != 2 || !strings.HasSuffix(v.OutputTail, "FAIL TestLogin") || len(v.OutputTail) > 20 {
		t.Fatalf("%+v, %v", v, err)
	}
	calls := r.sb.Calls()
	if calls[len(calls)-2].Req.NetworkProfile != sandbox.NetHooks {
		t.Fatal("verify.network was not applied")
	}
}

func TestVerifyThatChangesFilesFails(t *testing.T) {
	r := newRig(headA+" 2", headB+" 3")
	r.g.Export(ctx, ws)
	v, err := r.g.Verify(ctx, ws)
	if err != nil || v.Passed || !strings.HasPrefix(v.OutputTail, DirtiedTree) {
		t.Fatalf("%+v, %v", v, err)
	}
	// The new head is the baseline for the next verify.
	if v, _ := r.g.Verify(ctx, ws); !v.Passed {
		t.Fatalf("second verify: %+v", v)
	}
}

func TestVerifyExportsFirstWhenNothingIsKnown(t *testing.T) {
	r := newRig(headA + " 1")
	if v, err := r.g.Verify(ctx, ws); err != nil || !v.Passed {
		t.Fatalf("%+v, %v", v, err)
	}
	if got := strings.Join(r.sb.Trace(), " "); got != "export verify export" {
		t.Fatalf("steps = %s", got)
	}
	r.g.Forget("GH-1")
	r.g.Verify(ctx, ws)
	if got := len(r.sb.Calls()); got != 6 {
		t.Fatalf("%d steps after forgetting", got)
	}
}

func TestVerifyTimeoutIsAFailureNotAnError(t *testing.T) {
	r := newRig(headA + " 1")
	r.hang = true
	r.cfg.Verify.TimeoutMS = 30
	v, err := r.g.Verify(ctx, ws)
	if err != nil || v.Passed || !strings.Contains(v.OutputTail, "ran longer than") {
		t.Fatalf("%+v, %v", v, err)
	}
	// Cancelled from outside: an error.
	r.cfg.Verify.TimeoutMS = 60000
	c, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, err := r.g.Verify(c, ws); err == nil {
		t.Fatal("a cancelled verify reported a result")
	}
}

type fakePublisher struct{ called bool }

func (p *fakePublisher) Publish(context.Context, issue.Issue, workspace.Workspace, worker.VerifyResult) (*orchestrator.PublishRecord, *worker.Rejection, error) {
	p.called = true
	return &orchestrator.PublishRecord{PRNumber: 5}, nil, nil
}

func TestPublishDelegates(t *testing.T) {
	r := newRig(headA + " 1")
	p := &fakePublisher{}
	r.g.Publisher = p
	rec, _, err := r.g.Publish(ctx, issue.Issue{}, ws, worker.VerifyResult{Passed: true})
	if err != nil || rec.PRNumber != 5 || !p.called {
		t.Fatalf("%+v, %v", rec, err)
	}
}

func TestTailKeepsValidText(t *testing.T) {
	if got := tail([]byte("ééé"), 3); got != "é" {
		t.Fatalf("%q", got)
	}
	if got := tail([]byte("abc"), 0); got != "abc" {
		t.Fatalf("%q", got)
	}
}

type fakeHost struct {
	calls  []string
	res    hostcheck.Result
	err    error
	forgot []string
}

func (h *fakeHost) Unity(_ context.Context, editor, key, head, script string) (hostcheck.Result, error) {
	h.calls = append(h.calls, editor+"|"+key+"|"+head+"|"+script)
	return h.res, h.err
}

func (h *fakeHost) Forget(key string) { h.forgot = append(h.forgot, key) }

func TestUnityChecksRunOnTheMachineAfterTheSandbox(t *testing.T) {
	r := newRig(headA + " 2")
	r.cfg.Verify.OutputTailBytes = 1000
	r.cfg.Unity = config.Unity{Editor: "/x/Unity", Verify: "sh conventions/unity/verify.sh"}
	h := &fakeHost{res: hostcheck.Result{Output: []byte("3 tests passed.")}}
	r.g.Host = h
	r.verify = sandbox.Result{Stdout: []byte("sandbox ok\n")}
	v, err := r.g.Verify(ctx, ws)
	if err != nil || !v.Passed || v.OutputTail != "sandbox ok\n3 tests passed." {
		t.Fatalf("%+v, %v", v, err)
	}
	if len(h.calls) != 1 || h.calls[0] != "/x/Unity|GH-1|"+headA+"|sh conventions/unity/verify.sh" {
		t.Fatalf("calls %v", h.calls)
	}

	h.res = hostcheck.Result{ExitStatus: 1, Output: []byte("FAILED HealthTests.Heal")}
	if v, err := r.g.Verify(ctx, ws); err != nil || v.Passed || v.ExitStatus != 1 || !strings.HasSuffix(v.OutputTail, "FAILED HealthTests.Heal") {
		t.Fatalf("%+v, %v", v, err)
	}

	// A sandbox that fails already does not start Unity.
	r.verify = sandbox.Result{ExitStatus: 2}
	r.g.Verify(ctx, ws)
	if len(h.calls) != 2 {
		t.Fatalf("calls %v", h.calls)
	}

	// A problem of the machine is an error, not the agent's failure.
	r.verify = sandbox.Result{}
	h.err = errors.New("Unity Hub has no licence")
	if _, err := r.g.Verify(ctx, ws); err == nil {
		t.Fatal("no error")
	}
	r.g.Host = nil
	if _, err := r.g.Verify(ctx, ws); err == nil {
		t.Fatal("no error without a host")
	}

	r.g.Host = h
	r.g.Forget("GH-1")
	if len(h.forgot) != 1 {
		t.Fatal("not forgotten")
	}
}
