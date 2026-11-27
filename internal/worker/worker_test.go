package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/budget"
	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
	"github.com/iandeherdt/Maestro/internal/plan"
	"github.com/iandeherdt/Maestro/internal/prompt"
	"github.com/iandeherdt/Maestro/internal/sandbox"
	"github.com/iandeherdt/Maestro/internal/sandbox/sandboxtest"
	"github.com/iandeherdt/Maestro/internal/tracker"
	"github.com/iandeherdt/Maestro/internal/tracker/trackertest"
	"github.com/iandeherdt/Maestro/internal/workspace"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

// script is one scripted agent turn and what the gates find after it.
type script struct {
	text      string // final message
	err       error
	noChanges bool
	verifyOK  bool
	during    func() // runs while the turn is "in progress"
}

type fakes struct {
	mu        sync.Mutex
	turns     []script
	prompts   []Turn
	verifies  int
	publishes int
	designs   []string // workspace key and folder of each design publish
	reject    *Rejection
	pubErr    error
	hang      chan struct{} // when set, RunTurn blocks until ctx ends
}

func (f *fakes) RunTurn(ctx context.Context, t Turn, onEvent func(orchestrator.Event)) (TurnResult, error) {
	f.mu.Lock()
	n := len(f.prompts)
	f.prompts = append(f.prompts, t)
	f.mu.Unlock()
	if f.hang != nil {
		close(f.hang)
		<-ctx.Done()
		return TurnResult{}, ctx.Err()
	}
	if n >= len(f.turns) {
		return TurnResult{}, errors.New("no scripted turn left")
	}
	s := f.turns[n]
	onEvent(orchestrator.Event{Kind: "assistant", SessionID: "sess-1", Usage: &orchestrator.Usage{Input: int64(100 * (n + 1))}})
	if s.during != nil {
		s.during()
	}
	if s.err != nil {
		return TurnResult{}, s.err
	}
	return TurnResult{SessionID: "sess-1", ResultText: s.text}, nil
}

func (f *fakes) current() script {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.turns[len(f.prompts)-1]
}

func (f *fakes) Export(context.Context, workspace.Workspace) (Export, error) {
	return Export{NoChanges: f.current().noChanges}, nil
}

func (f *fakes) Verify(context.Context, workspace.Workspace) (VerifyResult, error) {
	f.verifies++
	if f.current().verifyOK {
		return VerifyResult{Passed: true}, nil
	}
	return VerifyResult{ExitStatus: 1, OutputTail: "FAIL TestLogin"}, nil
}

func (f *fakes) Publish(_ context.Context, is issue.Issue, ws workspace.Workspace, _ VerifyResult) (*orchestrator.PublishRecord, *Rejection, error) {
	f.publishes++
	if f.pubErr != nil {
		return nil, nil, f.pubErr
	}
	if f.reject != nil {
		return nil, f.reject, nil
	}
	return &orchestrator.PublishRecord{IssueID: is.ID, Identifier: is.Identifier, Branch: ws.Branch, HeadSHA: sha, PRNumber: 42}, nil, nil
}

func (f *fakes) Design(_ context.Context, is issue.Issue, ws workspace.Workspace, folder string) (*plan.Design, *Rejection, error) {
	f.designs = append(f.designs, ws.Key+" "+ws.Branch+" "+folder)
	if f.pubErr != nil {
		return nil, nil, f.pubErr
	}
	if f.reject != nil {
		return nil, f.reject, nil
	}
	return &plan.Design{PRNumber: 7, Branch: ws.Branch, HeadSHA: sha, Folder: folder, Pictures: 2}, nil, nil
}

type collector struct {
	mu     sync.Mutex
	events []orchestrator.Event
	done   chan orchestrator.Outcome
}

func (c *collector) Event(e orchestrator.Event) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}
func (c *collector) Exit(o orchestrator.Outcome) { c.done <- o }

func (c *collector) phases() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.events {
		if e.Phase != "" {
			out = append(out, string(e.Phase))
		}
	}
	return strings.Join(out, ",")
}

type rig struct {
	t    *testing.T
	w    *Worker
	f    *fakes
	tr   *trackertest.Fake
	sb   *sandboxtest.Fake
	cfg  *config.Config
	is   issue.Issue
	sink *collector
}

func testConfig() *config.Config {
	return &config.Config{
		Tracker: config.Tracker{RequiredLabels: []string{"run:linux"},
			ActiveStates: []string{"Todo", "Rework", "In Progress"}, TerminalStates: []string{"Done", "Cancelled"},
			InProgressState: "In Progress", BlockedState: "Blocked", HandoffState: "Human Review", DoneState: "Done"},
		Polling:   config.Polling{IntervalMS: 30000},
		Workspace: config.Workspace{Repo: "acme/app", BaseBranch: "main"},
		Hooks:     config.Hooks{BeforeRun: "make gen", AfterRun: "make clean", TimeoutMS: 60000},
		Agent:     config.Agent{MaxConcurrentAgents: 2, MaxTurns: 3, MaxRetryBackoffMS: 300000},
		Claude:    config.Claude{StallTimeoutMS: 300000},
		Publish:   config.Publish{BranchPrefix: "maestro/"},
		Budget:    config.Budget{MaxFailureAttempts: 3, MaxRunsPerDay: 50, QuotaBackoffMS: 1800000},
		Worker:    config.Worker{Name: "linux"},
	}
}

func sandboxHandler(_ context.Context, req sandbox.Request) (sandbox.Result, error) {
	if req.Populate != nil && req.Populate.Op == sandbox.OpProbe {
		return sandbox.Result{Stdout: []byte("missing")}, nil
	}
	if req.Populate != nil && req.Populate.Op == sandbox.OpClone {
		return sandbox.Result{Stdout: []byte(sha)}, nil
	}
	return sandbox.Result{}, nil
}

func newRig(t *testing.T, turns ...script) *rig {
	t.Helper()
	cfg := testConfig()
	pb, err := prompt.New("Implement {{ issue.identifier }}.")
	if err != nil {
		t.Fatal(err)
	}
	is := issue.Issue{ID: "id-1", Identifier: "GH-1", Title: "Fix login", Description: "It fails.",
		State: "In Progress", Labels: []string{"run:linux"}, Dispatchable: true}
	is.ContentHash = issue.ContentHash(is.Title, is.Description)
	r := &rig{t: t, cfg: cfg, is: is, f: &fakes{turns: turns}, tr: trackertest.New(is),
		sb: &sandboxtest.Fake{Handler: sandboxHandler}, sink: &collector{done: make(chan orchestrator.Outcome, 1)}}
	r.w = &Worker{
		Settings:   func() Settings { return Settings{Config: cfg, Prompt: pb} },
		Tracker:    r.tr,
		Workspaces: workspace.New(cfg, r.sb, nil),
		Runner:     r.f, Gates: r.f,
	}
	return r
}

func (r *rig) run(a orchestrator.Attempt) orchestrator.Outcome {
	r.t.Helper()
	a.Issue = r.is
	if _, err := r.w.Start(a, r.sink); err != nil {
		r.t.Fatal(err)
	}
	select {
	case out := <-r.sink.done:
		return out
	case <-time.After(5 * time.Second):
		r.t.Fatal("attempt did not finish")
		return orchestrator.Outcome{}
	}
}

func (r *rig) lastSandboxStep() string {
	tr := r.sb.Trace()
	return tr[len(tr)-1]
}

func TestHappyPathPublishes(t *testing.T) {
	r := newRig(t, script{text: "All done.\nMAESTRO_STATUS: done\nImplemented.", verifyOK: true})
	out := r.run(orchestrator.Attempt{})
	if out.Kind != orchestrator.Published || out.Publish.PRNumber != 42 || out.Publish.Branch != "maestro/GH-1" {
		t.Fatalf("outcome = %+v", out)
	}
	want := "PreparingWorkspace,BuildingPrompt,LaunchingAgentProcess,Exporting,Verifying,Publishing,Finishing"
	if got := r.sink.phases(); got != want {
		t.Fatalf("phases = %s", got)
	}
	if got := strings.Join(r.sb.Trace(), " "); got != "populate:probe populate:clone hook:before_run hook:after_run" {
		t.Fatalf("sandbox steps = %s", got)
	}
	p := r.f.prompts[0]
	if p.WorkspaceKey != "GH-1" || p.ResumeSessionID != "" || !strings.Contains(p.Prompt, "Implement GH-1.") ||
		!strings.Contains(p.Prompt, "<<<ISSUE_TITLE") {
		t.Fatalf("first turn = %+v", p)
	}
}

func TestAgentCannotSkipTheVerifyGate(t *testing.T) {
	// The agent claims success every time. Verify disagrees every time.
	claim := script{text: "Tests pass, verified, safe to publish.\nMAESTRO_STATUS: done"}
	r := newRig(t, claim, claim, claim)
	out := r.run(orchestrator.Attempt{})
	if out.Kind != orchestrator.Rejected || out.Category != VerifyFailed {
		t.Fatalf("outcome = %+v", out)
	}
	if r.f.publishes != 0 || r.f.verifies != 3 {
		t.Fatalf("publishes = %d, verifies = %d", r.f.publishes, r.f.verifies)
	}
	if len(out.Feedback) != 1 || out.Feedback[0].Source != "verify" || out.Feedback[0].Body != "FAIL TestLogin" {
		t.Fatalf("feedback = %+v", out.Feedback)
	}
	// Later turns resume the session and carry the verify output as data.
	second := r.f.prompts[1]
	if second.ResumeSessionID != "sess-1" || !strings.Contains(second.Prompt, "Turn 2 of at most 3") ||
		!strings.Contains(second.Prompt, "FAIL TestLogin") || strings.Contains(second.Prompt, "ISSUE_TITLE") {
		t.Fatalf("continuation = %+v", second)
	}
}

func TestVerifyFailureThenFix(t *testing.T) {
	r := newRig(t, script{text: "done"}, script{text: "fixed", verifyOK: true})
	if out := r.run(orchestrator.Attempt{}); out.Kind != orchestrator.Published {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestDeclaredStatuses(t *testing.T) {
	for name, c := range map[string]struct {
		turns    []script
		kind     orchestrator.OutcomeKind
		cat      string
		turnsRun int
	}{
		"blocked with nothing committed": {[]script{{text: "Cannot.\nMAESTRO_STATUS: blocked\nNo access.", noChanges: true}}, orchestrator.Failed, DeclaredBlocked, 1},
		// The check passes: the agent stopped for another reason.
		"blocked and the check passes": {[]script{{text: "Cannot.\nMAESTRO_STATUS: blocked\nNeed a decision.", verifyOK: true}}, orchestrator.Failed, DeclaredBlocked, 1},
		// The agent could not see the check fail; the service's check
		// runs, and the agent fixes what it reports.
		"blocked, check fails, then fixed": {[]script{{text: "No licence here.\nMAESTRO_STATUS: blocked\nCannot run the check."}, {text: "fixed", verifyOK: true}},
			orchestrator.Published, "", 2},
		// Once the agent has seen the check's output, blocked stands.
		"blocked twice": {[]script{{text: "MAESTRO_STATUS: blocked\nNo licence."}, {text: "MAESTRO_STATUS: blocked\nStill stuck."}},
			orchestrator.Failed, DeclaredBlocked, 2},
		"no changes and done":   {[]script{{text: "Nothing to do.", noChanges: true}}, orchestrator.Failed, NoChanges, 1},
		"continue then publish": {[]script{{text: "MAESTRO_STATUS: continue"}, {text: "MAESTRO_STATUS: done", verifyOK: true}}, orchestrator.Published, "", 2},
		"continue forever": {[]script{{text: "MAESTRO_STATUS: continue"}, {text: "MAESTRO_STATUS: continue"}, {text: "MAESTRO_STATUS: continue"}},
			orchestrator.Failed, MaxTurnsExhausted, 3},
		"no changes, continue, then work": {[]script{{text: "MAESTRO_STATUS: continue", noChanges: true}, {text: "ok", verifyOK: true}},
			orchestrator.Published, "", 2},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, c.turns...)
			out := r.run(orchestrator.Attempt{})
			if out.Kind != c.kind || out.Category != c.cat || len(r.f.prompts) != c.turnsRun {
				t.Fatalf("outcome = %+v after %d turns", out, len(r.f.prompts))
			}
			if r.lastSandboxStep() != "hook:after_run" {
				t.Fatalf("after_run did not run: %v", r.sb.Trace())
			}
		})
	}
}

func TestDeclaredStatusParsing(t *testing.T) {
	for text, want := range map[string]string{
		"":                                  "done",
		"no marker":                         "done",
		"MAESTRO_STATUS: continue":          "continue",
		"x\n  MAESTRO_STATUS: blocked  \ny": "blocked",
		"Changed it.\nMAESTRO_STATUS: check\nI cannot run Unity.": "check",
		"MAESTRO_STATUS: continue\nlater\nMAESTRO_STATUS: done":   "done",
		"I would say MAESTRO_STATUS: blocked here":                "done", // not on its own line
		"MAESTRO_STATUS: published":                               "done", // not a known value
		"MAESTRO_STATUS: done\r\n":                                "done",
	} {
		if got := DeclaredStatus(text); got != want {
			t.Errorf("%q: %s, want %s", text, got, want)
		}
	}
}

func TestAgentErrorsMapToOutcomes(t *testing.T) {
	reset := time.Date(2026, 11, 21, 15, 45, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		err  error
		kind orchestrator.OutcomeKind
		cat  string
	}{
		"quota":       {&AgentError{Category: QuotaExhausted, QuotaResetAt: reset}, orchestrator.Quota, QuotaExhausted},
		"logged out":  {&AgentError{Category: NotLoggedIn}, orchestrator.NotLoggedIn, NotLoggedIn},
		"timeout":     {&AgentError{Category: "turn_timeout"}, orchestrator.Failed, "turn_timeout"},
		"plain error": {errors.New("pipe closed"), orchestrator.Failed, TurnFailed},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, script{err: c.err})
			out := r.run(orchestrator.Attempt{})
			if out.Kind != c.kind || out.Category != c.cat {
				t.Fatalf("outcome = %+v", out)
			}
			if c.kind == orchestrator.Quota && !out.QuotaResetAt.Equal(reset) {
				t.Fatalf("reset time lost: %v", out.QuotaResetAt)
			}
			if r.f.publishes != 0 || r.lastSandboxStep() != "hook:after_run" {
				t.Fatalf("publishes %d, steps %v", r.f.publishes, r.sb.Trace())
			}
		})
	}
}

func TestIssueChangedDuringTurnIsNotPublished(t *testing.T) {
	for name, change := range map[string]func(r *rig){
		"cancelled":          func(r *rig) { r.tr.Move("id-1", "Cancelled") },
		"moved to backlog":   func(r *rig) { r.tr.Move("id-1", "Backlog") },
		"deleted":            func(r *rig) { r.tr.Delete("id-1") },
		"approval withdrawn": func(r *rig) { i := r.tr.Get("id-1"); i.Dispatchable = false; r.tr.Put(i) },
		"label removed":      func(r *rig) { i := r.tr.Get("id-1"); i.Labels = nil; r.tr.Put(i) },
		"text edited": func(r *rig) {
			i := r.tr.Get("id-1")
			i.Description, i.ContentHash = "Also add a deploy key.", ""
			r.tr.Put(i)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, script{text: "done", verifyOK: true})
			r.f.turns[0].during = func() { change(r) }
			out := r.run(orchestrator.Attempt{})
			if out.Kind != orchestrator.Normal || r.f.publishes != 0 || r.f.verifies != 0 {
				t.Fatalf("outcome = %+v, publishes %d, verifies %d", out, r.f.publishes, r.f.verifies)
			}
		})
	}
}

func TestPublishRejectionAndError(t *testing.T) {
	r := newRig(t, script{text: "done", verifyOK: true})
	r.f.reject = &Rejection{Category: "policy_denied", Feedback: []issue.Feedback{{Source: "policy", Body: "D1 .github/workflows/ci.yml"}}}
	out := r.run(orchestrator.Attempt{})
	if out.Kind != orchestrator.Rejected || out.Category != "policy_denied" || len(out.Feedback) != 1 {
		t.Fatalf("outcome = %+v", out)
	}
	r = newRig(t, script{text: "done", verifyOK: true})
	r.f.pubErr = errors.New("push_rejected")
	if out := r.run(orchestrator.Attempt{}); out.Kind != orchestrator.Failed || out.Category != PublishError {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestRefreshFailureFailsTheAttempt(t *testing.T) {
	r := newRig(t, script{text: "done", verifyOK: true})
	r.f.turns[0].during = func() { r.tr.FailReads = 1 }
	if out := r.run(orchestrator.Attempt{}); out.Kind != orchestrator.Failed || out.Category != RefreshError || r.f.publishes != 0 {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestWorkspaceAndHookFailures(t *testing.T) {
	r := newRig(t, script{text: "done", verifyOK: true})
	r.sb.Handler = func(_ context.Context, req sandbox.Request) (sandbox.Result, error) {
		if req.Populate != nil && req.Populate.Op == sandbox.OpClone {
			return sandbox.Result{ExitStatus: 128}, nil
		}
		return sandboxHandler(context.Background(), req)
	}
	out := r.run(orchestrator.Attempt{})
	if out.Kind != orchestrator.Failed || out.Category != workspace.PopulateFailed || len(r.f.prompts) != 0 {
		t.Fatalf("outcome = %+v", out)
	}
	for _, s := range r.sb.Trace() {
		if strings.HasPrefix(s, "hook:") {
			t.Fatalf("a hook ran without a workspace: %v", r.sb.Trace())
		}
	}

	r = newRig(t, script{text: "done", verifyOK: true})
	r.sb.Handler = func(_ context.Context, req sandbox.Request) (sandbox.Result, error) {
		if req.Env["MAESTRO_HOOK"] == workspace.BeforeRun {
			return sandbox.Result{ExitStatus: 1}, nil
		}
		return sandboxHandler(context.Background(), req)
	}
	out = r.run(orchestrator.Attempt{})
	if out.Kind != orchestrator.Failed || out.Category != workspace.HookFailed || len(r.f.prompts) != 0 {
		t.Fatalf("outcome = %+v", out)
	}
	if r.lastSandboxStep() != "hook:after_run" {
		t.Fatalf("after_run did not run after a failed before_run: %v", r.sb.Trace())
	}
}

func TestRetryAttemptPromptCarriesFeedback(t *testing.T) {
	r := newRig(t, script{text: "done", verifyOK: true})
	r.run(orchestrator.Attempt{Number: 1, RetryKind: orchestrator.KindGate,
		Feedback: []issue.Feedback{{Source: "ci", Body: "lint: unused variable"}}})
	p := r.f.prompts[0].Prompt
	if !strings.Contains(p, "This is attempt 2") || !strings.Contains(p, "source=ci\nlint: unused variable") {
		t.Fatalf("prompt = %s", p)
	}
}

func TestTerminateCancelsTheTurnAndStillCleansUp(t *testing.T) {
	r := newRig(t)
	r.f.hang = make(chan struct{})
	h, err := r.w.Start(orchestrator.Attempt{Issue: r.is}, r.sink)
	if err != nil {
		t.Fatal(err)
	}
	<-r.f.hang
	h.Terminate()
	h.Terminate() // safe to repeat
	select {
	case out := <-r.sink.done:
		if out.Kind != orchestrator.Normal {
			t.Fatalf("outcome = %+v", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attempt did not stop")
	}
	if r.lastSandboxStep() != "hook:after_run" || r.f.publishes != 0 {
		t.Fatalf("steps = %v", r.sb.Trace())
	}
}

func TestUnconfiguredWorkerRefusesToStart(t *testing.T) {
	if _, err := (&Worker{}).Start(orchestrator.Attempt{}, &collector{}); err == nil {
		t.Fatal("started without collaborators")
	}
}

// ---- end to end with the scheduler ----------------------------------------

func TestEndToEndWithScheduler(t *testing.T) {
	r := newRig(t,
		script{text: "first try\nMAESTRO_STATUS: done"},                  // verify fails, turn 2
		script{text: "second try\nMAESTRO_STATUS: done", verifyOK: true}, // publishes
	)
	todo := r.is
	todo.State = "Todo"
	r.tr.Put(todo)
	other := todo
	other.ID, other.Identifier, other.AuthorID, other.Dispatchable = "id-2", "GH-2", "stranger", false
	r.tr.Put(other)

	b, err := budget.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	o := orchestrator.New(r.cfg, orchestrator.Deps{Tracker: r.tr, Worker: r.w, Budget: b,
		Workspaces: workspace.New(r.cfg, r.sb, nil)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { o.Run(ctx); close(done) }()

	deadline := time.Now().Add(10 * time.Second)
	for len(o.Latest().AwaitingChecks) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("not published; snapshot %+v", o.Latest())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	s := o.Latest()
	if len(s.Running) != 0 || s.AwaitingChecks[0].Identifier != "GH-1" || s.AwaitingChecks[0].PRNumber != 42 {
		t.Fatalf("snapshot = %+v", s)
	}
	if got := strings.Join(r.tr.StateWrites("id-1"), ","); got != "In Progress" {
		t.Fatalf("state writes for GH-1 = %s", got)
	}
	// The issue that is not approved was never touched.
	if len(r.tr.StateWrites("id-2")) != 0 || r.tr.Get("id-2").State != "Todo" {
		t.Fatal("an issue that is not dispatchable was touched")
	}
	if len(r.f.prompts) != 2 || r.f.publishes != 1 || b.Failures("id-1") != 0 || b.Today(time.Now()).Runs != 1 {
		t.Fatalf("turns %d, publishes %d, failures %d", len(r.f.prompts), r.f.publishes, b.Failures("id-1"))
	}
	if s.Totals.Usage.Input != 200 {
		t.Fatalf("usage not accounted: %+v", s.Totals)
	}
}

func TestLostSessionRestartsOnceWithTheFullPrompt(t *testing.T) {
	lost := &AgentError{Category: SessionUnknown, Msg: "gone"}
	r := newRig(t,
		script{text: "MAESTRO_STATUS: continue"},
		script{err: lost},
		script{text: "MAESTRO_STATUS: done", verifyOK: true})
	if out := r.run(orchestrator.Attempt{}); out.Kind != orchestrator.Published {
		t.Fatalf("outcome = %+v", out)
	}
	p := r.f.prompts
	if len(p) != 3 || p[1].ResumeSessionID != "sess-1" || p[2].ResumeSessionID != "" ||
		!strings.Contains(p[2].Prompt, "Implement GH-1.") || strings.Contains(p[1].Prompt, "Implement GH-1.") {
		t.Fatalf("prompts = %+v", p)
	}
	found := false
	for _, e := range r.sink.events {
		found = found || e.Kind == "session_reset"
	}
	if !found {
		t.Fatal("no session_reset event")
	}

	// A second loss in the same attempt, and a loss on a first turn, fail.
	r = newRig(t, script{text: "MAESTRO_STATUS: continue"}, script{err: lost}, script{err: lost})
	if out := r.run(orchestrator.Attempt{}); out.Kind != orchestrator.Failed || out.Category != SessionUnknown || len(r.f.prompts) != 3 {
		t.Fatalf("outcome = %+v after %d turns", out, len(r.f.prompts))
	}
	r = newRig(t, script{err: lost})
	if out := r.run(orchestrator.Attempt{}); out.Kind != orchestrator.Failed || len(r.f.prompts) != 1 {
		t.Fatalf("outcome = %+v after %d turns", out, len(r.f.prompts))
	}
}

func TestOwnerCommentsReachTheAgentAndNobodyElses(t *testing.T) {
	r := newRig(t, script{text: "done", verifyOK: true})
	at := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	// The fake returns what the adapter would: allowlisted commenters only.
	for i := 0; i < maxOwnerComments+2; i++ {
		r.tr.Comments = append(r.tr.Comments, tracker.Feedback{Author: "ian", Body: fmt.Sprintf("note %d.", i), CreatedAt: at.Add(time.Duration(i) * time.Minute)})
	}
	out := r.run(orchestrator.Attempt{Feedback: []issue.Feedback{{Source: "ci", Body: "lint failed"}}})
	if out.Kind != orchestrator.Published {
		t.Fatalf("outcome %+v", out)
	}
	p := r.f.prompts[0].Prompt
	if !strings.Contains(p, "source=tracker author=ian written=2026-11-21T09:21Z\nnote 21.") || !strings.Contains(p, "source=ci\nlint failed") {
		t.Fatalf("prompt = %s", p)
	}
	// Only the most recent ones, oldest first, after the gate's feedback.
	if strings.Contains(p, "note 1.") || strings.Index(p, "note 2.") > strings.Index(p, "note 21.") || strings.Index(p, "lint failed") > strings.Index(p, "note 2.") {
		t.Errorf("order or cap of the comments is wrong:\n%s", p)
	}
}

type notes struct {
	fs  []issue.Feedback
	err error
}

func (n notes) Feedback(context.Context, issue.Issue) ([]issue.Feedback, error) { return n.fs, n.err }

func TestCommentsOnThePullRequestReachTheAgent(t *testing.T) {
	r := newRig(t, script{text: "done", verifyOK: true})
	r.w.Notes = notes{fs: []issue.Feedback{{Source: "review", Author: "ian", Body: "Rename this.", Path: "a.go", Line: 3}}}
	r.run(orchestrator.Attempt{})
	if p := r.f.prompts[0].Prompt; !strings.Contains(p, "source=review author=ian\npath: a.go\nline: 3\n\nRename this.") {
		t.Fatalf("prompt = %s", p)
	}

	// When they cannot be read the run does not start without them.
	r = newRig(t, script{text: "done", verifyOK: true})
	r.w.Notes = notes{err: errors.New("github is down")}
	if out := r.run(orchestrator.Attempt{}); out.Kind != orchestrator.Failed || out.Category != RefreshError || len(r.f.prompts) != 0 {
		t.Fatalf("outcome = %+v, %d prompts", out, len(r.f.prompts))
	}
}

// checkGates adds the pictures of a check to the fakes.
type checkGates struct {
	*fakes
	given int
	shows []string
}

func (c *checkGates) Show(key string, things []string) {
	c.shows = append(c.shows, key+": "+strings.Join(things, ", "))
}

func (c *checkGates) Verify(ctx context.Context, ws workspace.Workspace) (VerifyResult, error) {
	v, err := c.fakes.Verify(ctx, ws)
	if v.Passed {
		v.Screenshots = []Screenshot{{Caption: "1-Main", PNG: []byte("png")}}
	}
	return v, err
}

func (c *checkGates) GiveCheckPictures(_ context.Context, ws workspace.Workspace, shots []Screenshot) ([]CurrentFile, string) {
	c.given++
	return []CurrentFile{{Caption: shots[0].Caption, Path: "../check-screenshots/1.png"}}, ""
}

// The agent asks for the checks it cannot run where it works: they run,
// nothing is published, and the output and the pictures go back to it.
func TestTheAgentAsksForTheChecks(t *testing.T) {
	r := newRig(t,
		script{text: "Changed the shader.\nMAESTRO_SHOW: StartLevel\nMAESTRO_STATUS: check\nI cannot run Unity here."},
		script{text: "Fixed the test.\nMAESTRO_STATUS: check\nLet me see it.", verifyOK: true},
		script{text: "Looks right.\nMAESTRO_STATUS: done", verifyOK: true})
	r.cfg.Agent.MaxTurns = 5
	g := &checkGates{fakes: r.f}
	r.w.Gates = g
	out := r.run(orchestrator.Attempt{})
	if out.Kind != orchestrator.Published || r.f.publishes != 1 || r.f.verifies != 3 || g.given != 1 {
		t.Fatalf("outcome %+v, publishes %d, verifies %d, given %d", out, r.f.publishes, r.f.verifies, g.given)
	}
	second, third := r.f.prompts[1].Prompt, r.f.prompts[2].Prompt
	if !strings.Contains(second, "failed with exit status 1") || !strings.Contains(second, "FAIL TestLogin") || !strings.Contains(second, "Nothing was published") {
		t.Fatalf("after a failed check:\n%s", second)
	}
	if !strings.Contains(third, "passed") || !strings.Contains(third, "- ../check-screenshots/1.png: 1-Main") {
		t.Fatalf("after a passing check:\n%s", third)
	}
	if !strings.Contains(r.f.prompts[0].Prompt, `"MAESTRO_STATUS: check"`) || !strings.Contains(r.f.prompts[0].Prompt, "MAESTRO_SHOW:") {
		t.Fatal("the first prompt does not tell the agent about checks and what to show")
	}
	// What the pictures should show is given to the gates once, before
	// the check; the turns that name nothing keep it.
	if len(g.shows) != 1 || !strings.HasSuffix(g.shows[0], ": StartLevel") {
		t.Fatalf("shows %q", g.shows)
	}
}

func TestWhatThePicturesShouldShow(t *testing.T) {
	got := DeclaredShows("Done.\nMAESTRO_SHOW: /pricing?plan=pro\n  MAESTRO_SHOW:   StartLevel  \r\nMAESTRO_SHOW: StartLevel\n" +
		"MAESTRO_SHOW: ../../etc/passwd\nMAESTRO_SHOW: a|b\nMAESTRO_SHOW: $(rm -rf /)\nMAESTRO_SHOW:\nsays MAESTRO_SHOW: inline\nMAESTRO_STATUS: done")
	if strings.Join(got, ",") != "/pricing?plan=pro,StartLevel" {
		t.Fatalf("got %q", got)
	}
	var many strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&many, "MAESTRO_SHOW: page%d.html\n", i)
	}
	if got := DeclaredShows(many.String()); len(got) != config.MaxScreenshots {
		t.Fatalf("%d things", len(got))
	}
}

// On the last turn a check is the end of the work: it is verified and
// published as with done. Past the limit, the checks do not run again.
func TestChecksOnTheLastTurnAndPastTheLimit(t *testing.T) {
	r := newRig(t, script{text: "MAESTRO_STATUS: check"}, script{text: "MAESTRO_STATUS: check"}, script{text: "MAESTRO_STATUS: check", verifyOK: true})
	if out := r.run(orchestrator.Attempt{}); out.Kind != orchestrator.Published || r.f.verifies != 3 {
		t.Fatalf("outcome %+v, verifies %d", out, r.f.verifies)
	}

	var turns []script
	for i := 0; i < MaxChecks+1; i++ {
		turns = append(turns, script{text: "MAESTRO_STATUS: check"})
	}
	turns = append(turns, script{text: "MAESTRO_STATUS: done", verifyOK: true})
	r = newRig(t, turns...)
	r.cfg.Agent.MaxTurns = int64(len(turns))
	if out := r.run(orchestrator.Attempt{}); out.Kind != orchestrator.Published || r.f.verifies != MaxChecks+1 {
		t.Fatalf("outcome %+v, verifies %d", out, r.f.verifies)
	}
	if p := r.f.prompts[MaxChecks+1].Prompt; !strings.Contains(p, "did not run again") {
		t.Fatalf("past the limit:\n%s", p)
	}
}
