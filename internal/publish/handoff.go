package publish

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/mirror"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
)

// branchOf returns the branch the service publishes a ticket on. It is
// derived from the identifier alone, never read from the ticket's text.
func (p *Publisher) branchOf(is issue.Issue) (key, branch string, err error) {
	key, err = issue.WorkspaceKey(is.Identifier)
	if err != nil {
		return "", "", errors.New("the identifier cannot name a branch")
	}
	prefix := p.Config().Publish.BranchPrefix
	branch = prefix + key
	if prefix == "" || !mirror.ValidBranch(branch) || branch == p.Config().Workspace.BaseBranch {
		return "", "", errors.New("the ticket's branch is not under publish.branch_prefix")
	}
	return key, branch, nil
}

// pull finds the newest pull request on the ticket's branch. Only a pull
// request from a branch in the repository itself counts, and when BotLogin
// is set only one the service opened: a pull request from a fork that
// reuses the branch name is not the service's work.
func (p *Publisher) pull(ctx context.Context, h host, is issue.Issue) (*change, error) {
	_, branch, err := p.branchOf(is)
	if err != nil {
		return nil, err
	}
	list, err := h.changes(ctx, branch)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if p.BotLogin != "" && !strings.EqualFold(list[i].Author, p.BotLogin) {
			continue
		}
		return &list[i], nil
	}
	return nil, nil
}

// Review reports where the newest pull request on the ticket's branch
// stands (SPEC 15.8).
func (p *Publisher) Review(ctx context.Context, is issue.Issue) (orchestrator.ReviewStatus, error) {
	var none orchestrator.ReviewStatus
	h, err := p.host()
	if err != nil {
		return none, err
	}
	pr, err := p.pull(ctx, h, is)
	if err != nil {
		return none, err
	}
	cfg := p.Config()
	switch {
	case pr == nil:
		return none, nil
	case pr.State == changeMerged:
		return orchestrator.ReviewStatus{State: orchestrator.ReviewMerged, PRNumber: pr.Number}, nil
	case pr.State != changeOpen:
		return orchestrator.ReviewStatus{State: orchestrator.ReviewClosed, PRNumber: pr.Number}, nil
	}
	st := orchestrator.ReviewStatus{State: orchestrator.ReviewOpen, PRNumber: pr.Number}
	comments := p.commentsOn(cfg)
	if !cfg.Publish.ReworkOnChangesRequested && !comments {
		return st, nil
	}
	reviews, complete, err := h.reviews(ctx, pr.Number)
	if err != nil {
		return none, err
	}
	if !complete {
		return st, nil
	}

	// What the owner wrote on the pull request comes first: it sends the
	// ticket back with the words, where a bare verdict sends it back
	// without any.
	if comments {
		t, complete, err := p.thread(ctx, h, pr.Number, reviews)
		if err != nil {
			return none, err
		}
		if fresh := t.after(t.mark(0)); complete && len(fresh) > 0 {
			newest := fresh[len(fresh)-1].at
			// Someone who is still writing is not interrupted.
			if p.now().Sub(newest) < time.Duration(cfg.Publish.ReviewQuietMS)*time.Millisecond {
				return st, nil
			}
			st.State, st.FeedbackCount, st.FeedbackThrough = orchestrator.ReviewFeedback, len(fresh), newest
			return st, nil
		}
	}
	if !cfg.Publish.ReworkOnChangesRequested {
		return st, nil
	}

	// The latest verdict on the commit that is there now. A review of an
	// earlier commit was answered by the rework that followed it, and
	// acting on it again would send the ticket round in circles.
	verdict := verdictNone
	for _, r := range reviews {
		if r.CommitSHA != pr.HeadSHA || !allowed(cfg.Tracker.AllowedCommenters, r.Login) {
			continue
		}
		if r.Verdict != verdictNone {
			verdict = r.Verdict
		}
	}
	if verdict == verdictChanges {
		st.State = orchestrator.ReviewChangesRequested
	}
	return st, nil
}

func allowed(list []string, login string) bool {
	for _, l := range list {
		if login != "" && strings.EqualFold(l, login) {
			return true
		}
	}
	return false
}

// DeleteBranch removes the ticket's branch on the remote and forgets the
// push that created it. A branch that is already gone is not an error.
func (p *Publisher) DeleteBranch(ctx context.Context, is issue.Issue) error {
	h, err := p.host()
	if err != nil {
		return err
	}
	key, branch, err := p.branchOf(is)
	if err != nil {
		return err
	}
	if err := h.deleteBranch(ctx, branch); err != nil {
		return err
	}
	if p.Mirror != nil {
		return p.Mirror.ForgetPush(ctx, key)
	}
	return nil
}

var _ orchestrator.Reviews = (*Publisher)(nil)
