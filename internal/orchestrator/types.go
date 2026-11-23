package orchestrator

import (
	"context"
	"time"

	"github.com/iandeherdt/Maestro/internal/budget"
	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/plan"
)

// Phase of a run attempt (SPEC 7.2).
type Phase string

const (
	PreparingWorkspace    Phase = "PreparingWorkspace"
	BuildingPrompt        Phase = "BuildingPrompt"
	LaunchingAgentProcess Phase = "LaunchingAgentProcess"
	InitializingSession   Phase = "InitializingSession"
	StreamingTurn         Phase = "StreamingTurn"
	Exporting             Phase = "Exporting"
	Verifying             Phase = "Verifying"
	Publishing            Phase = "Publishing"
	Finishing             Phase = "Finishing"
)

// Retry kinds (SPEC 8.4).
const (
	KindContinuation = "continuation"
	KindFailure      = "failure"
	KindGate         = "gate"
	KindQuota        = "quota"
	KindTracker      = "tracker"
)

// Usage is cumulative token use of one attempt.
type Usage struct {
	Input, Output, CacheRead, CacheWrite int64
}

func (u Usage) Total() int64 { return u.Input + u.Output + u.CacheRead + u.CacheWrite }

// Event is an update from a running attempt.
type Event struct {
	Kind      string
	At        time.Time
	Message   string // summary, already length-capped by the runner
	SessionID string
	PID       int
	Phase     Phase  // empty means unchanged
	Usage     *Usage // cumulative for the attempt, nil means unchanged
	TurnStart bool
}

// OutcomeKind says how an attempt ended.
type OutcomeKind int

const (
	// Published: work was pushed and a pull request exists.
	Published OutcomeKind = iota
	// Normal: the attempt ended without publishing and without an error,
	// for example because the issue left the active states mid-run.
	Normal
	// Failed: an agent, workspace or sandbox error. Counts against the
	// failure budget. Timeouts and stalls are reported this way.
	Failed
	// Rejected: a gate refused the work. Counts against the failure budget
	// and carries feedback for the next attempt.
	Rejected
	// Quota: the agent reported a usage limit. Not a failure.
	Quota
	// NotLoggedIn: the agent has no valid login. Pauses dispatch.
	NotLoggedIn
	// Planned: a planning run ended with questions or a plan (SPEC 22).
	Planned
	// Designed: a design run published its design (SPEC 27).
	Designed
)

// Outcome is reported exactly once per attempt.
type Outcome struct {
	Kind     OutcomeKind
	Category string           // error category, SPEC 10.7 and 17.1
	Message  string           // untrusted when it comes from the agent or a tool
	Feedback []issue.Feedback // gate output for the next attempt
	Publish  *PublishRecord
	// QuotaResetAt is the reset time the CLI reported, if it did.
	QuotaResetAt time.Time
	// Plan is set for the outcome Planned. Its text is untrusted.
	Plan *plan.Plan
	// Design is set for the outcome Designed.
	Design *plan.Design
}

// PublishRecord describes pushed work that is waiting for its checks.
type PublishRecord struct {
	IssueID    string
	Identifier string
	Branch     string
	HeadSHA    string
	PRNumber   int
	// ChecksDeadline is when waiting for checks gives up.
	ChecksDeadline time.Time
	Flags          []string
}

// Attempt is what a worker is asked to run.
type Attempt struct {
	Issue issue.Issue
	// Number is 0 for the first run, >= 1 for retries and continuations.
	Number    int
	RetryKind string
	Feedback  []issue.Feedback
	// Planning is true for an idea: the run plans and changes nothing.
	Planning bool
	// Design is true for an idea in the design state: the run makes a
	// design for its plan (SPEC 27).
	Design bool
}

// Sink receives updates from one attempt. Both methods may be called from
// any goroutine and never block.
type Sink interface {
	Event(Event)
	Exit(Outcome)
}

// Handle controls a started attempt.
type Handle interface {
	// Terminate kills the whole sandbox unit. It must not wait for the
	// attempt to report its exit.
	Terminate()
}

// Worker runs attempts (SPEC 19.5). The orchestrator never touches the
// workspace or the agent itself.
type Worker interface {
	Start(a Attempt, sink Sink) (Handle, error)
}

// Workspaces removes per-issue workspaces.
type Workspaces interface {
	Remove(ctx context.Context, identifier string) error
}

// Prober answers whether an in-progress issue was already published before
// a restart (SPEC 17.3). It returns nil when it was not.
type Prober interface {
	Probe(ctx context.Context, is issue.Issue) (*PublishRecord, error)
}

// Plans is the planning stage as the orchestrator sees it (SPEC 22).
type Plans interface {
	// Propose shows the result of a planning run to the owner.
	Propose(ctx context.Context, is issue.Issue, p *plan.Plan) error
	// Materialize creates the tasks of the plan the owner approved. It
	// continues where an earlier call stopped. The summary is non-empty
	// once, when the last task is in place.
	Materialize(ctx context.Context, is issue.Issue) (summary string, err error)
}

// PlanFinisher is implemented by a Plans that can tell when every task of
// an approved plan is done.
type PlanFinisher interface {
	Finished(ctx context.Context, is issue.Issue) (done bool, tasks int, err error)
}

// Designs is the design stage as the orchestrator sees it (SPEC 27).
type Designs interface {
	// HasPlan reports whether the idea has a plan a design can be made for.
	HasPlan(issueID string) bool
	// HasDesign reports whether a design was published for the latest plan.
	HasDesign(issueID string) bool
	// Designed records a published design and tells the owner.
	Designed(ctx context.Context, is issue.Issue, d plan.Design) error
}

// Clock is the time source.
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// RetryEntry is a scheduled retry (SPEC 4.1.7).
type RetryEntry struct {
	IssueID    string
	Identifier string
	Attempt    int
	Kind       string
	Feedback   []issue.Feedback
	DueAt      time.Time
	Error      string

	// resumeKind is the kind to dispatch with once a tracker write succeeds.
	resumeKind string
	// blockReason, when set, means the pending write is the move to the
	// blocked state rather than a dispatch.
	blockReason string
	// writeFailures counts consecutive failed tracker writes.
	writeFailures int
}

// Totals aggregates token use and runtime over the process lifetime.
type Totals struct {
	Usage          Usage
	SecondsRunning float64
}

// RunningView is a read-only copy of a running entry.
type RunningView struct {
	IssueID, Identifier, State string
	SessionID                  string
	PID                        int
	Phase                      Phase
	Attempt                    int
	TurnCount                  int
	StartedAt                  time.Time
	LastEvent, LastMessage     string
	LastEventAt                time.Time
	Usage                      Usage
	FailureAttempts            int64
	// Feed is what the agent said and did lately, oldest first. The text
	// was written by the agent: it is information, never instructions.
	Feed []FeedItem
}

// FeedItem is one line of a running agent's feed.
type FeedItem struct {
	At            time.Time
	Kind, Message string
}

// FeedLen is how many feed lines a running entry keeps.
const FeedLen = 40

// feedKinds are the events that go into the feed: what the agent says,
// what it runs, and what holds it up.
var feedKinds = map[string]bool{"notification": true, "tool_call": true, "api_retry": true,
	"rate_limit": true, "permission_denied": true}

// Snapshot is a read-only copy of the scheduling state.
type Snapshot struct {
	GeneratedAt       time.Time
	Worker            string
	Today             budget.Day
	Running           []RunningView
	Retrying          []RetryEntry
	AwaitingChecks    []PublishRecord
	Totals            Totals
	Paused            bool
	PauseReason       string
	QuotaBlockedUntil time.Time
	ConfigError       string
	// DailyLimit says which daily limit stops new work, or is empty.
	DailyLimit string
}

// CheckState is what the checks gate found for a published pull request.
type CheckState int

const (
	// ChecksPending: at least one required check has not finished.
	ChecksPending CheckState = iota
	// ChecksPassed: every required check succeeded on the pushed commit.
	ChecksPassed
	// ChecksFailed: a required check failed.
	ChecksFailed
	// ChecksForeignPush: the pull request's head is not the pushed commit.
	ChecksForeignPush
	// ChecksClosed: the pull request was closed or merged by someone.
	ChecksClosed
)

// CheckStatus is the result of one look at a pull request's checks.
type CheckStatus struct {
	State CheckState
	// Detail is a short summary. Untrusted: check names come from the
	// repository's workflows.
	Detail string
	// Feedback describes failed checks for the agent's next attempt.
	Feedback []issue.Feedback
}

// Checker reads the checks of a published pull request (SPEC 15.6).
type Checker interface {
	Check(ctx context.Context, rec PublishRecord) (CheckStatus, error)
	// MarkReady takes the pull request out of draft. It must succeed when
	// the pull request is already ready.
	MarkReady(ctx context.Context, rec PublishRecord) error
}

// ReviewState is where the pull request of a handed-off ticket stands.
type ReviewState int

const (
	// ReviewNone: the service has no pull request for the ticket.
	ReviewNone ReviewState = iota
	// ReviewOpen: waiting for a person.
	ReviewOpen
	// ReviewMerged: a person merged it.
	ReviewMerged
	// ReviewClosed: closed without a merge.
	ReviewClosed
	// ReviewChangesRequested: the latest review of the current commit by
	// an allowlisted reviewer asks for changes.
	ReviewChangesRequested
	// ReviewFeedback: an allowlisted person wrote comments on the pull
	// request that no run has taken up yet, and has been quiet since.
	ReviewFeedback
)

// ReviewStatus is the answer of Reviews.Review.
type ReviewStatus struct {
	State    ReviewState
	PRNumber int
	// For ReviewFeedback: how many comments are new, and the time of the
	// newest. Acknowledge records that time on the pull request.
	FeedbackCount   int
	FeedbackThrough time.Time
}

// Reviews follows pull requests after hand-off (SPEC 15.8).
type Reviews interface {
	Review(ctx context.Context, is issue.Issue) (ReviewStatus, error)
	// Acknowledge records on the pull request that the comments up to
	// st.FeedbackThrough were taken up, so that they start one run only.
	Acknowledge(ctx context.Context, is issue.Issue, st ReviewStatus) error
	// DeleteBranch removes the ticket's branch on the remote after a
	// merge. A branch that is already gone is not an error.
	DeleteBranch(ctx context.Context, is issue.Issue) error
}
