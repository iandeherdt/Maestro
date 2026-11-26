// Package tracker defines the board boundary: a read kernel for scheduling
// and the small fixed set of writes the orchestrator performs (SPEC
// section 12). There are no agent-facing tools here.
package tracker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/ticketimage"
)

// Error categories (SPEC 12.4).
const (
	UnsupportedKind = "unsupported_tracker_kind"
	InvalidConfig   = "invalid_tracker_config"
	MissingSecret   = "missing_tracker_secret"
	Request         = "tracker_request"
	Status          = "tracker_status"
	Response        = "tracker_response"
	Pagination      = "tracker_pagination"
	RateLimited     = "tracker_rate_limited"
	WriteRejected   = "tracker_write_rejected"
)

// Error is an adapter failure with a stable category.
type Error struct {
	Category string
	Msg      string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Category, e.Msg) }

// CategoryOf returns the category of err, or "" if it is not a tracker error.
func CategoryOf(err error) string {
	var te *Error
	if errors.As(err, &te) {
		return te.Category
	}
	return ""
}

// Feedback is a comment by an allowlisted commenter.
type Feedback struct {
	Author    string
	Body      string
	CreatedAt time.Time
}

// Tracker is implemented by each board adapter.
type Tracker interface {
	// FetchIssuesByStates returns the issues in scope that are in one of the
	// given states, including those with Dispatchable false. An empty list
	// returns an empty result without a provider request.
	FetchIssuesByStates(ctx context.Context, states []string) ([]issue.Issue, error)
	// FetchIssuesByIDs returns current snapshots. IDs that are no longer
	// visible are omitted. A malformed requested record fails the call.
	FetchIssuesByIDs(ctx context.Context, ids []string) ([]issue.Issue, error)
	// SetState performs one of the transitions owned by the orchestrator.
	// It is idempotent.
	SetState(ctx context.Context, is issue.Issue, state string) error
	// UpsertStatusComment maintains one status comment per issue per worker.
	UpsertStatusComment(ctx context.Context, is issue.Issue, body string) error
	// FetchFeedback returns comments made after since by allowlisted
	// commenters only.
	FetchFeedback(ctx context.Context, is issue.Issue, since time.Time) ([]Feedback, error)
}

// Activity is implemented by adapters that can show on the board that an
// agent is working on an idea, which stays in its own column meanwhile
// (SPEC 22 and 27). Both calls are idempotent and best effort.
type Activity interface {
	// MarkWorking puts the label on the ticket.
	MarkWorking(ctx context.Context, is issue.Issue, label string) error
	// UnmarkWorking takes it off again. A label that is not there is not
	// an error.
	UnmarkWorking(ctx context.Context, is issue.Issue, label string) error
}

// Task is one piece of work from an approved plan (SPEC 22).
type Task struct {
	Title  string
	Body   string
	Worker string
}

// TaskRef identifies a task the service created on the board.
type TaskRef struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	// NativeID is the provider's own numeric ID, where it has one.
	NativeID int64 `json:"native_id,omitempty"`
}

// Planner is implemented by adapters that support the planning stage:
// talking to the owner on a ticket, and turning an approved plan into
// tickets of its own. Every write except CreateTask is idempotent.
type Planner interface {
	// Workers lists the machines a ticket can be routed to.
	Workers(ctx context.Context) ([]string, error)
	// PostComment adds a new comment to the ticket. The body is built by
	// the service; untrusted parts are fenced by the caller.
	PostComment(ctx context.Context, is issue.Issue, body string) error
	// CreateTask creates a ticket. It is not on the board yet.
	CreateTask(ctx context.Context, t Task) (TaskRef, error)
	// PlaceTask puts the ticket on the board, routed to a worker and in
	// the given state, and records it as a part of the parent.
	PlaceTask(ctx context.Context, parent issue.Issue, ref TaskRef, worker, state string) error
	// AddBlocker records that task cannot start before blocker is done.
	AddBlocker(ctx context.Context, task, blocker TaskRef) error
}

// SubIssues is implemented by adapters that can list the sub-issues of a
// ticket. An idea is finished only when these are done too, including
// sub-issues the owner added by hand.
type SubIssues interface {
	// SubIssueIDs returns the IDs of the ticket's sub-issues, open or not.
	SubIssueIDs(ctx context.Context, parent issue.Issue) ([]string, error)
}

// MaxUntrustedChars caps text from the agent or a tool inside a comment.
const MaxUntrustedChars = 2000

// Fence prepares text that came from the agent or from a tool for a comment:
// it is length-capped, stripped of control characters, has mentions, issue
// references and markup openers broken with a zero-width space, and is put
// in a fenced block that the text itself cannot close.
func Fence(text string) string { return FenceMax(text, MaxUntrustedChars) }

// Neutral returns text with control characters removed and mentions, issue
// references and markup openers broken, capped at limit characters. It is
// for agent text that becomes the title or the body of a ticket.
func Neutral(text string, limit int) string {
	const zwsp = "\u200b"
	var b strings.Builder
	n := 0
	for _, r := range text {
		if n >= limit {
			b.WriteString("…")
			break
		}
		n++
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			// dropped
		case r == '@' || r == '#' || r == '<':
			b.WriteRune(r)
			b.WriteString(zwsp)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// FenceMax is Fence with another length cap, for a plan the owner has to
// be able to read in full.
func FenceMax(text string, limit int) string {
	body := Neutral(text, limit)
	longest, run := 0, 0
	for _, r := range body {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + "text\n" + body + "\n" + fence
}

// ErrForeignIdentifier is a Lookup's answer for a name that cannot be an
// identifier of its board, such as a GitHub number asked of Linear.
var ErrForeignIdentifier = errors.New("not an identifier of this board")

// Lookup is implemented by adapters that can read one ticket by its
// identifier, whatever state it is in, including closed. It is used to
// tell whether what the service keeps for a ticket is still needed.
type Lookup interface {
	// LookupIdentifier reads the ticket. found is false when the board has
	// no such ticket in scope (any more).
	LookupIdentifier(ctx context.Context, identifier string) (is issue.Issue, found bool, err error)
}

// Images is implemented by adapters that can download the pictures people
// put on a ticket: in its description, and in the comments of allowlisted
// commenters (SPEC 13.5). The agent cannot download them itself, because
// that takes the service's credentials.
type Images interface {
	// TicketImages downloads them, oldest first. Pictures that could not
	// be downloaded are counted in the note, not an error.
	TicketImages(ctx context.Context, is issue.Issue) (ticketimage.Result, error)
}
