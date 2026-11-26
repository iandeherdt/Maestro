package github

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/tracker"
)

// LookupIdentifier reads one issue by its identifier, GH-<number>, in any
// state. A closed issue has the first terminal state, unless its card
// still waits in the hand-off column.
func (a *Adapter) LookupIdentifier(ctx context.Context, identifier string) (issue.Issue, bool, error) {
	num, ok := strings.CutPrefix(identifier, "GH-")
	n, err := strconv.Atoi(num)
	if !ok || err != nil || n <= 0 || strconv.Itoa(n) != num {
		return issue.Issue{}, false, tracker.ErrForeignIdentifier
	}
	gi, card, err := a.read(ctx, n)
	if errors.Is(err, errNotFound) {
		return issue.Issue{}, false, nil
	}
	if err != nil {
		return issue.Issue{}, false, err
	}
	// A pull request has a number of the same kind, but is no ticket.
	if len(gi.PullRequest) > 0 {
		return issue.Issue{}, false, nil
	}
	is, err := a.normalize(ctx, gi, card)
	if err != nil {
		return issue.Issue{}, false, err
	}
	return is, true, nil
}

var _ tracker.Lookup = (*Adapter)(nil)
