package publish

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/mirror"
	"github.com/iandeherdt/Maestro/internal/orchestrator"
)

// maxReviewPages bounds how many pages of reviews are read for one pull
// request. With more, the reviews are not acted on.
const maxReviewPages = 5

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

type listedPull struct {
	Number   int    `json:"number"`
	State    string `json:"state"`
	MergedAt string `json:"merged_at"`
	User     struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

// Review reports where the newest pull request on the ticket's branch
// stands (SPEC 15.8). Only a pull request from a branch in the repository
// itself counts, and when BotLogin is set only one the service opened: a
// pull request from a fork that reuses the branch name is not the
// service's work.
func (p *Publisher) Review(ctx context.Context, is issue.Issue) (orchestrator.ReviewStatus, error) {
	var none orchestrator.ReviewStatus
	a, prefix, err := p.repoAPI()
	if err != nil {
		return none, err
	}
	_, branch, err := p.branchOf(is)
	if err != nil {
		return none, err
	}
	cfg := p.Config()
	owner := strings.SplitN(cfg.Workspace.Repo, "/", 2)[0]
	var pulls []listedPull
	q := url.Values{"head": {owner + ":" + branch}, "state": {"all"}, "sort": {"created"}, "direction": {"desc"}, "per_page": {"20"}}
	if err := a.do(ctx, http.MethodGet, prefix+"/pulls", q, nil, &pulls); err != nil {
		return none, err
	}
	var pr *listedPull
	for i := range pulls {
		c := &pulls[i]
		if c.Head.Ref != branch || !strings.EqualFold(c.Head.Repo.FullName, cfg.Workspace.Repo) || c.Number <= 0 {
			continue
		}
		if p.BotLogin != "" && !strings.EqualFold(c.User.Login, p.BotLogin) {
			continue
		}
		pr = c
		break
	}
	switch {
	case pr == nil:
		return none, nil
	case pr.MergedAt != "":
		return orchestrator.ReviewStatus{State: orchestrator.ReviewMerged, PRNumber: pr.Number}, nil
	case pr.State != "open":
		return orchestrator.ReviewStatus{State: orchestrator.ReviewClosed, PRNumber: pr.Number}, nil
	}
	st := orchestrator.ReviewStatus{State: orchestrator.ReviewOpen, PRNumber: pr.Number}
	if !cfg.Publish.ReworkOnChangesRequested {
		return st, nil
	}

	// The latest verdict on the commit that is there now. A review of an
	// earlier commit was answered by the rework that followed it, and
	// acting on it again would send the ticket round in circles.
	verdict := ""
	for page := 1; ; page++ {
		if page > maxReviewPages {
			return st, nil
		}
		var reviews []struct {
			State    string `json:"state"`
			CommitID string `json:"commit_id"`
			User     struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		rq := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if err := a.do(ctx, http.MethodGet, prefix+"/pulls/"+strconv.Itoa(pr.Number)+"/reviews", rq, nil, &reviews); err != nil {
			return none, err
		}
		for _, r := range reviews {
			if r.CommitID != pr.Head.SHA || !allowed(cfg.Tracker.AllowedCommenters, r.User.Login) {
				continue
			}
			if r.State == "APPROVED" || r.State == "CHANGES_REQUESTED" {
				verdict = r.State
			}
		}
		if len(reviews) < 100 {
			break
		}
	}
	if verdict == "CHANGES_REQUESTED" {
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
	a, prefix, err := p.repoAPI()
	if err != nil {
		return err
	}
	key, branch, err := p.branchOf(is)
	if err != nil {
		return err
	}
	err = a.do(ctx, http.MethodDelete, prefix+"/git/refs/heads/"+branch, nil, nil, nil)
	var ae *apiError
	if errors.As(err, &ae) && (ae.Status == http.StatusNotFound || ae.Status == http.StatusUnprocessableEntity) {
		err = nil
	}
	if err != nil {
		return err
	}
	if p.Mirror != nil {
		return p.Mirror.ForgetPush(ctx, key)
	}
	return nil
}

var _ orchestrator.Reviews = (*Publisher)(nil)
