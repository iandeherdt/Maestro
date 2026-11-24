// Package orchestrator is the single authority over scheduling state: it
// polls the board, claims issues, starts workers, reconciles running work
// against the board, retries with backoff and enforces budgets (SPEC
// sections 7, 8 and 19).
//
// All state is touched from one goroutine. Workers report back through a
// Sink, which only enqueues; the reports are applied by Run, or by Drain in
// tests.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iandeherdt/Maestro/internal/budget"
	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/plan"
	"github.com/iandeherdt/Maestro/internal/tracker"
)

const (
	continuationDelay = time.Second
	gateDelay         = time.Second
	failureBaseDelay  = 10 * time.Second
	// maxWriteFailures consecutive failed tracker writes pause dispatch.
	maxWriteFailures = 10
	// maxSpawnFailures consecutive failed worker launches pause dispatch.
	maxSpawnFailures = 3
)

// Deps are the collaborators of the orchestrator.
type Deps struct {
	Tracker    tracker.Tracker
	Worker     Worker
	Workspaces Workspaces
	Budget     *budget.Store
	// Prober is optional. Without it an in-progress issue found after a
	// restart is always dispatched as a retry.
	Prober Prober
	// Checks is optional. Without it published work holds its claim and
	// nothing moves it on, which is only useful in tests.
	Checks Checker
	// Reviews is optional. Without it nothing follows a ticket once it is
	// handed to a person.
	Reviews Reviews
	// Plans is optional. Without it ideas are not planned.
	Plans Plans
	// Designs is optional. Without it no designs are made.
	Designs Designs
	// Preflight is the optional per-tick validation (SPEC 6.2, 6.3). It
	// returns a configuration to apply when the workflow file changed, or
	// nil. An error skips dispatch for that tick; reconciliation still
	// runs, with the last good configuration.
	Preflight func() (*config.Config, error)
	Clock     Clock
	Log       *slog.Logger
}

type runEntry struct {
	gen           uint64
	handle        Handle
	issue         issue.Issue
	dispatchState string // normalized state the issue was dispatched from
	dispatchHash  string
	attempt       int
	startedAt     time.Time
	sessionID     string
	pid           int
	phase         Phase
	lastEvent     string
	lastEventAt   time.Time
	lastMessage   string
	usage         Usage
	turnCount     int
	// feed is the agent's recent messages and tool calls, oldest first,
	// for the status page.
	feed []FeedItem
	// label is the working label the idea carries while this run lasts.
	label string
}

// Orchestrator holds the runtime state of SPEC 4.1.10.
type Orchestrator struct {
	cfg *config.Config
	d   Deps

	running  map[string]*runEntry
	awaiting map[string]*PublishRecord
	// checkedAt and awaitAttempt belong to awaiting entries.
	checkedAt    map[string]time.Time
	awaitAttempt map[string]int
	// noChecksNoted holds the awaiting issues whose card already says that
	// no check has started.
	noChecksNoted map[string]bool
	// handoffAt is when the hand-off watch last ran. closedNoted holds the
	// issues whose closed pull request was already commented on.
	handoffAt   time.Time
	closedNoted map[string]bool
	// plansAt is when approved plans were last looked at. planNoted holds
	// the ideas whose missing or outdated plan was already commented on.
	plansAt   time.Time
	planNoted map[string]bool
	// designNoted holds the ideas in the design state without a plan that
	// were already commented on.
	designNoted map[string]bool
	claimed     map[string]bool
	retries     map[string]*RetryEntry
	completed   map[string]bool
	totals      Totals

	gen           uint64
	spawnFailures int
	configError   string
	// gateNoted is the last logged reason that dispatch is stopped.
	gateNoted string

	active, terminal map[string]bool

	mu    sync.Mutex
	queue []func(context.Context)
	wake  chan struct{}

	forceTick bool
	latest    atomic.Pointer[Snapshot]
}

func New(cfg *config.Config, d Deps) *Orchestrator {
	if d.Clock == nil {
		d.Clock = systemClock{}
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	o := &Orchestrator{
		d:         d,
		running:   map[string]*runEntry{},
		awaiting:  map[string]*PublishRecord{},
		checkedAt: map[string]time.Time{}, awaitAttempt: map[string]int{}, noChecksNoted: map[string]bool{}, closedNoted: map[string]bool{},
		planNoted: map[string]bool{}, designNoted: map[string]bool{},
		claimed:   map[string]bool{},
		retries:   map[string]*RetryEntry{},
		completed: map[string]bool{},
		wake:      make(chan struct{}, 1),
	}
	o.SetConfig(cfg)
	o.publish()
	return o
}

// publish stores a snapshot for readers on other goroutines.
func (o *Orchestrator) publish() {
	s := o.Snapshot()
	o.latest.Store(&s)
}

// Latest returns the most recently published snapshot. It is safe to call
// from any goroutine.
func (o *Orchestrator) Latest() Snapshot { return *o.latest.Load() }

// RequestTick asks Run for a poll tick as soon as possible. It is safe to
// call from any goroutine.
func (o *Orchestrator) RequestTick() {
	o.post(func(context.Context) { o.forceTick = true })
}

// Pause stops new dispatches until Resume. Running attempts continue. It is
// safe to call from any goroutine, and it survives a restart.
func (o *Orchestrator) Pause(reason string) {
	o.post(func(context.Context) {
		if reason == "" {
			reason = "paused by the operator"
		}
		o.d.Budget.Resume() // so that a new reason replaces an old one
		o.d.Log.Info("dispatch paused", "reason", reason)
		o.d.Budget.Pause(reason)
		o.persist()
		o.publish()
	})
}

// Resume lifts a pause, whoever set it, and looks for work at once.
func (o *Orchestrator) Resume() {
	o.post(func(context.Context) {
		o.d.Budget.Resume()
		o.spawnFailures = 0
		o.d.Log.Info("dispatch resumed")
		o.persist()
		o.publish()
		o.forceTick = true
	})
}

// SetConfig applies a (re)loaded configuration. Call it from the goroutine
// that drives the orchestrator.
func (o *Orchestrator) SetConfig(cfg *config.Config) {
	o.cfg = cfg
	o.active, o.terminal = map[string]bool{}, map[string]bool{}
	for _, s := range cfg.Tracker.ActiveStates {
		o.active[issue.NormalizeState(s)] = true
	}
	for _, s := range cfg.Tracker.TerminalStates {
		o.terminal[issue.NormalizeState(s)] = true
	}
}

func (o *Orchestrator) now() time.Time { return o.d.Clock.Now() }

func (o *Orchestrator) persist() {
	if err := o.d.Budget.Save(); err != nil {
		o.d.Log.Error("budget record not saved", "error", err)
	}
}

func (o *Orchestrator) isActive(is issue.Issue) bool {
	s := issue.NormalizeState(is.State)
	return o.active[s] && !o.terminal[s]
}

func (o *Orchestrator) isTerminal(is issue.Issue) bool {
	return o.terminal[issue.NormalizeState(is.State)]
}

// routable: the adapter approved it and it carries this worker's labels.
func (o *Orchestrator) routable(is issue.Issue) bool {
	return is.Dispatchable && is.HasLabels(o.cfg.Tracker.RequiredLabels)
}

// ---- inbox ----------------------------------------------------------------

func (o *Orchestrator) post(fn func(context.Context)) {
	o.mu.Lock()
	o.queue = append(o.queue, fn)
	o.mu.Unlock()
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// Drain applies every queued worker report. Run does this itself.
func (o *Orchestrator) Drain(ctx context.Context) {
	for {
		o.mu.Lock()
		q := o.queue
		o.queue = nil
		o.mu.Unlock()
		if len(q) == 0 {
			return
		}
		for _, fn := range q {
			fn(ctx)
		}
		o.publish()
	}
}

type sink struct {
	o   *Orchestrator
	id  string
	gen uint64
}

func (s sink) Event(e Event) {
	s.o.post(func(context.Context) { s.o.onEvent(s.id, s.gen, e) })
}

func (s sink) Exit(out Outcome) {
	s.o.post(func(ctx context.Context) { s.o.onWorkerExit(ctx, s.id, s.gen, out) })
}

// ---- run loop -------------------------------------------------------------

// Run drives ticks, retry timers and worker reports until ctx ends. On
// return every running sandbox unit has been told to terminate.
func (o *Orchestrator) Run(ctx context.Context) {
	nextTick := o.now()
	for {
		o.Drain(ctx)
		now := o.now()
		if o.forceTick || !now.Before(nextTick) {
			o.forceTick = false
			o.Tick(ctx)
			nextTick = o.now().Add(time.Duration(o.cfg.Polling.IntervalMS) * time.Millisecond)
		}
		o.FireDueRetries(ctx)

		wait := nextTick.Sub(o.now())
		for _, r := range o.retries {
			if d := r.DueAt.Sub(o.now()); d < wait {
				wait = d
			}
		}
		if wait < 0 {
			wait = 0
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			for _, e := range o.running {
				e.handle.Terminate()
			}
			return
		case <-o.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// ---- tick -----------------------------------------------------------------

// Tick runs one poll-and-dispatch cycle (SPEC 19.2).
func (o *Orchestrator) Tick(ctx context.Context) {
	defer o.publish()

	// A changed workflow file is applied first, so that reconciliation
	// already uses it. An invalid one leaves the last good configuration.
	o.configError = ""
	if o.d.Preflight != nil {
		cfg, err := o.d.Preflight()
		if cfg != nil {
			o.SetConfig(cfg)
		}
		if err != nil {
			o.configError = err.Error()
			o.d.Log.Error("dispatch validation failed, skipping dispatch", "error", err)
		}
	}

	o.reconcile(ctx)
	o.watchChecks(ctx)
	o.watchHandoff(ctx)
	o.watchPlans(ctx)

	if o.configError != "" {
		return
	}
	reason := o.gated()
	if reason != o.gateNoted {
		// A change is logged once, so a stopped dispatch is never silent.
		if reason != "" {
			o.d.Log.Warn("dispatch stopped", "reason", reason)
		} else {
			o.d.Log.Info("dispatch resumed", "was", o.gateNoted)
		}
		o.gateNoted = reason
	}
	if reason != "" {
		return
	}

	issues, err := o.d.Tracker.FetchIssuesByStates(ctx, o.cfg.Tracker.ActiveStates)
	if err != nil {
		o.d.Log.Warn("candidate fetch failed, skipping dispatch", "error", err)
		return
	}
	issue.SortForDispatch(issues)

	// A block marker on an issue that is active again means a human moved
	// it back: fresh budget. This runs before the budget check below.
	reset := false
	for _, is := range issues {
		if o.isActive(is) && o.d.Budget.BlockMarked(is.ID) && !o.claimed[is.ID] {
			o.d.Budget.ClearIssue(is.ID)
			reset = true
		}
	}
	if reset {
		o.persist()
	}

	for _, is := range issues {
		if o.availableSlots() == 0 || o.gated() != "" {
			break
		}
		if o.claimed[is.ID] || !o.eligible(is) || !o.stateSlotFree(is) {
			continue
		}
		if o.designing(is) && !o.d.Designs.HasPlan(is.ID) {
			if !o.designNoted[is.ID] {
				o.designNoted[is.ID] = true
				o.comment(ctx, is, "This card is in "+o.cfg.Tracker.DesignState+", but this machine has no plan for it that you have seen. "+
					"A design is made for a plan: move the card to "+o.cfg.Tracker.PlanningState+" first.", "")
			}
			continue
		}
		if o.d.Prober != nil && issue.NormalizeState(is.State) == issue.NormalizeState(o.cfg.Tracker.InProgressState) {
			rec, err := o.d.Prober.Probe(ctx, is)
			if err != nil {
				o.d.Log.Warn("publish probe failed, will retry", "issue", is.Identifier, "error", err)
				continue
			}
			if rec != nil {
				o.claimed[is.ID] = true
				o.awaiting[is.ID] = rec
				continue
			}
		}
		o.dispatch(ctx, is, 0, "", nil, 0)
	}
}

// gated returns why nothing may be dispatched right now, or "".
func (o *Orchestrator) gated() string {
	b, now := o.d.Budget, o.now()
	if paused, reason := b.Paused(); paused {
		return "paused: " + reason
	}
	if now.Before(b.QuotaBlockedUntil()) {
		return "quota window"
	}
	day, lim := b.Today(now), o.cfg.Budget
	// An attempt whose agent has not started yet is not a run, but holds
	// its place, so that concurrent dispatches stay under the limit.
	for _, e := range o.running {
		if e.turnCount == 0 {
			day.Runs++
		}
	}
	switch {
	case day.Runs >= lim.MaxRunsPerDay:
		return fmt.Sprintf("daily run limit reached (%d of %d runs); raise budget.max_runs_per_day", day.Runs, lim.MaxRunsPerDay)
	case lim.MaxTokensPerDay != nil && day.TotalTokens >= *lim.MaxTokensPerDay:
		return fmt.Sprintf("daily token limit reached (%d of %d tokens); raise budget.max_tokens_per_day", day.TotalTokens, *lim.MaxTokensPerDay)
	case lim.MaxAgentSecondsPerDay != nil && day.AgentSeconds >= *lim.MaxAgentSecondsPerDay:
		return fmt.Sprintf("daily agent time limit reached (%ds of %ds); raise budget.max_agent_seconds_per_day", day.AgentSeconds, *lim.MaxAgentSecondsPerDay)
	}
	return ""
}

// dailyLimit is gated's reason when a daily limit is what stops dispatch.
func (o *Orchestrator) dailyLimit() string {
	if r := o.gated(); strings.HasPrefix(r, "daily ") {
		return r
	}
	return ""
}

// eligible applies the per-issue rules of SPEC 8.2, without the claim and
// slot checks.
func (o *Orchestrator) eligible(is issue.Issue) bool {
	if is.ID == "" || is.Identifier == "" || is.Title == "" || is.State == "" {
		return false
	}
	if !o.isActive(is) || !o.routable(is) {
		return false
	}
	if o.planning(is) && o.d.Plans == nil {
		return false
	}
	if o.designing(is) && o.d.Designs == nil {
		return false
	}
	if _, err := issue.WorkspaceKey(is.Identifier); err != nil {
		return false
	}
	if _, running := o.running[is.ID]; running {
		return false
	}
	return o.d.Budget.Failures(is.ID) < o.cfg.Budget.MaxFailureAttempts
}

func (o *Orchestrator) availableSlots() int {
	n := int(o.cfg.Agent.MaxConcurrentAgents) - len(o.running)
	return max(n, 0)
}

// stateSlotFree applies the per-state limit. Running work is counted under
// the state it was dispatched from, because dispatch itself moves every
// issue to the in-progress state.
func (o *Orchestrator) stateSlotFree(is issue.Issue) bool {
	state := issue.NormalizeState(is.State)
	limit, ok := o.cfg.Agent.MaxConcurrentAgentsByState[state]
	if !ok {
		return true
	}
	var n int64
	for _, e := range o.running {
		if e.dispatchState == state {
			n++
		}
	}
	return n < limit
}

// ---- dispatch -------------------------------------------------------------

// dispatch claims the issue and starts a worker (SPEC 19.4).
func (o *Orchestrator) dispatch(ctx context.Context, is issue.Issue, attempt int, kind string, feedback []issue.Feedback, writeFailures int) {
	o.claimed[is.ID] = true
	delete(o.retries, is.ID)

	// An idea stays in its own state while it is planned. Moving it to the
	// in-progress state would turn it into work to implement after a
	// restart.
	planning, designing := o.planning(is), o.designing(is)
	delete(o.designNoted, is.ID)
	dispatchState := issue.NormalizeState(is.State)
	if !planning && !designing {
		if err := o.d.Tracker.SetState(ctx, is, o.cfg.Tracker.InProgressState); err != nil {
			// The claim is held: an issue whose state cannot be written must
			// not be picked up again by the next poll.
			o.trackerRetry(is, &RetryEntry{Attempt: attempt, resumeKind: kind, Feedback: feedback,
				writeFailures: writeFailures + 1, Error: "tracker write failed: " + err.Error()})
			return
		}
		is.State = o.cfg.Tracker.InProgressState
	}

	what := "Dispatched to"
	if planning {
		what = "Planning on"
	}
	if designing {
		what = "Making a design on"
	}
	o.comment(ctx, is, what+" worker `"+o.cfg.Worker.Name+"`"+attemptSuffix(attempt)+".", "")

	o.gen++
	h, err := o.d.Worker.Start(Attempt{Issue: is, Number: attempt, RetryKind: kind, Feedback: feedback, Planning: planning, Design: designing},
		sink{o, is.ID, o.gen})
	if err != nil {
		o.spawnFailures++
		o.d.Log.Error("worker launch failed", "issue", is.Identifier, "error", err)
		if o.spawnFailures >= maxSpawnFailures {
			o.pause("sandbox launch failed " + fmt.Sprint(o.spawnFailures) + " times in a row")
		}
		o.schedule(&RetryEntry{IssueID: is.ID, Identifier: is.Identifier, Attempt: attempt + 1,
			Kind: KindFailure, Feedback: feedback, Error: "failed to spawn worker"})
		return
	}
	o.spawnFailures = 0
	e := &runEntry{
		gen: o.gen, handle: h, issue: is, dispatchState: dispatchState,
		dispatchHash: is.ContentHash, attempt: attempt, startedAt: o.now(),
		phase: PreparingWorkspace,
	}
	o.running[is.ID] = e
	switch {
	case designing:
		o.working(ctx, e, DesigningLabel, "An agent is making a design for the plan on worker `"+o.cfg.Worker.Name+"`"+attemptSuffix(attempt)+
			". The card stays in "+o.cfg.Tracker.DesignState+" while it works and carries the label `"+DesigningLabel+
			"`. When the design is ready, its pull request is linked here and the card moves to "+o.cfg.Tracker.HandoffState+".")
	case planning:
		o.working(ctx, e, PlanningLabel, "An agent is planning this idea on worker `"+o.cfg.Worker.Name+"`"+attemptSuffix(attempt)+
			". The card stays in "+o.cfg.Tracker.PlanningState+" while it works and carries the label `"+PlanningLabel+
			"`. Its questions or its plan follow here, and the card moves to "+o.cfg.Tracker.HandoffState+".")
	}
}

// The labels an idea carries while an agent plans it or makes its design.
// The card stays in its column meanwhile, so the label is what the board
// shows (SPEC 22 and 27).
const (
	PlanningLabel  = "maestro:planning"
	DesigningLabel = "maestro:designing"
)

// working tells the owner that an agent started on an idea: a new comment,
// which shows on the issue below the earlier ones, and a label, which
// shows on the card. Both are best effort.
func (o *Orchestrator) working(ctx context.Context, e *runEntry, label, text string) {
	if p, ok := o.d.Tracker.(interface {
		PostComment(context.Context, issue.Issue, string) error
	}); ok {
		if err := p.PostComment(ctx, e.issue, text); err != nil {
			o.d.Log.Warn("start note not posted", "issue", e.issue.Identifier, "error", err)
		}
	}
	if a, ok := o.d.Tracker.(tracker.Activity); ok {
		if err := a.MarkWorking(ctx, e.issue, label); err != nil {
			o.d.Log.Warn("working label not set", "issue", e.issue.Identifier, "error", err)
			return
		}
		e.label = label
	}
}

// stopped takes the working label off an idea whose run ended.
func (o *Orchestrator) stopped(ctx context.Context, e *runEntry) {
	if e.label == "" {
		return
	}
	if a, ok := o.d.Tracker.(tracker.Activity); ok {
		if err := a.UnmarkWorking(ctx, e.issue, e.label); err != nil {
			o.d.Log.Warn("working label not removed", "issue", e.issue.Identifier, "error", err)
		}
	}
}

func attemptSuffix(attempt int) string {
	if attempt == 0 {
		return ""
	}
	return fmt.Sprintf(" (attempt %d)", attempt+1)
}

func (o *Orchestrator) pause(reason string) {
	if paused, _ := o.d.Budget.Paused(); paused {
		return
	}
	o.d.Log.Error("dispatch paused", "reason", reason)
	o.d.Budget.Pause(reason)
	o.persist()
}

// comment writes the status comment. Failure is logged and does not change
// scheduling state. untrusted is text from the agent or a tool.
func (o *Orchestrator) comment(ctx context.Context, is issue.Issue, fixed, untrusted string) {
	// The same sentence goes to the log, so that the terminal shows a
	// ticket's progress and not only its failures.
	o.d.Log.Info("status", "issue", is.Identifier, "text", fixed)
	body := fixed
	if untrusted != "" {
		body += "\n\n" + tracker.Fence(untrusted)
	}
	if err := o.d.Tracker.UpsertStatusComment(ctx, is, body); err != nil {
		o.d.Log.Warn("status comment not written", "issue", is.Identifier, "error", err)
	}
}

// ---- retries --------------------------------------------------------------

func (o *Orchestrator) delay(r *RetryEntry) time.Duration {
	switch r.Kind {
	case KindFailure:
		limit := time.Duration(o.cfg.Agent.MaxRetryBackoffMS) * time.Millisecond
		d := failureBaseDelay
		for i := 1; i < r.Attempt && d < limit; i++ {
			d *= 2
		}
		return min(d, limit)
	case KindQuota:
		if until := o.d.Budget.QuotaBlockedUntil(); until.After(o.now()) {
			return until.Sub(o.now())
		}
		return time.Duration(o.cfg.Budget.QuotaBackoffMS) * time.Millisecond
	case KindTracker:
		return time.Duration(o.cfg.Polling.IntervalMS) * time.Millisecond
	case KindGate:
		return gateDelay
	default:
		return continuationDelay
	}
}

// schedule queues a retry. The claim stays in place.
func (o *Orchestrator) schedule(r *RetryEntry) {
	if r.DueAt.IsZero() {
		r.DueAt = o.now().Add(o.delay(r))
	}
	o.claimed[r.IssueID] = true
	o.retries[r.IssueID] = r
	o.d.Log.Info("retry scheduled", "issue", r.Identifier, "kind", r.Kind, "attempt", r.Attempt,
		"due", r.DueAt, "error", r.Error)
}

// trackerRetry holds the claim after a failed tracker write and tries the
// same write again one poll interval later.
func (o *Orchestrator) trackerRetry(is issue.Issue, r *RetryEntry) {
	r.IssueID, r.Identifier, r.Kind = is.ID, is.Identifier, KindTracker
	if r.writeFailures >= maxWriteFailures {
		o.pause("tracker writes are failing: " + r.Error)
	}
	o.schedule(r)
}

// FireDueRetries handles every retry whose time has come.
func (o *Orchestrator) FireDueRetries(ctx context.Context) {
	now := o.now()
	var due []string
	for id, r := range o.retries {
		if !r.DueAt.After(now) {
			due = append(due, id)
		}
	}
	sort.Slice(due, func(a, b int) bool {
		x, y := o.retries[due[a]], o.retries[due[b]]
		if !x.DueAt.Equal(y.DueAt) {
			return x.DueAt.Before(y.DueAt)
		}
		return x.Identifier < y.Identifier
	})
	for _, id := range due {
		o.onRetryTimer(ctx, id)
	}
	if len(due) > 0 {
		o.publish()
	}
}

func (o *Orchestrator) release(id string) {
	delete(o.claimed, id)
	delete(o.retries, id)
}

func (o *Orchestrator) onRetryTimer(ctx context.Context, id string) {
	r := o.retries[id]
	if r == nil {
		return
	}
	delete(o.retries, id)
	requeue := func(attempt int, reason string, due time.Time) {
		n := *r
		n.Attempt, n.Error, n.DueAt = attempt, reason, due
		o.schedule(&n)
	}

	refreshed, err := o.d.Tracker.FetchIssuesByIDs(ctx, []string{id})
	if err != nil {
		requeue(r.Attempt+1, "retry refresh failed", time.Time{})
		return
	}
	if len(refreshed) == 0 {
		o.release(id)
		return
	}
	is := refreshed[0]
	if o.isTerminal(is) {
		o.cleanup(ctx, is)
		o.release(id)
		return
	}
	if r.blockReason != "" {
		// The pending write is the move to the blocked state. If a human
		// already moved the card elsewhere, there is nothing left to do.
		if !o.isActive(is) {
			o.release(id)
			return
		}
		o.block(ctx, is, r.blockReason, r.Error, r.writeFailures)
		return
	}
	if !o.eligible(is) {
		o.release(id)
		return
	}
	poll := time.Duration(o.cfg.Polling.IntervalMS) * time.Millisecond
	if reason := o.gated(); reason != "" {
		// Not a new attempt: keep the number, look again next poll.
		requeue(r.Attempt, "dispatch on hold: "+reason, o.now().Add(poll))
		return
	}
	if o.availableSlots() == 0 || !o.stateSlotFree(is) {
		requeue(r.Attempt+1, "no available orchestrator slots", time.Time{})
		return
	}
	kind := r.Kind
	if kind == KindTracker {
		kind = r.resumeKind
	}
	o.dispatch(ctx, is, r.Attempt, kind, r.Feedback, r.writeFailures)
}

// ---- worker reports -------------------------------------------------------

func (o *Orchestrator) onEvent(id string, gen uint64, ev Event) {
	e := o.running[id]
	if e == nil || e.gen != gen {
		return
	}
	at := ev.At
	if at.IsZero() {
		at = o.now()
	}
	e.lastEvent, e.lastEventAt, e.lastMessage = ev.Kind, at, ev.Message
	e.feed = addFeed(e.feed, ev, at)
	if ev.SessionID != "" {
		e.sessionID = ev.SessionID
	}
	if ev.PID != 0 {
		e.pid = ev.PID
	}
	if ev.Phase != "" {
		e.phase = ev.Phase
	}
	if ev.Usage != nil {
		e.usage = *ev.Usage
	}
	if ev.TurnStart {
		// A run counts against the daily limit once the agent starts: an
		// attempt that a hook or the sandbox stops before then does not.
		if e.turnCount == 0 {
			o.d.Budget.AddRun(o.now())
			o.persist()
		}
		e.turnCount++
	}
}

// account moves a finished entry's usage into the process and day totals.
func (o *Orchestrator) account(e *runEntry) {
	secs := o.now().Sub(e.startedAt).Seconds()
	o.totals.Usage.Input += e.usage.Input
	o.totals.Usage.Output += e.usage.Output
	o.totals.Usage.CacheRead += e.usage.CacheRead
	o.totals.Usage.CacheWrite += e.usage.CacheWrite
	o.totals.SecondsRunning += secs
	o.d.Budget.AddUsage(o.now(), e.usage.Total(), int64(secs))
	o.persist()
}

// onWorkerExit applies an attempt's outcome (SPEC 19.6). A report from an
// attempt that reconciliation already stopped is ignored.
func (o *Orchestrator) onWorkerExit(ctx context.Context, id string, gen uint64, out Outcome) {
	e := o.running[id]
	if e == nil || e.gen != gen {
		return
	}
	delete(o.running, id)
	o.account(e)
	o.stopped(ctx, e)
	is := e.issue

	switch out.Kind {
	case Published:
		if out.Publish == nil {
			o.failed(ctx, e, KindFailure, "publish_record_missing", "", nil)
			return
		}
		o.awaiting[id] = out.Publish
		o.awaitAttempt[id] = e.attempt
		o.comment(ctx, is, fmt.Sprintf("Pull request #%d opened. Waiting for required checks.",
			out.Publish.PRNumber), "")
	case Normal:
		o.completed[id] = true
		o.schedule(&RetryEntry{IssueID: id, Identifier: is.Identifier, Attempt: 1, Kind: KindContinuation})
	case Quota:
		until := out.QuotaResetAt
		if !until.After(o.now()) {
			until = o.now().Add(time.Duration(o.cfg.Budget.QuotaBackoffMS) * time.Millisecond)
		}
		o.d.Budget.SetQuotaBlockedUntil(until)
		o.persist()
		o.schedule(&RetryEntry{IssueID: id, Identifier: is.Identifier, Attempt: max(e.attempt, 1),
			Kind: KindQuota, Error: "agent usage limit reached"})
	case NotLoggedIn:
		o.pause("agent login required")
		o.release(id)
	case Planned:
		o.proposed(ctx, e, out.Plan)
	case Designed:
		o.designed(ctx, e, out.Design)
	case Rejected:
		o.failed(ctx, e, KindGate, out.Category, out.Message, out.Feedback)
	default:
		o.failed(ctx, e, KindFailure, out.Category, out.Message, nil)
	}
}

// failed counts a failure attempt, then retries or blocks.
func (o *Orchestrator) failed(ctx context.Context, e *runEntry, kind, category, message string, feedback []issue.Feedback) {
	o.failIssue(ctx, e.issue, e.attempt, kind, category, message, feedback)
}

func (o *Orchestrator) failIssue(ctx context.Context, is issue.Issue, attempt int, kind, category, message string, feedback []issue.Feedback) {
	n := o.d.Budget.IncFailure(is.ID)
	o.persist()
	// The category alone does not tell a person what to fix.
	o.d.Log.Warn("attempt failed", "issue", is.Identifier, "attempt", attempt, "category", category, "detail", message)
	if n >= o.cfg.Budget.MaxFailureAttempts {
		o.block(ctx, is, category, blockDetail(message, feedback), 0)
		return
	}
	o.schedule(&RetryEntry{IssueID: is.ID, Identifier: is.Identifier, Attempt: attempt + 1,
		Kind: kind, Feedback: feedback, Error: category})
}

// ---- planning -------------------------------------------------------------

// planning reports whether the issue is an idea to plan (SPEC 22).
func (o *Orchestrator) planning(is issue.Issue) bool {
	p := issue.NormalizeState(o.cfg.Tracker.PlanningState)
	return p != "" && issue.NormalizeState(is.State) == p
}

// proposed hands the result of a planning run to the owner. The card is
// moved to the hand-off state first: if that write fails, the idea would
// otherwise be planned again on every poll, with a new comment each time.
func (o *Orchestrator) proposed(ctx context.Context, e *runEntry, p *plan.Plan) {
	is := e.issue
	if p == nil || o.d.Plans == nil {
		o.failed(ctx, e, KindFailure, "plan_missing", "", nil)
		return
	}
	if err := o.d.Tracker.SetState(ctx, is, o.cfg.Tracker.HandoffState); err != nil {
		o.failed(ctx, e, KindFailure, "tracker_write_failed", err.Error(), nil)
		return
	}
	o.d.Budget.ClearIssue(is.ID)
	o.persist()
	o.release(is.ID)
	if err := o.d.Plans.Propose(ctx, is, p); err != nil {
		o.d.Log.Warn("plan not posted", "issue", is.Identifier, "error", err)
		o.comment(ctx, is, "The planning run finished, but its result could not be posted here. Move the card back to "+
			o.cfg.Tracker.PlanningState+" to plan again.", "")
		return
	}
	if len(p.Tasks) == 0 {
		o.comment(ctx, is, "The planning agent has questions. They are in a comment below. Answer them, then move the card back to "+
			o.cfg.Tracker.PlanningState+".", "")
		return
	}
	o.comment(ctx, is, fmt.Sprintf("A plan with %d task(s) is in a comment below. Move the card to %s to approve it, or comment and move it back to %s.",
		len(p.Tasks), o.cfg.Tracker.PlannedState, o.cfg.Tracker.PlanningState), "")
}

// designing reports whether the issue is an idea to make a design for
// (SPEC 27).
func (o *Orchestrator) designing(is issue.Issue) bool {
	d := issue.NormalizeState(o.cfg.Tracker.DesignState)
	return d != "" && issue.NormalizeState(is.State) == d
}

// designed hands a published design to the owner. As with a plan, the card
// is moved first, so that a failed write cannot start the design again.
func (o *Orchestrator) designed(ctx context.Context, e *runEntry, d *plan.Design) {
	is := e.issue
	if d == nil || o.d.Designs == nil {
		o.failed(ctx, e, KindFailure, "design_missing", "", nil)
		return
	}
	if err := o.d.Tracker.SetState(ctx, is, o.cfg.Tracker.HandoffState); err != nil {
		o.failed(ctx, e, KindFailure, "tracker_write_failed", err.Error(), nil)
		return
	}
	o.d.Budget.ClearIssue(is.ID)
	o.persist()
	o.release(is.ID)
	if err := o.d.Designs.Designed(ctx, is, *d); err != nil {
		o.d.Log.Warn("design not recorded", "issue", is.Identifier, "error", err)
		o.comment(ctx, is, fmt.Sprintf("The design is in pull request #%d, but this machine could not record it. Move the card back to %s to try again.",
			d.PRNumber, o.cfg.Tracker.DesignState), "")
	}
}

// watchDesign looks at the design pull request of an idea that waits for
// the owner. Comments on it send the idea back to the design state, the
// way comments on a ticket's pull request send it back to work.
func (o *Orchestrator) watchDesign(ctx context.Context, is issue.Issue) {
	if o.d.Designs == nil || o.cfg.Tracker.DesignState == "" || !o.d.Designs.HasDesign(is.ID) {
		return
	}
	di := plan.DesignIssue(is)
	st, err := o.d.Reviews.Review(ctx, di)
	if err != nil {
		o.d.Log.Warn("design pull request could not be read, will retry", "issue", is.Identifier, "error", err)
		return
	}
	switch {
	case st.State == ReviewFeedback:
	case st.State == ReviewChangesRequested && o.cfg.Publish.ReworkOnChangesRequested:
	default:
		return
	}
	if err := o.d.Tracker.SetState(ctx, is, o.cfg.Tracker.DesignState); err != nil {
		o.d.Log.Warn("design state not written, will retry", "issue", is.Identifier, "error", err)
		return
	}
	if st.State == ReviewFeedback {
		if err := o.d.Reviews.Acknowledge(ctx, di, st); err != nil {
			o.d.Log.Warn("comments on the design pull request not acknowledged", "issue", is.Identifier, "error", err)
		}
	}
	o.d.Log.Info("status", "issue", is.Identifier,
		"text", fmt.Sprintf("New comments on design pull request #%d. Back to the design.", st.PRNumber))
}

// watchPlans turns approved plans into tasks. An idea is approved by the
// owner moving its card to the planned state; the service never does that.
func (o *Orchestrator) watchPlans(ctx context.Context) {
	if o.d.Plans == nil || o.cfg.Tracker.PlanningState == "" {
		return
	}
	interval := time.Duration(o.cfg.Publish.ChecksPollIntervalMS) * time.Millisecond
	if !o.plansAt.IsZero() && o.now().Sub(o.plansAt) < interval {
		return
	}
	o.plansAt = o.now()
	issues, err := o.d.Tracker.FetchIssuesByStates(ctx, []string{o.cfg.Tracker.PlannedState})
	if err != nil {
		o.d.Log.Warn("approved plans could not be read, will retry", "error", err)
		return
	}
	seen := map[string]bool{}
	for _, is := range issues {
		// Only ideas the owner wrote, routed to this worker.
		if !o.routable(is) || o.claimed[is.ID] {
			continue
		}
		seen[is.ID] = true
		summary, err := o.d.Plans.Materialize(ctx, is)
		note := ""
		var waiting *plan.WaitingForDesign
		switch {
		case errors.Is(err, plan.ErrNoPlan):
			note = "This card is in " + o.cfg.Tracker.PlannedState + ", but this machine has no plan for it that you have seen. Move it to " +
				o.cfg.Tracker.PlanningState + " to have it planned."
		case errors.Is(err, plan.ErrStale):
			note = "The idea was edited after its plan was proposed, so the plan is not used. Move the card to " +
				o.cfg.Tracker.PlanningState + " to plan the new text."
		case errors.As(err, &waiting):
			note = fmt.Sprintf("The tasks are created once the design in pull request #%d is merged. Merge it to approve the design, "+
				"or close it to go on without a design.", waiting.PRNumber)
		case err != nil:
			o.d.Log.Warn("tasks of an approved plan not created yet, will retry", "issue", is.Identifier, "error", err)
			note = "The tasks of the approved plan could not all be created yet. The service keeps trying. Last error: `" +
				safeCategory(tracker.CategoryOf(err)) + "`."
		case summary != "":
			delete(o.planNoted, is.ID)
			o.comment(ctx, is, summary, "")
			o.designDone(ctx, is)
			continue
		default:
			o.finishIdea(ctx, is)
			continue
		}
		if !o.planNoted[is.ID] {
			o.planNoted[is.ID] = true
			o.comment(ctx, is, note, "")
		}
	}
	for id := range o.planNoted {
		if !seen[id] {
			delete(o.planNoted, id)
		}
	}
}

// finishIdea moves an idea to Done once every task of its plan is done.
// The tasks are what was built; the idea's card would otherwise stay in
// Planned for good.
func (o *Orchestrator) finishIdea(ctx context.Context, is issue.Issue) {
	f, ok := o.d.Plans.(PlanFinisher)
	if !ok || o.cfg.Tracker.DoneState == "" {
		return
	}
	done, n, err := f.Finished(ctx, is)
	if err != nil {
		o.d.Log.Warn("the tasks of a plan could not be read, will retry", "issue", is.Identifier, "error", err)
		return
	}
	if !done {
		return
	}
	if err := o.d.Tracker.SetState(ctx, is, o.cfg.Tracker.DoneState); err != nil {
		o.d.Log.Warn("done state not written, will retry", "issue", is.Identifier, "error", err)
		return
	}
	o.comment(ctx, is, fmt.Sprintf("All %d task(s) of the plan are done, so this idea is done too.", n), "")
}

// designDone tidies up after the tasks of a plan with a design were
// created: the design's branch and workspace are not needed any more.
func (o *Orchestrator) designDone(ctx context.Context, is issue.Issue) {
	if o.d.Designs == nil || !o.d.Designs.HasDesign(is.ID) {
		return
	}
	di := plan.DesignIssue(is)
	if o.d.Reviews != nil {
		if err := o.d.Reviews.DeleteBranch(ctx, di); err != nil {
			o.d.Log.Warn("design branch not deleted", "issue", is.Identifier, "error", err)
		}
	}
	if o.d.Workspaces != nil {
		if err := o.d.Workspaces.Remove(ctx, di.Identifier); err != nil {
			o.d.Log.Warn("design workspace not removed", "issue", is.Identifier, "error", err)
		}
	}
}

// ---- checks gate ----------------------------------------------------------

// dropAwaiting forgets a published record. The claim is not touched.
func (o *Orchestrator) dropAwaiting(id string) {
	delete(o.awaiting, id)
	delete(o.checkedAt, id)
	delete(o.awaitAttempt, id)
	delete(o.noChecksNoted, id)
}

// noChecksAfter is how long after publishing a pull request with no check
// at all is pointed out on the card.
const noChecksAfter = 10 * time.Minute

// watchChecks looks at every published pull request that is due for a look
// (SPEC 15.6). A ticket reaches the hand-off state only from here: after
// the required checks have passed on exactly the commit that was pushed.
func (o *Orchestrator) watchChecks(ctx context.Context) {
	if o.d.Checks == nil || len(o.awaiting) == 0 {
		return
	}
	interval := time.Duration(o.cfg.Publish.ChecksPollIntervalMS) * time.Millisecond
	ids := make([]string, 0, len(o.awaiting))
	for id := range o.awaiting {
		if at, ok := o.checkedAt[id]; ok && o.now().Sub(at) < interval {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return
	}
	sort.Strings(ids)
	fresh, err := o.d.Tracker.FetchIssuesByIDs(ctx, ids)
	if err != nil {
		o.d.Log.Warn("issue refresh failed, checks are looked at next tick", "error", err)
		return
	}
	byID := map[string]issue.Issue{}
	for _, is := range fresh {
		byID[is.ID] = is
	}
	for _, id := range ids {
		rec := o.awaiting[id]
		o.checkedAt[id] = o.now()
		is, found := byID[id]
		inProgress := issue.NormalizeState(is.State) == issue.NormalizeState(o.cfg.Tracker.InProgressState)
		switch {
		case !found, o.isTerminal(is):
			// Closed or deleted while waiting. The pull request is left alone.
			o.dropAwaiting(id)
			o.release(id)
			if found {
				o.cleanup(ctx, is)
			}
			continue
		case !inProgress:
			// A person moved the ticket. Their state stands: it is not
			// overwritten with the hand-off state when checks pass.
			o.dropAwaiting(id)
			o.release(id)
			continue
		}

		st, err := o.d.Checks.Check(ctx, *rec)
		if err != nil {
			o.d.Log.Warn("checks could not be read, will retry", "issue", rec.Identifier, "error", err)
			if o.now().After(rec.ChecksDeadline) {
				o.dropAwaiting(id)
				o.block(ctx, is, "checks_timeout", "", 0)
			}
			continue
		}
		switch st.State {
		case ChecksPassed:
			if err := o.d.Checks.MarkReady(ctx, *rec); err != nil {
				o.d.Log.Warn("pull request not marked ready, will retry", "issue", rec.Identifier, "error", err)
				continue
			}
			if err := o.d.Tracker.SetState(ctx, is, o.cfg.Tracker.HandoffState); err != nil {
				o.d.Log.Warn("hand-off state not written, will retry", "issue", rec.Identifier, "error", err)
				continue
			}
			o.d.Budget.ClearIssue(id)
			o.persist()
			o.comment(ctx, is, fmt.Sprintf("Pull request #%d passed its required checks and is ready for review.", rec.PRNumber), "")
			o.dropAwaiting(id)
			o.release(id)
		case ChecksFailed:
			attempt := o.awaitAttempt[id]
			o.dropAwaiting(id)
			o.failIssue(ctx, is, attempt, KindGate, "checks_failed", st.Detail, st.Feedback)
		case ChecksForeignPush:
			// Someone else pushed to the branch. What was verified is not
			// what is there now, so the service stops and a person decides.
			o.dropAwaiting(id)
			o.block(ctx, is, "foreign_push", "", 0)
		case ChecksClosed:
			o.dropAwaiting(id)
			o.block(ctx, is, "pull_request_closed", "", 0)
		default:
			if o.now().After(rec.ChecksDeadline) {
				o.dropAwaiting(id)
				o.block(ctx, is, "checks_timeout", st.Detail, 0)
				continue
			}
			// Waiting is right while CI runs, but a repository without CI
			// would only time out after the whole wait. Say so early; no
			// check is still never read as passed.
			published := rec.ChecksDeadline.Add(-time.Duration(o.cfg.Publish.ChecksTimeoutMS) * time.Millisecond)
			if st.NoneReported && !o.noChecksNoted[id] && o.now().Sub(published) >= noChecksAfter {
				o.noChecksNoted[id] = true
				o.comment(ctx, is, fmt.Sprintf("No check has started on pull request #%d after %d minutes. "+
					"The service waits for the repository's CI to pass before it hands the pull request to review, "+
					"and gives up at %s. If this repository has no CI, add `publish: required_checks: none` to the "+
					"workflow file; the service then hands pull requests over without waiting.",
					rec.PRNumber, int(noChecksAfter/time.Minute), rec.ChecksDeadline.UTC().Format("15:04 UTC")), "")
			}
		}
	}
}

// reworkState is where a ticket goes when a reviewer asks for changes.
const reworkState = "Rework"

// watchHandoff looks at the tickets waiting for a person (SPEC 15.8). It
// keeps no state of its own between runs beyond a throttle: after a restart
// it finds the same tickets on the board again.
func (o *Orchestrator) watchHandoff(ctx context.Context) {
	if o.d.Reviews == nil {
		return
	}
	interval := time.Duration(o.cfg.Publish.ChecksPollIntervalMS) * time.Millisecond
	if !o.handoffAt.IsZero() && o.now().Sub(o.handoffAt) < interval {
		return
	}
	o.handoffAt = o.now()
	issues, err := o.d.Tracker.FetchIssuesByStates(ctx, []string{o.cfg.Tracker.HandoffState})
	if err != nil {
		o.d.Log.Warn("hand-off tickets could not be read, will retry", "error", err)
		return
	}
	seen := map[string]bool{}
	for _, is := range issues {
		// Another worker's tickets, and tickets this worker is busy with,
		// are left alone.
		if !is.HasLabels(o.cfg.Tracker.RequiredLabels) || o.claimed[is.ID] {
			continue
		}
		seen[is.ID] = true
		st, err := o.d.Reviews.Review(ctx, is)
		if err != nil {
			o.d.Log.Warn("pull request could not be read, will retry", "issue", is.Identifier, "error", err)
			continue
		}
		switch st.State {
		case ReviewNone:
			delete(o.closedNoted, is.ID)
			o.watchDesign(ctx, is)
		case ReviewMerged:
			// The branch goes first. If the state write then fails, the
			// next run finds the merged pull request again and retries.
			if err := o.d.Reviews.DeleteBranch(ctx, is); err != nil {
				o.d.Log.Warn("merged branch not deleted", "issue", is.Identifier, "error", err)
			}
			if err := o.d.Tracker.SetState(ctx, is, o.cfg.Tracker.DoneState); err != nil {
				o.d.Log.Warn("done state not written, will retry", "issue", is.Identifier, "error", err)
				continue
			}
			o.cleanup(ctx, is)
			o.comment(ctx, is, fmt.Sprintf("Pull request #%d was merged.", st.PRNumber), "")
			delete(o.closedNoted, is.ID)
		case ReviewClosed:
			if !o.closedNoted[is.ID] {
				o.closedNoted[is.ID] = true
				o.comment(ctx, is, fmt.Sprintf("Pull request #%d was closed without being merged. "+
					"The ticket stays where it is: move it to Rework to have it picked up again, or close it.", st.PRNumber), "")
			}
		case ReviewFeedback:
			// The owner commented on the pull request. The ticket goes
			// back to work without the owner moving the card. The state
			// is written first: if the note on the pull request then
			// fails, the comments start one more run, which is better
			// than losing them.
			delete(o.closedNoted, is.ID)
			if !o.hasActiveState(reworkState) {
				continue
			}
			if err := o.d.Tracker.SetState(ctx, is, reworkState); err != nil {
				o.d.Log.Warn("rework state not written, will retry", "issue", is.Identifier, "error", err)
				continue
			}
			if err := o.d.Reviews.Acknowledge(ctx, is, st); err != nil {
				o.d.Log.Warn("comments on the pull request not acknowledged", "issue", is.Identifier, "error", err)
			}
			o.d.Log.Info("status", "issue", is.Identifier,
				"text", fmt.Sprintf("%d new comment(s) on pull request #%d. Back to work.", st.FeedbackCount, st.PRNumber))
		case ReviewChangesRequested:
			delete(o.closedNoted, is.ID)
			if !o.cfg.Publish.ReworkOnChangesRequested || !o.hasActiveState(reworkState) {
				continue
			}
			if err := o.d.Tracker.SetState(ctx, is, reworkState); err != nil {
				o.d.Log.Warn("rework state not written, will retry", "issue", is.Identifier, "error", err)
			}
		default:
			delete(o.closedNoted, is.ID)
		}
	}
	for id := range o.closedNoted {
		if !seen[id] {
			delete(o.closedNoted, id)
		}
	}
}

func (o *Orchestrator) hasActiveState(state string) bool {
	for _, s := range o.cfg.Tracker.ActiveStates {
		if issue.NormalizeState(s) == issue.NormalizeState(state) {
			return true
		}
	}
	return false
}

// block moves the issue to the blocked state and releases the claim. If the
// write fails the claim is held and the write is retried.
func (o *Orchestrator) block(ctx context.Context, is issue.Issue, reason, detail string, writeFailures int) {
	if err := o.d.Tracker.SetState(ctx, is, o.cfg.Tracker.BlockedState); err != nil {
		o.trackerRetry(is, &RetryEntry{blockReason: reason, writeFailures: writeFailures + 1,
			Error: "tracker write failed: " + err.Error()})
		return
	}
	o.d.Budget.MarkBlocked(is.ID, o.now())
	o.persist()
	text := o.blockLead(reason) + " Move the card back to an active state to retry with a fresh budget."
	o.comment(ctx, is, text, detail)
	// The status comment is edited in place, so it stays where it was
	// first posted and the edit notifies no one. The reason also goes in a
	// new comment, below the others, where the owner looks.
	if p, ok := o.d.Tracker.(interface {
		PostComment(context.Context, issue.Issue, string) error
	}); ok {
		body := text
		if detail != "" {
			body += "\n\n" + tracker.Fence(detail)
		}
		if err := p.PostComment(ctx, is, body); err != nil {
			o.d.Log.Warn("blocked note not posted", "issue", is.Identifier, "error", err)
		}
	}
	o.release(is.ID)
}

// blockLead says why a ticket was blocked. Most reasons are the last of
// several failed attempts; the ones the pull request watch gives are not,
// and saying "repeated failures" for them sends a person looking for
// failures that did not happen.
func (o *Orchestrator) blockLead(reason string) string {
	switch reason {
	case "checks_timeout":
		return fmt.Sprintf("Blocked: the checks on the pull request did not finish within %d minutes "+
			"(`checks_timeout`, set by `publish.checks_timeout_ms`). The list below says which checks it waited for. "+
			"A check that never started usually means the CI workflow did not run for this branch, "+
			"or a required check's name does not match the job's name.",
			o.cfg.Publish.ChecksTimeoutMS/60000)
	case "foreign_push":
		return "Blocked: someone else pushed to the pull request's branch (`foreign_push`), " +
			"so what the service checked is not what is there now."
	case "pull_request_closed":
		return "Blocked: the pull request was closed before its checks passed (`pull_request_closed`)."
	}
	return "Blocked after repeated failures. Last error: `" + safeCategory(reason) + "`."
}

// blockDetail is what the blocked comment shows under the category, so a
// person can see what to fix without the service's log. A rejection by a
// gate carries its reason in the feedback (the end of the verify output, a
// failed check), not in the message. A long text keeps its end: an agent's
// report ends with the reason it stopped, and a check's output with the
// error.
func blockDetail(message string, feedback []issue.Feedback) string {
	text := strings.TrimSpace(message)
	if text == "" {
		parts := make([]string, 0, len(feedback))
		for _, f := range feedback {
			if b := strings.TrimSpace(f.Body); b != "" {
				parts = append(parts, b)
			}
		}
		text = strings.Join(parts, "\n\n")
	}
	r := []rune(text)
	if len(r) > tracker.MaxUntrustedChars-1 {
		text = "…" + string(r[len(r)-(tracker.MaxUntrustedChars-1):])
	}
	return text
}

// safeCategory keeps only the characters of an error category, so that a
// category supplied by a worker cannot carry markup into a comment.
func safeCategory(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s) && len(out) < 64; i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return "unknown"
	}
	return string(out)
}

// ---- reconciliation -------------------------------------------------------

// reconcile checks running work against the clock and the board (SPEC 8.5).
func (o *Orchestrator) reconcile(ctx context.Context) {
	ids := make([]string, 0, len(o.running))
	for id := range o.running {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	// Part A: stalls. Verify and publish have their own timeouts.
	if stall := time.Duration(o.cfg.Claude.StallTimeoutMS) * time.Millisecond; stall > 0 {
		for _, id := range ids {
			e := o.running[id]
			if e.phase == Verifying || e.phase == Publishing {
				continue
			}
			last := e.lastEventAt
			if last.IsZero() {
				last = e.startedAt
			}
			if o.now().Sub(last) > stall {
				o.d.Log.Warn("run stalled, terminating", "issue", e.issue.Identifier)
				e.handle.Terminate()
				delete(o.running, id)
				o.account(e)
				o.stopped(ctx, e)
				o.failed(ctx, e, KindFailure, "stalled", "", nil)
			}
		}
	}

	ids = ids[:0]
	for id := range o.running {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return
	}
	sort.Strings(ids)

	// Part B: board state. Part C: content pinning.
	refreshed, err := o.d.Tracker.FetchIssuesByIDs(ctx, ids)
	if err != nil {
		o.d.Log.Warn("running-state refresh failed, keeping workers", "error", err)
		return
	}
	seen := map[string]bool{}
	for _, is := range refreshed {
		e := o.running[is.ID]
		if e == nil {
			continue
		}
		seen[is.ID] = true
		switch {
		case o.isTerminal(is):
			o.terminate(ctx, is.ID, true, "issue reached a terminal state")
		case o.isActive(is) && o.routable(is):
			if is.ContentHash != e.dispatchHash {
				// Released, so a later tick dispatches it as a fresh first
				// attempt for the text as it now stands.
				o.terminate(ctx, is.ID, false, "issue text changed after dispatch")
			} else {
				e.issue = is
			}
		default:
			o.terminate(ctx, is.ID, false, "issue is no longer active or routable")
		}
	}
	for _, id := range ids {
		if !seen[id] {
			o.terminate(ctx, id, false, "issue is no longer visible")
		}
	}
}

// terminate stops a running attempt and releases its claim.
func (o *Orchestrator) terminate(ctx context.Context, id string, cleanupWorkspace bool, why string) {
	e := o.running[id]
	if e == nil {
		return
	}
	o.d.Log.Info("terminating run", "issue", e.issue.Identifier, "reason", why)
	e.handle.Terminate()
	delete(o.running, id)
	o.account(e)
	o.stopped(ctx, e)
	o.release(id)
	if cleanupWorkspace {
		o.cleanup(ctx, e.issue)
	}
}

// cleanup removes the workspace and the budget entries of a finished issue.
func (o *Orchestrator) cleanup(ctx context.Context, is issue.Issue) {
	if o.d.Workspaces != nil {
		if err := o.d.Workspaces.Remove(ctx, is.Identifier); err != nil {
			o.d.Log.Warn("workspace not removed", "issue", is.Identifier, "error", err)
		}
	}
	o.d.Budget.ClearIssue(is.ID)
	o.persist()
}

// ---- observation ----------------------------------------------------------

// Snapshot copies the scheduling state for status output.
func (o *Orchestrator) Snapshot() Snapshot {
	s := Snapshot{Totals: o.totals, ConfigError: o.configError, GeneratedAt: o.now(), DailyLimit: o.dailyLimit(),
		Worker: o.cfg.Worker.Name, Today: o.d.Budget.Today(o.now()),
		QuotaBlockedUntil: o.d.Budget.QuotaBlockedUntil()}
	s.Paused, s.PauseReason = o.d.Budget.Paused()
	for id, e := range o.running {
		s.Running = append(s.Running, RunningView{
			IssueID: id, Identifier: e.issue.Identifier, State: e.issue.State,
			SessionID: e.sessionID, PID: e.pid, Phase: e.phase, Attempt: e.attempt,
			TurnCount: e.turnCount, StartedAt: e.startedAt, LastEvent: e.lastEvent,
			LastMessage: e.lastMessage, LastEventAt: e.lastEventAt, Usage: e.usage,
			FailureAttempts: o.d.Budget.Failures(id), Feed: append([]FeedItem(nil), e.feed...),
		})
	}
	sort.Slice(s.Running, func(a, b int) bool { return s.Running[a].Identifier < s.Running[b].Identifier })
	for _, r := range o.retries {
		s.Retrying = append(s.Retrying, *r)
	}
	sort.Slice(s.Retrying, func(a, b int) bool { return s.Retrying[a].Identifier < s.Retrying[b].Identifier })
	for _, p := range o.awaiting {
		s.AwaitingChecks = append(s.AwaitingChecks, *p)
	}
	sort.Slice(s.AwaitingChecks, func(a, b int) bool {
		return s.AwaitingChecks[a].Identifier < s.AwaitingChecks[b].Identifier
	})
	return s
}

// addFeed appends an event to a feed when it says something worth showing,
// and keeps the last FeedLen lines.
func addFeed(feed []FeedItem, ev Event, at time.Time) []FeedItem {
	if ev.Message == "" || !feedKinds[ev.Kind] {
		return feed
	}
	feed = append(feed, FeedItem{At: at, Kind: ev.Kind, Message: ev.Message})
	if len(feed) > FeedLen {
		feed = append([]FeedItem(nil), feed[len(feed)-FeedLen:]...)
	}
	return feed
}
