package publish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/iandeherdt/Symphony-beads/internal/issue"
	"github.com/iandeherdt/Symphony-beads/internal/orchestrator"
)

// maxCheckPages bounds how many pages of check runs are read for one
// commit. A commit with more is treated as still pending.
const maxCheckPages = 5

type pullState struct {
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	Draft  bool   `json:"draft"`
	NodeID string `json:"node_id"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

type result int

const (
	resPending result = iota
	resPassed
	resFailed
)

func runResult(status, conclusion string) result {
	if status != "completed" {
		return resPending
	}
	switch conclusion {
	case "success", "neutral", "skipped":
		return resPassed
	}
	return resFailed
}

func statusResult(state string) result {
	switch state {
	case "success":
		return resPassed
	case "failure", "error":
		return resFailed
	}
	return resPending
}

func (p *Publisher) repoAPI() (*api, string, error) {
	cfg := p.Config()
	m := repoRE.FindStringSubmatch(cfg.Workspace.Repo)
	if m == nil {
		return nil, "", errors.New("workspace.repo must be owner/name")
	}
	a, _, err := p.endpoints(m[1], m[2])
	return a, "/repos/" + m[1] + "/" + m[2], err
}

// Check reads the pull request and the checks of the pushed commit (SPEC
// 15.6). With publish.required_checks set, exactly those must pass. With
// an empty list, every check reported for the commit must pass and there
// must be at least one, so that "no checks yet" is never read as green.
func (p *Publisher) Check(ctx context.Context, rec orchestrator.PublishRecord) (orchestrator.CheckStatus, error) {
	var none orchestrator.CheckStatus
	a, prefix, err := p.repoAPI()
	if err != nil {
		return none, err
	}
	if rec.PRNumber <= 0 || !shaRE.MatchString(rec.HeadSHA) {
		return none, errors.New("the publish record is incomplete")
	}
	var pr pullState
	if err := a.do(ctx, http.MethodGet, prefix+"/pulls/"+strconv.Itoa(rec.PRNumber), nil, nil, &pr); err != nil {
		return none, err
	}
	if pr.State != "open" || pr.Merged {
		return orchestrator.CheckStatus{State: orchestrator.ChecksClosed}, nil
	}
	if pr.Head.SHA != rec.HeadSHA {
		return orchestrator.CheckStatus{State: orchestrator.ChecksForeignPush}, nil
	}

	results := map[string]result{}
	details := map[string]string{}
	complete := false
	for page := 1; page <= maxCheckPages; page++ {
		var runs struct {
			Total int `json:"total_count"`
			Runs  []struct {
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
				Output     struct {
					Title   string `json:"title"`
					Summary string `json:"summary"`
				} `json:"output"`
			} `json:"check_runs"`
		}
		q := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}, "filter": {"latest"}}
		if err := a.do(ctx, http.MethodGet, prefix+"/commits/"+rec.HeadSHA+"/check-runs", q, nil, &runs); err != nil {
			return none, err
		}
		for _, r := range runs.Runs {
			res := runResult(r.Status, r.Conclusion)
			// Two runs with one name: the worse result counts.
			if prev, ok := results[r.Name]; !ok || worse(res, prev) {
				results[r.Name] = res
				details[r.Name] = strings.TrimSpace(r.Output.Title + "\n" + r.Output.Summary)
			}
		}
		if len(runs.Runs) < 100 {
			complete = true
			break
		}
	}
	var combined struct {
		Statuses []struct {
			Context     string `json:"context"`
			State       string `json:"state"`
			Description string `json:"description"`
		} `json:"statuses"`
	}
	if err := a.do(ctx, http.MethodGet, prefix+"/commits/"+rec.HeadSHA+"/status", url.Values{"per_page": {"100"}}, nil, &combined); err != nil {
		return none, err
	}
	for _, s := range combined.Statuses {
		res := statusResult(s.State)
		if prev, ok := results[s.Context]; !ok || worse(res, prev) {
			results[s.Context] = res
			details[s.Context] = s.Description
		}
	}

	// A copy: the list is sorted below and the configuration is shared.
	names := append([]string(nil), p.Config().Publish.RequiredChecks...)
	if len(names) == 0 {
		for n := range results {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var failedNames []string
	waiting := 0
	for _, n := range names {
		switch res, ok := results[n]; {
		case !ok || res == resPending:
			waiting++
		case res == resFailed:
			failedNames = append(failedNames, n)
		}
	}
	switch {
	case len(failedNames) > 0:
		st := orchestrator.CheckStatus{State: orchestrator.ChecksFailed,
			Detail: fmt.Sprintf("%d required check(s) failed", len(failedNames))}
		for _, n := range failedNames {
			body := "The check " + clean(n, 100) + " failed."
			if d := clean(strings.ReplaceAll(details[n], "\n", " "), 1500); d != "" {
				body += " Its report: " + d
			}
			st.Feedback = append(st.Feedback, issue.Feedback{Source: "ci", Body: body})
		}
		return st, nil
	case waiting > 0 || len(names) == 0 || !complete:
		return orchestrator.CheckStatus{State: orchestrator.ChecksPending,
			Detail: fmt.Sprintf("%d of %d check(s) not finished", waiting, len(names))}, nil
	}
	return orchestrator.CheckStatus{State: orchestrator.ChecksPassed}, nil
}

func worse(a, b result) bool {
	rank := map[result]int{resPassed: 0, resPending: 1, resFailed: 2}
	return rank[a] > rank[b]
}

// MarkReady takes the pull request out of draft. GitHub offers this only
// through GraphQL. A pull request that is already ready is left alone.
func (p *Publisher) MarkReady(ctx context.Context, rec orchestrator.PublishRecord) error {
	a, prefix, err := p.repoAPI()
	if err != nil {
		return err
	}
	var pr pullState
	if err := a.do(ctx, http.MethodGet, prefix+"/pulls/"+strconv.Itoa(rec.PRNumber), nil, nil, &pr); err != nil {
		return err
	}
	if pr.Head.SHA != rec.HeadSHA {
		return errors.New("the pull request no longer points at the pushed commit")
	}
	if !pr.Draft {
		return nil
	}
	if !nodeRE.MatchString(pr.NodeID) {
		return errors.New("the pull request has no usable node id")
	}
	var out struct {
		Data struct {
			Mark struct {
				PullRequest struct {
					IsDraft bool `json:"isDraft"`
				} `json:"pullRequest"`
			} `json:"markPullRequestReadyForReview"`
		} `json:"data"`
		Errors []struct {
			Type string `json:"type"`
		} `json:"errors"`
	}
	body := map[string]any{
		"query":     `mutation($id: ID!) { markPullRequestReadyForReview(input: {pullRequestId: $id}) { pullRequest { isDraft } } }`,
		"variables": map[string]string{"id": pr.NodeID},
	}
	if err := a.do(ctx, http.MethodPost, "/graphql", nil, body, &out); err != nil {
		return err
	}
	if len(out.Errors) > 0 || out.Data.Mark.PullRequest.IsDraft {
		return errors.New("GitHub did not mark the pull request as ready")
	}
	return nil
}

var _ orchestrator.Checker = (*Publisher)(nil)
