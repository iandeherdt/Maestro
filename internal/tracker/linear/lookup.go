package linear

import (
	"context"
	"strings"

	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/tracker"
)

// LookupIdentifier reads one issue of the team by its identifier, such as
// ENG-12, in any state. Only the fields that say where it stands are
// filled: no further request is spent on it.
func (a *Adapter) LookupIdentifier(ctx context.Context, identifier string) (issue.Issue, bool, error) {
	team, _, _ := strings.Cut(identifier, "-")
	if !identRE.MatchString(identifier) || !strings.EqualFold(team, a.team) {
		return issue.Issue{}, false, tracker.ErrForeignIdentifier
	}
	li, err := a.read(ctx, identifier)
	if err == errNotFound {
		return issue.Issue{}, false, nil
	}
	if err != nil {
		return issue.Issue{}, false, err
	}
	// Moved to another team: no longer this board's, and not gone either.
	if li.Team == nil || !strings.EqualFold(li.Team.Key, a.team) {
		return issue.Issue{}, false, tracker.ErrForeignIdentifier
	}
	if li.ID == "" || li.State == nil || strings.TrimSpace(li.State.Name) == "" {
		return issue.Issue{}, false, &tracker.Error{Category: tracker.Response, Msg: "issue record lacks a required field"}
	}
	return issue.Issue{ID: li.ID, Identifier: strings.ToUpper(li.Identifier), State: strings.TrimSpace(li.State.Name)}, true, nil
}

var _ tracker.Lookup = (*Adapter)(nil)
