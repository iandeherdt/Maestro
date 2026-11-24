package publish

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/mirror"
	"github.com/iandeherdt/Maestro/internal/worker"
)

// GitHub as the code host: REST for nearly everything, GraphQL for the one
// thing REST cannot do (marking a pull request ready).

const (
	apiVersion  = "2022-11-28"
	maxResponse = 4 << 20
	// maxCheckPages bounds how many pages of check runs are read for one
	// commit. A commit with more is treated as still pending.
	maxCheckPages = 5
	// maxReviewPages bounds how many pages of reviews or comments are
	// read for one pull request. With more, they are not acted on.
	maxReviewPages = 5
)

// api is a minimal GitHub REST client for pull requests. The token goes
// only to the configured origin and redirects are not followed.
type api struct {
	base  *url.URL
	hc    *http.Client
	token func(context.Context) (string, error)
}

type apiError struct {
	Method, Path string
	Status       int
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s %s: status %d", e.Method, e.Path, e.Status)
}

func (a *api) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	u := *a.base
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawQuery = query.Encode()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return err
	}
	tok, err := a.token(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The response text is not passed on: it can quote request content.
		return &apiError{Method: method, Path: path, Status: resp.StatusCode}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s %s: the response could not be read", method, path)
		}
	}
	return nil
}

// newAPI checks an API URL and builds a client for it. Only https is
// accepted, and plain http on the loopback for tests.
func newAPI(apiURL string, hc *http.Client, token func(context.Context) (string, error)) (*api, error) {
	base, err := url.Parse(apiURL)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" {
		return nil, errors.New("the API URL is not valid")
	}
	if base.Scheme != "https" && !(base.Scheme == "http" && (base.Hostname() == "127.0.0.1" || base.Hostname() == "localhost")) {
		return nil, errors.New("the API URL must use https")
	}
	c := &http.Client{Timeout: 30 * time.Second}
	if hc != nil {
		cp := *hc
		c = &cp
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &api{base: base, hc: c, token: token}, nil
}

// listAll reads every page of a list. It reports false when there are more
// than maxReviewPages pages: what was read is then not the whole picture.
func listAll[T any](ctx context.Context, a *api, path string) ([]T, bool, error) {
	var all []T
	for page := 1; ; page++ {
		if page > maxReviewPages {
			return all, false, nil
		}
		var items []T
		q := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if err := a.do(ctx, http.MethodGet, path, q, nil, &items); err != nil {
			return nil, false, err
		}
		all = append(all, items...)
		if len(items) < 100 {
			return all, true, nil
		}
	}
}

// gitHub is one repository on GitHub.
type gitHub struct {
	a           *api
	owner, repo string
	// prefix is the repository's REST path.
	prefix string
	// web is where GitHub's pages are, such as https://github.com.
	web string
	// shotBranch is where the pictures are kept.
	shotBranch string
}

func (p *Publisher) gitHub(owner, repo string) (*gitHub, error) {
	apiURL := p.APIURL
	if apiURL == "" {
		apiURL = "https://api.github.com"
	}
	a, err := newAPI(apiURL, p.HTTP, p.Token)
	if err != nil {
		return nil, err
	}
	web := strings.TrimSuffix(p.GitURL, "/")
	if web == "" {
		web = "https://github.com"
	}
	return &gitHub{a: a, owner: owner, repo: repo, prefix: "/repos/" + owner + "/" + repo, web: web,
		shotBranch: shotBranch(p.Config().Publish.BranchPrefix)}, nil
}

func (g *gitHub) gitURL() string { return g.web + "/" + g.owner + "/" + g.repo + ".git" }

type pull struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	// MergedAt is set in lists, where merged is not.
	MergedAt string `json:"merged_at"`
	Draft    bool   `json:"draft"`
	NodeID   string `json:"node_id"`
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
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (pr pull) change() change {
	c := change{Number: pr.Number, State: changeClosed, Draft: pr.Draft, HeadSHA: pr.Head.SHA, Author: pr.User.Login, id: pr.NodeID}
	switch {
	case pr.Merged || pr.MergedAt != "":
		c.State = changeMerged
	case pr.State == "open":
		c.State = changeOpen
	}
	return c
}

func (g *gitHub) upsert(ctx context.Context, branch, base, title, body string, draft bool) (int, error) {
	if base == "" {
		var r struct {
			DefaultBranch string `json:"default_branch"`
		}
		if err := g.a.do(ctx, http.MethodGet, g.prefix, nil, nil, &r); err != nil {
			return 0, err
		}
		if r.DefaultBranch == "" {
			return 0, errors.New("the repository reports no default branch")
		}
		base = r.DefaultBranch
	}
	if base == branch {
		return 0, errors.New("the head branch is the base branch")
	}
	var open []pull
	q := url.Values{"head": {g.owner + ":" + branch}, "state": {"open"}, "per_page": {"10"}}
	if err := g.a.do(ctx, http.MethodGet, g.prefix+"/pulls", q, nil, &open); err != nil {
		return 0, err
	}
	for _, p := range open {
		// The filter is by user and branch; confirm rather than assume.
		if p.Head.Ref != branch || p.Number <= 0 {
			continue
		}
		if p.Base.Ref != base {
			return 0, fmt.Errorf("an open pull request for this branch targets %q instead of the base branch", clean(p.Base.Ref, 100))
		}
		err := g.a.do(ctx, http.MethodPatch, g.prefix+"/pulls/"+strconv.Itoa(p.Number), nil,
			map[string]any{"title": title, "body": body}, nil)
		return p.Number, err
	}
	var created pull
	err := g.a.do(ctx, http.MethodPost, g.prefix+"/pulls", nil,
		map[string]any{"title": title, "head": branch, "base": base, "body": body, "draft": draft}, &created)
	if err != nil {
		return 0, err
	}
	if created.Number <= 0 {
		return 0, errors.New("the created pull request has no number")
	}
	return created.Number, nil
}

func (g *gitHub) change(ctx context.Context, number int) (change, error) {
	var pr pull
	if err := g.a.do(ctx, http.MethodGet, g.prefix+"/pulls/"+strconv.Itoa(number), nil, nil, &pr); err != nil {
		return change{}, err
	}
	return pr.change(), nil
}

func (g *gitHub) changes(ctx context.Context, branch string) ([]change, error) {
	var pulls []pull
	q := url.Values{"head": {g.owner + ":" + branch}, "state": {"all"}, "sort": {"created"}, "direction": {"desc"}, "per_page": {"20"}}
	if err := g.a.do(ctx, http.MethodGet, g.prefix+"/pulls", q, nil, &pulls); err != nil {
		return nil, err
	}
	var out []change
	for _, c := range pulls {
		if c.Head.Ref != branch || !strings.EqualFold(c.Head.Repo.FullName, g.owner+"/"+g.repo) || c.Number <= 0 {
			continue
		}
		out = append(out, c.change())
	}
	return out, nil
}

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

// checks reads the check runs (latest run per check) and the commit
// statuses.
func (g *gitHub) checks(ctx context.Context, sha string) (map[string]check, bool, error) {
	results := map[string]check{}
	add := func(name string, res result, detail string) {
		// Two results with one name: the worse one counts.
		if prev, ok := results[name]; !ok || worse(res, prev.res) {
			results[name] = check{res, detail}
		}
	}
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
		if err := g.a.do(ctx, http.MethodGet, g.prefix+"/commits/"+sha+"/check-runs", q, nil, &runs); err != nil {
			return nil, false, err
		}
		for _, r := range runs.Runs {
			add(r.Name, runResult(r.Status, r.Conclusion), strings.TrimSpace(r.Output.Title+"\n"+r.Output.Summary))
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
	if err := g.a.do(ctx, http.MethodGet, g.prefix+"/commits/"+sha+"/status", url.Values{"per_page": {"100"}}, nil, &combined); err != nil {
		return nil, false, err
	}
	for _, s := range combined.Statuses {
		add(s.Context, statusResult(s.State), s.Description)
	}
	return results, complete, nil
}

// markReady uses GraphQL: GitHub offers this nowhere else. GitHub refuses
// it when the head moved in the meantime.
func (g *gitHub) markReady(ctx context.Context, c change) error {
	if !nodeRE.MatchString(c.id) {
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
		"variables": map[string]string{"id": c.id},
	}
	if err := g.a.do(ctx, http.MethodPost, "/graphql", nil, body, &out); err != nil {
		return err
	}
	if len(out.Errors) > 0 || out.Data.Mark.PullRequest.IsDraft {
		return errors.New("GitHub did not mark the pull request as ready")
	}
	return nil
}

func (g *gitHub) reviews(ctx context.Context, number int) ([]prReview, bool, error) {
	listed, complete, err := listAll[struct {
		ID          int64  `json:"id"`
		State       string `json:"state"`
		CommitID    string `json:"commit_id"`
		Body        string `json:"body"`
		SubmittedAt string `json:"submitted_at"`
		User        struct {
			Login string `json:"login"`
		} `json:"user"`
	}](ctx, g.a, g.prefix+"/pulls/"+strconv.Itoa(number)+"/reviews")
	if err != nil || !complete {
		return nil, complete, err
	}
	out := make([]prReview, len(listed))
	for i, r := range listed {
		v := verdictNone
		switch r.State {
		case "APPROVED":
			v = verdictApprove
		case "CHANGES_REQUESTED":
			v = verdictChanges
		}
		out[i] = prReview{ID: r.ID, Verdict: v, CommitSHA: r.CommitID, Body: r.Body, At: r.SubmittedAt,
			Login: r.User.Login, Pending: r.State == "PENDING"}
	}
	return out, true, nil
}

func (g *gitHub) comments(ctx context.Context, number int, reviews []prReview) ([]comment, bool, error) {
	n := strconv.Itoa(number)
	type user struct {
		Login string `json:"login"`
	}
	talk, complete, err := listAll[struct {
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
		User      user   `json:"user"`
	}](ctx, g.a, g.prefix+"/issues/"+n+"/comments")
	if err != nil || !complete {
		return nil, false, err
	}
	var out []comment
	for _, c := range talk {
		out = append(out, comment{Login: c.User.Login, At: c.CreatedAt, Body: c.Body})
	}

	inline, complete, err := listAll[struct {
		Body         string `json:"body"`
		CreatedAt    string `json:"created_at"`
		Path         string `json:"path"`
		Line         int    `json:"line"`
		OriginalLine int    `json:"original_line"`
		ReviewID     int64  `json:"pull_request_review_id"`
		User         user   `json:"user"`
	}](ctx, g.a, g.prefix+"/pulls/"+n+"/comments")
	if err != nil || !complete {
		return nil, false, err
	}
	// A comment on a line is written while a review is still a draft, and
	// nobody sees it until the review is submitted. It counts from then:
	// otherwise a mark set in between would hide it for good.
	submitted := map[int64]string{}
	for _, r := range reviews {
		submitted[r.ID] = r.At
	}
	for _, c := range inline {
		line := c.Line
		if line == 0 {
			line = c.OriginalLine
		}
		when := c.CreatedAt
		if s := submitted[c.ReviewID]; c.ReviewID != 0 && s > when {
			when = s // both are UTC in the same format, so they compare as text
		}
		out = append(out, comment{Login: c.User.Login, At: when, Body: c.Body, Path: c.Path, Line: line})
	}
	return out, true, nil
}

func (g *gitHub) comment(ctx context.Context, number int, body string) error {
	return g.a.do(ctx, http.MethodPost, g.prefix+"/issues/"+strconv.Itoa(number)+"/comments", nil, map[string]string{"body": body}, nil)
}

func (g *gitHub) commitTime(ctx context.Context, sha string) (time.Time, error) {
	var c struct {
		Commit struct {
			Committer struct {
				Date string `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := g.a.do(ctx, http.MethodGet, g.prefix+"/commits/"+sha, nil, nil, &c); err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, c.Commit.Committer.Date)
}

func (g *gitHub) deleteBranch(ctx context.Context, branch string) error {
	err := g.a.do(ctx, http.MethodDelete, g.prefix+"/git/refs/heads/"+branch, nil, nil, nil)
	var ae *apiError
	if errors.As(err, &ae) && (ae.Status == http.StatusNotFound || ae.Status == http.StatusUnprocessableEntity) {
		return nil
	}
	return err
}

type gitObject struct {
	SHA string `json:"sha"`
}

// storeShots keeps the pictures in the repository itself, on one branch
// that holds nothing else (SPEC 24). GitHub has no other place an App can
// put a picture where a pull request of a private repository can show it.
// Every publish adds one commit with its pictures below <ticket>/<commit>/,
// and the pull request points at that commit, so later pictures never
// change what an earlier pull request shows.
func (g *gitHub) storeShots(ctx context.Context, key, head string, shots []worker.Screenshot) ([]shotLink, error) {
	branch := g.shotBranch
	if !mirror.ValidBranch(branch) || !shaRE.MatchString(head) {
		return nil, errors.New("the branch for screenshots cannot be named")
	}
	a, prefix := g.a, g.prefix
	dir := key + "/" + head[:12]
	type entry struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
		Type string `json:"type"`
		SHA  string `json:"sha"`
	}
	var entries []entry
	for i, s := range shots {
		var blob gitObject
		in := map[string]string{"content": base64.StdEncoding.EncodeToString(s.PNG), "encoding": "base64"}
		if err := a.do(ctx, http.MethodPost, prefix+"/git/blobs", nil, in, &blob); err != nil {
			return nil, err
		}
		entries = append(entries, entry{dir + "/" + strconv.Itoa(i+1) + ".png", "100644", "blob", blob.SHA})
	}

	// Two machines can publish at the same moment. The branch only moves
	// forward, so the slower one finds that out and builds on the other.
	var commit gitObject
	for try := 0; ; try++ {
		var ref struct {
			Object gitObject `json:"object"`
		}
		err := a.do(ctx, http.MethodGet, prefix+"/git/ref/heads/"+branch, nil, nil, &ref)
		var ae *apiError
		exists := err == nil
		if err != nil && !(errors.As(err, &ae) && ae.Status == http.StatusNotFound) {
			return nil, err
		}
		tree := map[string]any{"tree": entries}
		parents := []string{}
		if exists {
			var tip struct {
				Tree gitObject `json:"tree"`
			}
			if err := a.do(ctx, http.MethodGet, prefix+"/git/commits/"+ref.Object.SHA, nil, nil, &tip); err != nil {
				return nil, err
			}
			tree["base_tree"], parents = tip.Tree.SHA, []string{ref.Object.SHA}
		}
		var made gitObject
		if err := a.do(ctx, http.MethodPost, prefix+"/git/trees", nil, tree, &made); err != nil {
			return nil, err
		}
		in := map[string]any{"message": "Screenshots for " + key, "tree": made.SHA, "parents": parents}
		if err := a.do(ctx, http.MethodPost, prefix+"/git/commits", nil, in, &commit); err != nil {
			return nil, err
		}
		if exists {
			err = a.do(ctx, http.MethodPatch, prefix+"/git/refs/heads/"+branch, nil, map[string]any{"sha": commit.SHA, "force": false}, nil)
		} else {
			err = a.do(ctx, http.MethodPost, prefix+"/git/refs", nil, map[string]string{"ref": "refs/heads/" + branch, "sha": commit.SHA}, nil)
		}
		if err == nil {
			break
		}
		if !(errors.As(err, &ae) && ae.Status == http.StatusUnprocessableEntity) || try >= 2 {
			return nil, err
		}
	}
	if !shaRE.MatchString(commit.SHA) {
		return nil, errors.New("the commit with the screenshots has no usable name")
	}

	links := make([]shotLink, len(shots))
	for i, s := range shots {
		// A picture of a private repository is shown to whoever may read
		// the repository, through their own session with GitHub.
		links[i] = shotLink{s.Caption, fmt.Sprintf("%s/%s/%s/blob/%s/%s/%s/%d.png?raw=true", g.web, g.owner, g.repo,
			commit.SHA, url.PathEscape(key), head[:12], i+1)}
	}
	return links, nil
}

var _ host = (*gitHub)(nil)
