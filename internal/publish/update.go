package publish

import (
	"context"
	"fmt"

	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/mirror"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
)

// UpdateBranch merges the newest base branch into the branch of the
// ticket's open pull request and pushes the result, when the merge has no
// conflicts. The host then runs the checks on the two together, which it
// does not do by itself when the base moves. A merge with conflicts is
// reported and nothing is pushed: resolving it is work for an agent.
//
// The merge is made of commits that were already accepted: what the
// service pushed for the ticket, and the owner's base branch. A branch
// that anyone else pushed to is left alone.
func (p *Publisher) UpdateBranch(ctx context.Context, is issue.Issue) (orchestrator.BaseUpdate, error) {
	var none orchestrator.BaseUpdate
	cfg := p.Config()
	if !cfg.Publish.UpdateBranches || p.Fetch == nil || p.Mirror == nil ||
		cfg.Workspace.BaseBranch == "" || p.BotName == "" || p.BotEmail == "" {
		return none, nil
	}
	key, branch, err := p.branchOf(is)
	if err != nil {
		return none, err
	}
	h, err := p.host()
	if err != nil {
		return none, err
	}
	pr, err := p.pull(ctx, h, is)
	if err != nil || pr == nil || pr.State != changeOpen {
		return none, err
	}
	none.PRNumber = pr.Number
	if err := p.Fetch(ctx); err != nil {
		return none, fmt.Errorf("fetch: %w", err)
	}
	st, head, err := p.Mirror.MergeBase(ctx, key, branch, cfg.Workspace.BaseBranch, p.BotName, p.BotEmail)
	if err != nil {
		return none, fmt.Errorf("merge: %w", err)
	}
	switch st {
	case mirror.BaseConflict:
		return orchestrator.BaseUpdate{State: orchestrator.BaseConflicted, PRNumber: pr.Number}, nil
	case mirror.BaseMerged:
		tok, err := p.Token(ctx)
		if err != nil {
			return none, fmt.Errorf("repository token: %w", err)
		}
		if err := p.Mirror.Push(ctx, h.gitURL(), key, branch, tok); err != nil {
			return none, fmt.Errorf("push: %w", err)
		}
		return orchestrator.BaseUpdate{State: orchestrator.BaseUpdated, HeadSHA: head, PRNumber: pr.Number}, nil
	}
	return none, nil
}

var _ orchestrator.Updater = (*Publisher)(nil)
