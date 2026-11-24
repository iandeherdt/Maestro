package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/gitlab"
	"github.com/iandeherdt/Maestro/internal/worker"
)

// GitLab as the code host (REST, API v4). A merge request stands for the
// pull request. The service's identity is a project access token, which
// comes with a bot user of its own, so the owner can approve what it opens.
//
// GitLab's own words differ in places: a merge request has an iid, its
// number within the project; "opened" is open; a draft is a title that
// starts with "Draft:". Pictures are uploads of the project, shown to whoever
// may read it.

// DefaultGitLab is gitlab.com.
const DefaultGitLab = gitlab.Default

// ValidGitLabPath reports whether path names a GitLab project.
func ValidGitLabPath(path string) bool { return gitlab.ValidPath(path) }

var draftRE = regexp.MustCompile(`(?i)^\s*(\[draft\]|\(draft\)|draft:|draft\s*-\s*|draft\s)\s*`)

// gitLab is one project on GitLab.
type gitLab struct {
	a *api
	// web is the instance, such as https://gitlab.com, without a slash at
	// the end.
	web  string
	path string
	// prefix is the project's REST path, with the path escaped as one
	// segment.
	prefix string

	// projectID is the project's number, read once: a merge request from
	// a fork has another one as its source.
	projectID int64
}

func (p *Publisher) gitLab(path string) (*gitLab, error) {
	if !ValidGitLabPath(path) {
		return nil, errors.New("workspace.repo must be the project's path, such as group/name")
	}
	web := strings.TrimSuffix(p.Config().Workspace.HostURL, "/")
	if web == "" {
		web = DefaultGitLab
	}
	a, err := newAPI(web+"/api/v4", p.HTTP, p.Token)
	if err != nil {
		return nil, err
	}
	a.plain = true
	return &gitLab{a: a, web: web, path: path, prefix: "/projects/" + url.PathEscape(path)}, nil
}

func (g *gitLab) gitURL() string { return g.web + "/" + g.path + ".git" }

// id returns the project's number.
func (g *gitLab) id(ctx context.Context) (int64, string, error) {
	var p struct {
		ID            int64  `json:"id"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.a.do(ctx, http.MethodGet, g.prefix, nil, nil, &p); err != nil {
		return 0, "", err
	}
	if p.ID <= 0 {
		return 0, "", errors.New("GitLab reports no project number")
	}
	g.projectID = p.ID
	return p.ID, p.DefaultBranch, nil
}

func (g *gitLab) project(ctx context.Context) (int64, error) {
	if g.projectID > 0 {
		return g.projectID, nil
	}
	id, _, err := g.id(ctx)
	return id, err
}

type mergeRequest struct {
	IID             int    `json:"iid"`
	State           string `json:"state"`
	Draft           bool   `json:"draft"`
	WorkInProgress  bool   `json:"work_in_progress"`
	Title           string `json:"title"`
	SHA             string `json:"sha"`
	SourceBranch    string `json:"source_branch"`
	TargetBranch    string `json:"target_branch"`
	SourceProjectID int64  `json:"source_project_id"`
	TargetProjectID int64  `json:"target_project_id"`
	Author          struct {
		Username string `json:"username"`
	} `json:"author"`
}

func (m mergeRequest) change() change {
	c := change{Number: m.IID, State: changeClosed, Draft: m.Draft || m.WorkInProgress, HeadSHA: m.SHA, Author: m.Author.Username}
	switch m.State {
	case "merged":
		c.State = changeMerged
	case "opened":
		c.State = changeOpen
	}
	return c
}

func (g *gitLab) upsert(ctx context.Context, branch, base, title, body string, draft bool) (int, error) {
	if base == "" {
		_, def, err := g.id(ctx)
		if err != nil {
			return 0, err
		}
		if def == "" {
			return 0, errors.New("the project reports no default branch")
		}
		base = def
	}
	if base == branch {
		return 0, errors.New("the head branch is the base branch")
	}
	pid, err := g.project(ctx)
	if err != nil {
		return 0, err
	}
	var open []mergeRequest
	q := url.Values{"source_branch": {branch}, "state": {"opened"}, "per_page": {"10"}}
	if err := g.a.do(ctx, http.MethodGet, g.prefix+"/merge_requests", q, nil, &open); err != nil {
		return 0, err
	}
	for _, m := range open {
		if m.SourceBranch != branch || m.SourceProjectID != pid || m.IID <= 0 {
			continue
		}
		if m.TargetBranch != base {
			return 0, fmt.Errorf("an open merge request for this branch targets %q instead of the base branch", clean(m.TargetBranch, 100))
		}
		// The title says whether it is a draft; an update keeps that.
		t := title
		if m.Draft || m.WorkInProgress {
			t = "Draft: " + title
		}
		err := g.a.do(ctx, http.MethodPut, g.prefix+"/merge_requests/"+strconv.Itoa(m.IID), nil,
			map[string]any{"title": t, "description": body}, nil)
		return m.IID, err
	}
	t := title
	if draft {
		t = "Draft: " + title
	}
	var created mergeRequest
	err = g.a.do(ctx, http.MethodPost, g.prefix+"/merge_requests", nil, map[string]any{
		"source_branch": branch, "target_branch": base, "title": t, "description": body,
		"remove_source_branch": false}, &created)
	if err != nil {
		return 0, err
	}
	if created.IID <= 0 {
		return 0, errors.New("the created merge request has no number")
	}
	return created.IID, nil
}

func (g *gitLab) change(ctx context.Context, number int) (change, error) {
	var m mergeRequest
	if err := g.a.do(ctx, http.MethodGet, g.prefix+"/merge_requests/"+strconv.Itoa(number), nil, nil, &m); err != nil {
		return change{}, err
	}
	c := m.change()
	c.id = m.Title
	return c, nil
}

func (g *gitLab) changes(ctx context.Context, branch string) ([]change, error) {
	pid, err := g.project(ctx)
	if err != nil {
		return nil, err
	}
	var list []mergeRequest
	q := url.Values{"source_branch": {branch}, "state": {"all"}, "order_by": {"created_at"}, "sort": {"desc"}, "per_page": {"20"}}
	if err := g.a.do(ctx, http.MethodGet, g.prefix+"/merge_requests", q, nil, &list); err != nil {
		return nil, err
	}
	var out []change
	for _, m := range list {
		if m.SourceBranch != branch || m.SourceProjectID != pid || m.IID <= 0 {
			continue
		}
		out = append(out, m.change())
	}
	return out, nil
}

// gitLabResult reads a commit status. A job that may fail without failing
// the pipeline counts as passed when it fails, as GitLab itself counts it.
func gitLabResult(status string, allowFailure bool) result {
	switch status {
	case "success", "skipped":
		return resPassed
	case "failed", "canceled":
		if allowFailure {
			return resPassed
		}
		return resFailed
	}
	// created, waiting_for_resource, preparing, pending, running, manual,
	// scheduled
	return resPending
}

// checks reads the commit's statuses. Every job of a pipeline reports one,
// under its name, and so does anything outside GitLab that reports on the
// commit.
func (g *gitLab) checks(ctx context.Context, sha string) (map[string]check, bool, error) {
	results := map[string]check{}
	for page := 1; page <= maxCheckPages; page++ {
		var list []struct {
			Name         string `json:"name"`
			Status       string `json:"status"`
			Description  string `json:"description"`
			AllowFailure bool   `json:"allow_failure"`
		}
		q := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if err := g.a.do(ctx, http.MethodGet, g.prefix+"/repository/commits/"+sha+"/statuses", q, nil, &list); err != nil {
			return nil, false, err
		}
		for _, s := range list {
			res := gitLabResult(s.Status, s.AllowFailure)
			if prev, ok := results[s.Name]; !ok || worse(res, prev.res) {
				results[s.Name] = check{res, s.Description}
			}
		}
		if len(list) < 100 {
			return results, true, nil
		}
	}
	return results, false, nil
}

// markReady takes the draft prefix off the title. c.id holds the title as
// it was read.
func (g *gitLab) markReady(ctx context.Context, c change) error {
	title := draftRE.ReplaceAllString(c.id, "")
	if title == "" {
		return errors.New("the merge request has no title to keep")
	}
	var m mergeRequest
	if err := g.a.do(ctx, http.MethodPut, g.prefix+"/merge_requests/"+strconv.Itoa(c.Number), nil,
		map[string]any{"title": title}, &m); err != nil {
		return err
	}
	if m.Draft || m.WorkInProgress {
		return errors.New("GitLab did not mark the merge request as ready")
	}
	return nil
}

type gitLabNote struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	System    bool   `json:"system"`
	Type      string `json:"type"`
	Author    struct {
		Username string `json:"username"`
	} `json:"author"`
	Position *struct {
		NewPath string `json:"new_path"`
		OldPath string `json:"old_path"`
		NewLine int    `json:"new_line"`
		OldLine int    `json:"old_line"`
	} `json:"position"`
}

// notes reads every note of a merge request, oldest first.
func (g *gitLab) notes(ctx context.Context, number int) ([]gitLabNote, bool, error) {
	var all []gitLabNote
	for page := 1; ; page++ {
		if page > maxReviewPages {
			return all, false, nil
		}
		var items []gitLabNote
		q := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}, "sort": {"asc"}, "order_by": {"created_at"}}
		if err := g.a.do(ctx, http.MethodGet, g.prefix+"/merge_requests/"+strconv.Itoa(number)+"/notes", q, nil, &items); err != nil {
			return nil, false, err
		}
		all = append(all, items...)
		if len(items) < 100 {
			return all, true, nil
		}
	}
}

// reviews are read from the merge request's system notes: GitLab writes
// one when someone approves, takes an approval back or requests changes,
// with the time. A verdict counts for the head commit when it was given
// after that commit was made; otherwise it was about an earlier one.
func (g *gitLab) reviews(ctx context.Context, number int) ([]prReview, bool, error) {
	notes, complete, err := g.notes(ctx, number)
	if err != nil || !complete {
		return nil, complete, err
	}
	m, err := g.change(ctx, number)
	if err != nil {
		return nil, false, err
	}
	var made time.Time
	if shaRE.MatchString(m.HeadSHA) {
		made, _ = g.commitTime(ctx, m.HeadSHA)
	}
	var out []prReview
	for _, n := range notes {
		if !n.System {
			continue
		}
		v := ""
		switch body := strings.ToLower(strings.TrimSpace(n.Body)); {
		case body == "approved this merge request":
			v = verdictApprove
		case body == "requested changes":
			v = verdictChanges
		case body == "unapproved this merge request":
			v = verdictNone
		default:
			continue
		}
		at, err := time.Parse(time.RFC3339, n.CreatedAt)
		commit := ""
		if err == nil && !made.IsZero() && at.After(made) {
			commit = m.HeadSHA
		}
		// The verdict's words are GitLab's, not the reviewer's: the
		// summary of a review is a note of its own, read by comments.
		out = append(out, prReview{ID: n.ID, Verdict: v, CommitSHA: commit, At: n.CreatedAt, Login: n.Author.Username})
	}
	return out, true, nil
}

func (g *gitLab) comments(ctx context.Context, number int, _ []prReview) ([]comment, bool, error) {
	notes, complete, err := g.notes(ctx, number)
	if err != nil || !complete {
		return nil, false, err
	}
	var out []comment
	for _, n := range notes {
		if n.System {
			continue
		}
		c := comment{Login: n.Author.Username, At: n.CreatedAt, Body: n.Body}
		if n.Position != nil {
			c.Path, c.Line = n.Position.NewPath, n.Position.NewLine
			if c.Path == "" {
				c.Path = n.Position.OldPath
			}
			if c.Line == 0 {
				c.Line = n.Position.OldLine
			}
			if c.Path == "" {
				// A comment on a line always names its file. One that does
				// not cannot carry the service's mark.
				c.Path = "?"
			}
		}
		out = append(out, c)
	}
	return out, true, nil
}

func (g *gitLab) comment(ctx context.Context, number int, body string) error {
	return g.a.do(ctx, http.MethodPost, g.prefix+"/merge_requests/"+strconv.Itoa(number)+"/notes", nil, map[string]string{"body": body}, nil)
}

func (g *gitLab) commitTime(ctx context.Context, sha string) (time.Time, error) {
	var c struct {
		CommittedDate string `json:"committed_date"`
	}
	if err := g.a.do(ctx, http.MethodGet, g.prefix+"/repository/commits/"+sha, nil, nil, &c); err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, c.CommittedDate)
}

func (g *gitLab) deleteBranch(ctx context.Context, branch string) error {
	err := g.a.do(ctx, http.MethodDelete, g.prefix+"/repository/branches/"+url.PathEscape(branch), nil, nil, nil)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// storeShots uploads each picture to the project. GitLab shows an upload
// to whoever may read the project.
func (g *gitLab) storeShots(ctx context.Context, key, head string, shots []worker.Screenshot) ([]shotLink, error) {
	if !shaRE.MatchString(head) {
		return nil, errors.New("the commit of the screenshots has no usable name")
	}
	links := make([]shotLink, len(shots))
	for i, s := range shots {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		name := fmt.Sprintf("%s-%s-%d.png", key, head[:12], i+1)
		part, err := w.CreateFormFile("file", name)
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(s.PNG); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		var up struct {
			FullPath string `json:"full_path"`
			URL      string `json:"url"`
		}
		if err := g.a.raw(ctx, http.MethodPost, g.prefix+"/uploads", w.FormDataContentType(), &buf, &up); err != nil {
			return nil, err
		}
		path := up.FullPath
		if path == "" {
			// Before GitLab 17 the answer has only the path below the project.
			path = "/" + g.path + up.URL
		}
		if !strings.HasPrefix(path, "/") || strings.Contains(path, "..") || strings.ContainsAny(path, " ()<>\n") {
			return nil, errors.New("GitLab gave the picture an address that cannot be used")
		}
		links[i] = shotLink{s.Caption, g.web + path}
	}
	return links, nil
}

// raw sends a body that is not JSON, such as an upload.
func (a *api) raw(ctx context.Context, method, path, contentType string, body io.Reader, out any) error {
	u, err := a.url(path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return err
	}
	tok, err := a.token(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", contentType)
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &apiError{Method: method, Path: path, Status: resp.StatusCode}
	}
	if out != nil && json.Unmarshal(raw, out) != nil {
		return fmt.Errorf("%s %s: the response could not be read", method, path)
	}
	return nil
}

var _ host = (*gitLab)(nil)
