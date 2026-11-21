package orchestrator

import (
	"context"
	"time"

	"github.com/iandeherdt/Symphony-beads/internal/budget"
	"github.com/iandeherdt/Symphony-beads/internal/issue"
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
}

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
}
