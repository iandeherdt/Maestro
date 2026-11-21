// Package worker runs one attempt on one issue (SPEC sections 7.4, 10.8
// and 19.5): prepare the workspace, prompt the agent, and after every turn
// decide between another turn, the verify gate, publication and failure.
//
// The agent never decides that its work is fit to publish. Its status line
// can ask for more turns or give up; it cannot skip a gate.
package worker

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"time"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
	"github.com/iandeherdt/Maestro/internal/plan"
	"github.com/iandeherdt/Maestro/internal/prompt"
	"github.com/iandeherdt/Maestro/internal/tracker"
	"github.com/iandeherdt/Maestro/internal/workspace"
)

// Failure categories decided here. Agent categories come from the runner.
const (
	RefreshError      = "issue_state_refresh_error"
	ExportError       = "export_error"
	VerifyFailed      = "verify_failed"
	VerifyError       = "verify_error"
	PublishError      = "publish_error"
	NoChanges         = "no_changes"
	MaxTurnsExhausted = "max_turns_exhausted"
	DeclaredBlocked   = "agent_declared_blocked"
	QuotaExhausted    = "agent_quota_exhausted"
	NotLoggedIn       = "agent_not_logged_in"
	TurnFailed        = "turn_failed"
	// SessionUnknown: a continuation turn could not resume its session.
	// The worker starts a fresh session with the full prompt, once.
	SessionUnknown = "session_unknown"
	// Planning runs (SPEC 22).
	PlanBriefError      = "plan_brief_error"
	PlanMalformed       = "plan_malformed"
	afterRunGracePeriod = 2 * time.Minute
	// maxOwnerComments is how many of the owner's most recent comments on a
	// ticket are given to the agent.
	maxOwnerComments = 20
)

// Turn is one agent turn to run.
type Turn struct {
	WorkspaceKey string
	Prompt       string
	// ResumeSessionID is empty for the first turn of an attempt.
	ResumeSessionID string
}

// TurnResult is a turn that ended normally.
type TurnResult struct {
	SessionID string
	// ResultText is the agent's final message. Untrusted.
	ResultText string
}

// AgentError is a turn that did not end normally (SPEC 10.7).
type AgentError struct {
	Category string
	Msg      string
	// QuotaResetAt is set when the CLI reported when the limit resets.
	QuotaResetAt time.Time
}

func (e *AgentError) Error() string { return e.Category + ": " + e.Msg }

// Runner drives the agent for one turn inside the sandbox and forwards its
// events. When ctx ends it must kill the whole sandbox unit.
type Runner interface {
	RunTurn(ctx context.Context, t Turn, onEvent func(orchestrator.Event)) (TurnResult, error)
}

// Export is the result of exporting the workspace's commits (SPEC 15.1).
type Export struct{ NoChanges bool }

// VerifyResult is the outcome of the verify command (SPEC 14).
type VerifyResult struct {
	Passed     bool
	ExitStatus int
	// OutputTail is the end of the command's output. Untrusted.
	OutputTail string
	// Screenshots were taken by the service after the command passed
	// (SPEC 24). ScreenshotNote says in the service's own words why there
	// are none, or fewer than asked for.
	Screenshots    []Screenshot
	ScreenshotNote string
}

// Screenshot is one picture of the work. PNG was decoded and written again
// by the service, so it holds pixels and nothing else. What the pixels
// show comes from the agent's code.
type Screenshot struct {
	// Caption is a configured page, or a cleaned file name.
	Caption string
	PNG     []byte
}

// Rejection is a gate refusing the work: an import or diff-policy denial.
type Rejection struct {
	Category string
	Feedback []issue.Feedback
}

// Gates are the steps between the agent's commits and a pull request. All
// of them are run by the service, none by the agent.
type Gates interface {
	Export(ctx context.Context, ws workspace.Workspace) (Export, error)
	Verify(ctx context.Context, ws workspace.Workspace) (VerifyResult, error)
	// Publish imports the exported commits, applies the diff policy and the
	// secret scan, pushes and opens the pull request. Exactly one of the
	// record and the rejection is set when err is nil.
	Publish(ctx context.Context, is issue.Issue, ws workspace.Workspace, v VerifyResult) (*orchestrator.PublishRecord, *Rejection, error)
}

// Workspaces is the part of the workspace manager an attempt needs.
type Workspaces interface {
	Ensure(ctx context.Context, is issue.Issue) (workspace.Workspace, error)
	RunHook(ctx context.Context, name, workspaceKey string) error
	AfterRunBestEffort(ctx context.Context, workspaceKey string)
}

// Settings is the configuration an attempt runs with. It is read once, when
// the attempt starts.
type Settings struct {
	Config *config.Config
	Prompt *prompt.Builder
}

// Planning supplies what a planning run needs besides the idea: the
// machines tasks can be given to and the conversation so far.
type Planning interface {
	Brief(ctx context.Context, is issue.Issue) (plan.Brief, error)
}

// PullRequestNotes supplies what the owner wrote on the ticket's pull
// request and has not been answered by a commit yet (SPEC 15.8).
type PullRequestNotes interface {
	Feedback(ctx context.Context, is issue.Issue) ([]issue.Feedback, error)
}

// Worker implements orchestrator.Worker.
type Worker struct {
	Settings   func() Settings
	Tracker    tracker.Tracker
	Workspaces Workspaces
	Runner     Runner
	Gates      Gates
	// Planning is optional. Without it a planning attempt fails.
	Planning Planning
	// Notes is optional. Without it comments on a pull request do not
	// reach the agent.
	Notes PullRequestNotes
}

type handle struct {
	cancel context.CancelFunc
	once   sync.Once
}

func (h *handle) Terminate() { h.once.Do(h.cancel) }

// Start launches the attempt and returns at once.
func (w *Worker) Start(a orchestrator.Attempt, sink orchestrator.Sink) (orchestrator.Handle, error) {
	if w.Settings == nil || w.Tracker == nil || w.Workspaces == nil || w.Runner == nil || w.Gates == nil {
		return nil, errors.New("worker is not fully configured")
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &handle{cancel: cancel}
	go func() {
		defer cancel()
		sink.Exit(w.run(ctx, a, sink))
	}()
	return h, nil
}

func failed(category string, err error) orchestrator.Outcome {
	out := orchestrator.Outcome{Kind: orchestrator.Failed, Category: category}
	if err != nil {
		out.Message = err.Error()
	}
	return out
}

var statusRE = regexp.MustCompile(`(?m)^[ \t]*SYMPHONY_STATUS:[ \t]*(done|continue|blocked)[ \t]*\r?$`)

// DeclaredStatus reads the agent's status from its final message: the last
// line of the form "SYMPHONY_STATUS: done|continue|blocked". No such line
// means done.
func DeclaredStatus(resultText string) string {
	m := statusRE.FindAllStringSubmatch(resultText, -1)
	if len(m) == 0 {
		return "done"
	}
	return m[len(m)-1][1]
}

func (w *Worker) run(ctx context.Context, a orchestrator.Attempt, sink orchestrator.Sink) orchestrator.Outcome {
	set := w.Settings()
	cfg := set.Config
	is := a.Issue
	phase := func(p orchestrator.Phase) { sink.Event(orchestrator.Event{Kind: "phase", Phase: p}) }

	phase(orchestrator.PreparingWorkspace)
	ws, err := w.Workspaces.Ensure(ctx, is)
	if err != nil {
		return failed(orDefault(workspace.CategoryOf(err), "workspace_error"), err)
	}
	// From here on after_run runs on every exit path, also when the attempt
	// was cancelled, so it gets a context of its own.
	defer func() {
		phase(orchestrator.Finishing)
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), afterRunGracePeriod)
		defer cancel()
		w.Workspaces.AfterRunBestEffort(actx, ws.Key)
	}()

	if err := w.Workspaces.RunHook(ctx, workspace.BeforeRun, ws.Key); err != nil {
		return failed(orDefault(workspace.CategoryOf(err), workspace.HookFailed), err)
	}
	if a.Planning {
		return w.plan(ctx, a, set, ws, sink)
	}

	// What the owner wrote on the ticket belongs to the task: a
	// clarification, or what to change when the ticket came back for
	// rework. Only allowlisted commenters are returned.
	comments, err := w.Tracker.FetchFeedback(ctx, is, time.Time{})
	if err != nil {
		return failed(RefreshError, err)
	}
	if len(comments) > maxOwnerComments {
		comments = comments[len(comments)-maxOwnerComments:]
	}
	feedback := append([]issue.Feedback(nil), a.Feedback...)
	for _, c := range comments {
		feedback = append(feedback, issue.Feedback{Source: "tracker", Author: c.Author, Body: c.Body})
	}
	if w.Notes != nil {
		notes, err := w.Notes.Feedback(ctx, is)
		if err != nil {
			return failed(RefreshError, err)
		}
		feedback = append(feedback, notes...)
	}

	maxTurns := int(cfg.Agent.MaxTurns)
	sessionID, reason := "", ""
	sessionReset := false
	var lastVerify *prompt.Verify
	for turn := 1; ; turn++ {
		if ctx.Err() != nil {
			return orchestrator.Outcome{Kind: orchestrator.Normal}
		}
		phase(orchestrator.BuildingPrompt)
		first := func() (string, error) {
			return set.Prompt.First(prompt.Input{
				Issue: is, Attempt: a.Number, RetryKind: a.RetryKind, Feedback: feedback,
				WorkerName: cfg.Worker.Name, RepoName: cfg.Workspace.Repo,
				BaseBranch: cfg.Workspace.BaseBranch, Branch: ws.Branch,
			})
		}
		var text string
		if turn == 1 {
			text, err = first()
		} else {
			text, err = set.Prompt.Continuation(turn, maxTurns, reason, lastVerify)
		}
		if err != nil {
			return failed(prompt.RenderError, err)
		}

		phase(orchestrator.LaunchingAgentProcess)
		sink.Event(orchestrator.Event{Kind: "turn_start", TurnStart: true})
		res, err := w.Runner.RunTurn(ctx, Turn{WorkspaceKey: ws.Key, Prompt: text, ResumeSessionID: sessionID}, sink.Event)
		var lost *AgentError
		if errors.As(err, &lost) && lost.Category == SessionUnknown && sessionID != "" && !sessionReset && ctx.Err() == nil {
			// The agent lost the session (SPEC 10.2). The work in the
			// workspace is still there, so a new session gets the full
			// prompt and carries on. This happens once per attempt.
			sessionReset = true
			sink.Event(orchestrator.Event{Kind: "session_reset"})
			if text, err = first(); err != nil {
				return failed(prompt.RenderError, err)
			}
			res, err = w.Runner.RunTurn(ctx, Turn{WorkspaceKey: ws.Key, Prompt: text}, sink.Event)
		}
		if err != nil {
			if ctx.Err() != nil {
				return orchestrator.Outcome{Kind: orchestrator.Normal}
			}
			return agentOutcome(err)
		}
		sessionID = res.SessionID

		// Is this still work we should be doing?
		refreshed, err := w.Tracker.FetchIssuesByIDs(ctx, []string{is.ID})
		if err != nil {
			return failed(RefreshError, err)
		}
		if len(refreshed) == 0 || !stillWanted(cfg, refreshed[0]) {
			return orchestrator.Outcome{Kind: orchestrator.Normal}
		}
		// The session was started for one text. If the issue was edited
		// since, nothing from this session is published; the scheduler
		// starts a fresh attempt for the new text.
		if refreshed[0].ContentHash != is.ContentHash {
			return orchestrator.Outcome{Kind: orchestrator.Normal}
		}

		phase(orchestrator.Exporting)
		exp, err := w.Gates.Export(ctx, ws)
		if err != nil {
			return failed(ExportError, err)
		}

		status := DeclaredStatus(res.ResultText)
		if status == "blocked" {
			return orchestrator.Outcome{Kind: orchestrator.Failed, Category: DeclaredBlocked, Message: res.ResultText}
		}
		if exp.NoChanges || status == "continue" {
			if exp.NoChanges && status == "done" {
				return failed(NoChanges, nil)
			}
			if turn >= maxTurns {
				return failed(MaxTurnsExhausted, nil)
			}
			reason, lastVerify = prompt.ReasonContinue, nil
			if exp.NoChanges {
				reason = prompt.ReasonNoChanges
			}
			continue
		}

		phase(orchestrator.Verifying)
		v, err := w.Gates.Verify(ctx, ws)
		if err != nil {
			return failed(VerifyError, err)
		}
		if !v.Passed {
			if turn >= maxTurns {
				return orchestrator.Outcome{Kind: orchestrator.Rejected, Category: VerifyFailed,
					Feedback: []issue.Feedback{{Source: "verify", Body: v.OutputTail}}}
			}
			reason = prompt.ReasonVerifyFailed
			lastVerify = &prompt.Verify{ExitStatus: v.ExitStatus, OutputTail: v.OutputTail}
			continue
		}

		phase(orchestrator.Publishing)
		rec, rej, err := w.Gates.Publish(ctx, is, ws, v)
		switch {
		case err != nil:
			return failed(PublishError, err)
		case rej != nil:
			return orchestrator.Outcome{Kind: orchestrator.Rejected, Category: rej.Category, Feedback: rej.Feedback}
		case rec == nil:
			return failed(PublishError, errors.New("publisher returned neither a record nor a rejection"))
		}
		return orchestrator.Outcome{Kind: orchestrator.Published, Publish: rec}
	}
}

// plan runs a planning attempt: the agent reads the repository and answers
// with questions or with a plan. Nothing is exported, verified or
// published, whatever the agent did in the workspace.
func (w *Worker) plan(ctx context.Context, a orchestrator.Attempt, set Settings, ws workspace.Workspace, sink orchestrator.Sink) orchestrator.Outcome {
	cfg, is := set.Config, a.Issue
	phase := func(p orchestrator.Phase) { sink.Event(orchestrator.Event{Kind: "phase", Phase: p}) }
	if w.Planning == nil {
		return failed(PlanBriefError, errors.New("planning is not set up on this worker"))
	}
	phase(orchestrator.BuildingPrompt)
	brief, err := w.Planning.Brief(ctx, is)
	if err != nil {
		return failed(PlanBriefError, err)
	}
	first := func() (string, error) {
		return set.Prompt.Planning(prompt.PlanInput{Issue: is, RepoName: cfg.Workspace.Repo, Workers: brief.Workers, History: brief.History})
	}
	maxTurns := int(cfg.Agent.MaxTurns)
	sessionID, problem := "", ""
	sessionReset := false
	for turn := 1; ; turn++ {
		if ctx.Err() != nil {
			return orchestrator.Outcome{Kind: orchestrator.Normal}
		}
		var text string
		if turn == 1 {
			if text, err = first(); err != nil {
				return failed(prompt.RenderError, err)
			}
		} else {
			text = set.Prompt.PlanningRetry(turn, maxTurns, problem)
		}
		phase(orchestrator.LaunchingAgentProcess)
		sink.Event(orchestrator.Event{Kind: "turn_start", TurnStart: true})
		res, err := w.Runner.RunTurn(ctx, Turn{WorkspaceKey: ws.Key, Prompt: text, ResumeSessionID: sessionID}, sink.Event)
		var lost *AgentError
		if errors.As(err, &lost) && lost.Category == SessionUnknown && sessionID != "" && !sessionReset && ctx.Err() == nil {
			sessionReset = true
			sink.Event(orchestrator.Event{Kind: "session_reset"})
			if text, err = first(); err != nil {
				return failed(prompt.RenderError, err)
			}
			res, err = w.Runner.RunTurn(ctx, Turn{WorkspaceKey: ws.Key, Prompt: text}, sink.Event)
		}
		if err != nil {
			if ctx.Err() != nil {
				return orchestrator.Outcome{Kind: orchestrator.Normal}
			}
			return agentOutcome(err)
		}
		sessionID = res.SessionID

		// Is this still an idea the owner wants planned, with the same text?
		refreshed, err := w.Tracker.FetchIssuesByIDs(ctx, []string{is.ID})
		if err != nil {
			return failed(RefreshError, err)
		}
		if len(refreshed) == 0 || !stillWanted(cfg, refreshed[0]) || refreshed[0].ContentHash != is.ContentHash ||
			issue.NormalizeState(refreshed[0].State) != issue.NormalizeState(cfg.Tracker.PlanningState) {
			return orchestrator.Outcome{Kind: orchestrator.Normal}
		}

		p, err := plan.Parse(res.ResultText, brief.Workers)
		if err == nil {
			return orchestrator.Outcome{Kind: orchestrator.Planned, Plan: p}
		}
		var fe *plan.FormatError
		if !errors.As(err, &fe) || turn >= maxTurns {
			return failed(PlanMalformed, err)
		}
		problem = fe.Msg
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func agentOutcome(err error) orchestrator.Outcome {
	var ae *AgentError
	if !errors.As(err, &ae) {
		return failed(TurnFailed, err)
	}
	switch ae.Category {
	case QuotaExhausted:
		return orchestrator.Outcome{Kind: orchestrator.Quota, Category: ae.Category, Message: ae.Msg, QuotaResetAt: ae.QuotaResetAt}
	case NotLoggedIn:
		return orchestrator.Outcome{Kind: orchestrator.NotLoggedIn, Category: ae.Category, Message: ae.Msg}
	}
	return orchestrator.Outcome{Kind: orchestrator.Failed, Category: orDefault(ae.Category, TurnFailed), Message: ae.Msg}
}

// stillWanted mirrors the scheduler's rule: the issue is in an active
// state, approved, and labelled for this worker.
func stillWanted(cfg *config.Config, is issue.Issue) bool {
	state := issue.NormalizeState(is.State)
	active := false
	for _, s := range cfg.Tracker.ActiveStates {
		if issue.NormalizeState(s) == state {
			active = true
		}
	}
	for _, s := range cfg.Tracker.TerminalStates {
		if issue.NormalizeState(s) == state {
			active = false
		}
	}
	return active && is.Dispatchable && is.HasLabels(cfg.Tracker.RequiredLabels)
}
