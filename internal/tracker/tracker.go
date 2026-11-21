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

	"github.com/iandeherdt/Symphony-beads/internal/issue"
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

// MaxUntrustedChars caps text from the agent or a tool inside a comment.
const MaxUntrustedChars = 2000

// Fence prepares text that came from the agent or from a tool for a comment:
// it is length-capped, stripped of control characters, has mentions, issue
// references and markup openers broken with a zero-width space, and is put
// in a fenced block that the text itself cannot close.
func Fence(text string) string {
	const zwsp = "​"
	var b strings.Builder
	n := 0
	for _, r := range text {
		if n >= MaxUntrustedChars {
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
	body := b.String()
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
