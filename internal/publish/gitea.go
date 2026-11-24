package publish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/gitea"
	"github.com/iandeherdt/Maestro/internal/worker"
)

// Gitea, and Forgejo, which keeps Gitea's API, as the code host (REST, API
// v1). The pull request is much like GitHub's. The service's identity is
// a user account of its own with Write on the repository, so the owner can
// approve what it opens.
//
// Gitea's own words differ in places: a draft is a title that starts with
// "WIP:" or "[WIP]"; lists are paged with limit, and the instance decides
// the largest page; a commit status has its result under "status".

// ValidGiteaRepo reports whether path names a Gitea repository.
func ValidGiteaRepo(path string) bool { return gitea.ValidRepo(path) }

// wipRE is the prefixes Gitea reads as work in progress, as it ships.
var wipRE = regexp.MustCompile(`(?i)^\s*(wip:|\[wip\])\s*`)

// giteaPage is the page size asked for. An instance can give fewer, so a
// list ends at an empty page, not at a short one.
const giteaPage = 50

// gitea is one repository on Gitea or Forgejo.
type giteaHost struct {
	a           *api
	web         string
	owner, repo string
	prefix      string
}

func (p *Publisher) gitea(path string) (*giteaHost, error) {
	if !ValidGiteaRepo(path) {
		return nil, errors.New("workspace.repo must be owner/name")
	}
	web, err := gitea.CheckWeb(p.Config().Workspace.HostURL)
	if err != nil {
		return nil, err
	}
	a, err := newAPI(web+"/api/v1", p.HTTP, p.Token)
	if err != nil {
		return nil, err
	}
	a.plain = true
	owner, repo, _ := strings.Cut(path, "/")
	return &giteaHost{a: a, web: web, owner: owner, repo: repo,
		prefix: "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)}, nil
}

func (g *giteaHost) gitURL() string { return g.web + "/" + g.owner + "/" + g.repo + ".git" }

// giteaList reads a list page by page until a page is empty. It reports
// false when there were more pages than it reads.
func giteaList[T any](ctx context.Context, a *api, path string, q url.Values) ([]T, bool, error) {
	var all []T
	for page := 1; page <= 2*maxReviewPages; page++ {
		var items []T
		pq := url.Values{"limit": {strconv.Itoa(giteaPage)}, "page": {strconv.Itoa(page)}}
		for k, v := range q {
			pq[k] = v
		}
		if err := a.do(ctx, http.MethodGet, path, pq, nil, &items); err != nil {
			return nil, false, err
		}
		if len(items) == 0 {
			return all, true, nil
		}
		all = append(all, items...)
	}
	return all, false, nil
}

type giteaPull struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	Title  string `json:"title"`
	// Draft is set by newer versions; the title says it on all.
	Draft bool `json:"draft"`
	User  struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (pr giteaPull) change() change {
	c := change{Number: pr.Number, State: changeClosed, Draft: pr.Draft || wipRE.MatchString(pr.Title),
		HeadSHA: pr.Head.SHA, Author: pr.User.Login, id: pr.Title}
	switch {
	case pr.Merged:
		c.State = changeMerged
	case pr.State == "open":
		c.State = changeOpen
	}
	return c
}

// own reports whether a pull request comes from branch of the repository
// itself, not from a fork that reuses the name.
func (g *giteaHost) own(pr giteaPull, branch string) bool {
	return pr.Number > 0 && pr.Head.Ref == branch && pr.Head.Repo != nil &&
		strings.EqualFold(pr.Head.Repo.FullName, g.owner+"/"+g.repo)
}

func (g *giteaHost) upsert(ctx context.Context, branch, base, title, body string, draft bool) (int, error) {
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
	open, _, err := giteaList[giteaPull](ctx, g.a, g.prefix+"/pulls", url.Values{"state": {"open"}})
	if err != nil {
		return 0, err
	}
	for _, p := range open {
		if !g.own(p, branch) {
			continue
		}
		if p.Base.Ref != base {
			return 0, fmt.Errorf("an open pull request for this branch targets %q instead of the base branch", clean(p.Base.Ref, 100))
		}
		// The title says whether it is a draft; an update keeps that.
		t := title
		if p.change().Draft {
			t = "WIP: " + title
		}
		err := g.a.do(ctx, http.MethodPatch, g.prefix+"/pulls/"+strconv.Itoa(p.Number), nil,
			map[string]any{"title": t, "body": body}, nil)
		return p.Number, err
	}
	t := title
	if draft {
		t = "WIP: " + title
	}
	var created giteaPull
	if err := g.a.do(ctx, http.MethodPost, g.prefix+"/pulls", nil,
		map[string]any{"title": t, "head": branch, "base": base, "body": body}, &created); err != nil {
		return 0, err
	}
	if created.Number <= 0 {
		return 0, errors.New("the created pull request has no number")
	}
	return created.Number, nil
}

func (g *giteaHost) change(ctx context.Context, number int) (change, error) {
	var pr giteaPull
	if err := g.a.do(ctx, http.MethodGet, g.prefix+"/pulls/"+strconv.Itoa(number), nil, nil, &pr); err != nil {
		return change{}, err
	}
	return pr.change(), nil
}

// changes lists the pull requests of the branch, newest first. Gitea's
// list has no filter by branch, so the recent ones are read and sorted out.
func (g *giteaHost) changes(ctx context.Context, branch string) ([]change, error) {
	var out []change
	for page := 1; page <= maxReviewPages; page++ {
		var list []giteaPull
		q := url.Values{"state": {"all"}, "sort": {"newest"}, "limit": {strconv.Itoa(giteaPage)}, "page": {strconv.Itoa(page)}}
		if err := g.a.do(ctx, http.MethodGet, g.prefix+"/pulls", q, nil, &list); err != nil {
			return nil, err
		}
		for _, p := range list {
			if g.own(p, branch) {
				out = append(out, p.change())
			}
		}
		if len(list) == 0 || len(out) > 0 {
			break
		}
	}
	return out, nil
}

// giteaResult reads a commit status. A warning is not counted as passed.
func giteaResult(state string) result {
	switch state {
	case "success":
		return resPassed
	case "failure", "error", "warning":
		return resFailed
	}
	return resPending
}

// checks reads the latest status of each context. Gitea Actions and
// Forgejo Actions report each job as one, as does anything outside.
func (g *giteaHost) checks(ctx context.Context, sha string) (map[string]check, bool, error) {
	results := map[string]check{}
	for page := 1; page <= maxCheckPages; page++ {
		var combined struct {
			Statuses []struct {
				Context     string `json:"context"`
				Status      string `json:"status"`
				State       string `json:"state"`
				Description string `json:"description"`
			} `json:"statuses"`
		}
		q := url.Values{"limit": {strconv.Itoa(giteaPage)}, "page": {strconv.Itoa(page)}}
		if err := g.a.do(ctx, http.MethodGet, g.prefix+"/commits/"+sha+"/status", q, nil, &combined); err != nil {
			return nil, false, err
		}
		if len(combined.Statuses) == 0 {
			return results, true, nil
		}
		for _, s := range combined.Statuses {
			st := s.Status
			if st == "" {
				st = s.State
			}
			res := giteaResult(st)
			if prev, ok := results[s.Context]; !ok || worse(res, prev.res) {
				results[s.Context] = check{res, s.Description}
			}
		}
	}
	return results, false, nil
}

// markReady takes the work-in-progress prefix off the title. c.id holds
// the title as it was read.
func (g *giteaHost) markReady(ctx context.Context, c change) error {
	title := wipRE.ReplaceAllString(c.id, "")
	if strings.TrimSpace(title) == "" {
		return errors.New("the pull request has no title to keep")
	}
	var pr giteaPull
	if err := g.a.do(ctx, http.MethodPatch, g.prefix+"/pulls/"+strconv.Itoa(c.Number), nil,
		map[string]any{"title": title}, &pr); err != nil {
		return err
	}
	if pr.change().Draft {
		return errors.New("Gitea did not mark the pull request as ready")
	}
	return nil
}

func (g *giteaHost) reviews(ctx context.Context, number int) ([]prReview, bool, error) {
	listed, complete, err := giteaList[struct {
		ID          int64  `json:"id"`
		State       string `json:"state"`
		CommitID    string `json:"commit_id"`
		Body        string `json:"body"`
		SubmittedAt string `json:"submitted_at"`
		Dismissed   bool   `json:"dismissed"`
		User        *struct {
			Login string `json:"login"`
		} `json:"user"`
	}](ctx, g.a, g.prefix+"/pulls/"+strconv.Itoa(number)+"/reviews", nil)
	if err != nil || !complete {
		return nil, complete, err
	}
	var out []prReview
	for _, r := range listed {
		// A review request has no author's verdict, and a dismissed
		// review no longer counts.
		if r.User == nil || r.State == "REQUEST_REVIEW" || r.Dismissed {
			continue
		}
		v := verdictNone
		switch r.State {
		case "APPROVED":
			v = verdictApprove
		case "REQUEST_CHANGES":
			v = verdictChanges
		}
		out = append(out, prReview{ID: r.ID, Verdict: v, CommitSHA: r.CommitID, Body: r.Body, At: utc(r.SubmittedAt),
			Login: r.User.Login, Pending: r.State == "PENDING"})
	}
	return out, true, nil
}

// utc writes a time in UTC, as the shared code compares times as text.
// Gitea writes them in the server's zone.
func utc(at string) string {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return at
	}
	return t.UTC().Format(time.RFC3339)
}

func (g *giteaHost) comments(ctx context.Context, number int, reviews []prReview) ([]comment, bool, error) {
	n := strconv.Itoa(number)
	type user struct {
		Login string `json:"login"`
	}
	talk, complete, err := giteaList[struct {
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
		User      user   `json:"user"`
	}](ctx, g.a, g.prefix+"/issues/"+n+"/comments", nil)
	if err != nil || !complete {
		return nil, false, err
	}
	var out []comment
	for _, c := range talk {
		out = append(out, comment{Login: c.User.Login, At: utc(c.CreatedAt), Body: c.Body})
	}
	// Comments on lines belong to a review and are seen once it is
	// submitted; they count from then.
	for _, r := range reviews {
		if r.Pending || r.At == "" {
			continue
		}
		var inline []struct {
			Body             string `json:"body"`
			CreatedAt        string `json:"created_at"`
			Path             string `json:"path"`
			Position         int    `json:"position"`
			OriginalPosition int    `json:"original_position"`
			User             user   `json:"user"`
		}
		if err := g.a.do(ctx, http.MethodGet, g.prefix+"/pulls/"+n+"/reviews/"+strconv.FormatInt(r.ID, 10)+"/comments", nil, nil, &inline); err != nil {
			return nil, false, err
		}
		for _, c := range inline {
			line := c.Position
			if line == 0 {
				line = c.OriginalPosition
			}
			when := utc(c.CreatedAt)
			if r.At > when {
				when = r.At
			}
			out = append(out, comment{Login: c.User.Login, At: when, Body: c.Body, Path: c.Path, Line: line})
		}
	}
	return out, true, nil
}

func (g *giteaHost) comment(ctx context.Context, number int, body string) error {
	return g.a.do(ctx, http.MethodPost, g.prefix+"/issues/"+strconv.Itoa(number)+"/comments", nil, map[string]string{"body": body}, nil)
}

func (g *giteaHost) commitTime(ctx context.Context, sha string) (time.Time, error) {
	if !shaRE.MatchString(sha) {
		return time.Time{}, errors.New("the commit has no usable name")
	}
	var c struct {
		Commit struct {
			Committer struct {
				Date string `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := g.a.do(ctx, http.MethodGet, g.prefix+"/git/commits/"+sha, url.Values{"stat": {"false"}, "files": {"false"}}, nil, &c); err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, c.Commit.Committer.Date)
}

func (g *giteaHost) deleteBranch(ctx context.Context, branch string) error {
	err := g.a.do(ctx, http.MethodDelete, g.prefix+"/branches/"+url.PathEscape(branch), nil, nil, nil)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// errNoShots says that pictures are not stored on Gitea yet: its API can
// attach a file only to a pull request that exists, and the pictures are
// stored before it is opened. The pull request then says the pictures were
// taken but not stored.
var errNoShots = errors.New("pictures are not stored on Gitea yet")

func (g *giteaHost) storeShots(context.Context, string, string, []worker.Screenshot) ([]shotLink, error) {
	return nil, errNoShots
}

var _ host = (*giteaHost)(nil)
