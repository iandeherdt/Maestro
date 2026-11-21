// Package github is the GitHub Issues adapter (SPEC section 12.5).
//
// This file implements state_source "labels": an issue's state is the one
// label carrying the state prefix. Anyone can open an issue or comment on a
// public repository, so nothing here trusts either. An issue is
// dispatchable only when an allowlisted account wrote it and an allowlisted
// account applied the labels that approve and route it.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iandeherdt/Symphony-beads/internal/config"
	"github.com/iandeherdt/Symphony-beads/internal/issue"
	"github.com/iandeherdt/Symphony-beads/internal/tracker"
)

const (
	// NoState is reported for an open issue without exactly one state
	// label. It is never an active or a terminal state.
	NoState = "No Status"

	defaultAPI    = "https://api.github.com"
	defaultPrefix = "status:"
	blockerTTL    = 5 * time.Minute
	reworkState   = "Rework"
)

// Options are the inputs that do not come from the workflow file.
type Options struct {
	// Env resolves the $VAR reference in provider.token.
	Env func(string) string
	// Token overrides provider.token, for a GitHub App installation token.
	Token TokenSource
	HTTP  *http.Client
	Log   *slog.Logger
	Now   func() time.Time
}

// Adapter implements tracker.Tracker for one repository.
type Adapter struct {
	c      *client
	owner  string
	repo   string
	prefix string // normalized state label prefix
	bot    string // normalized login the service acts as
	worker string
	log    *slog.Logger
	now    func() time.Time

	required     []string
	firstActive  string
	terminal     string
	ownedStates  map[string]string // normalized -> configured name
	authors      map[string]bool
	commenters   map[string]bool
	blockersOnce sync.Once

	mu      sync.Mutex
	numbers map[string]int       // node id -> issue number
	cache   map[int]*cachedIssue // issue number -> derived facts
}

type cachedIssue struct {
	updatedAt  string
	provenance bool
	blockers   []issue.Blocker
	blockersAt time.Time
}

var (
	repoRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$`)
	varRE   = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)$`)
	loginRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*(\[bot\])?$`)
)

func norm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func loginSet(list []string) map[string]bool {
	m := map[string]bool{}
	for _, l := range list {
		m[norm(l)] = true
	}
	return m
}

func cfgErr(format string, a ...any) error {
	return &tracker.Error{Category: tracker.InvalidConfig, Msg: fmt.Sprintf(format, a...)}
}

// New validates tracker.provider and builds the adapter.
func New(cfg *config.Config, opt Options) (*Adapter, error) {
	p := cfg.Tracker.Provider
	known := map[string]bool{"repo": true, "token": true, "api_url": true, "state_source": true,
		"state_label_prefix": true, "bot_login": true, "project": true}
	for k := range p {
		if !known[k] {
			return nil, cfgErr("tracker.provider.%s is not a known key", k)
		}
	}
	str := func(key, def string) (string, error) {
		v, ok := p[key]
		if !ok || v == nil {
			return def, nil
		}
		s, ok := v.(string)
		if !ok {
			return "", cfgErr("tracker.provider.%s must be a string", key)
		}
		return s, nil
	}
	repo, err := str("repo", "")
	if err != nil {
		return nil, err
	}
	if !repoRE.MatchString(repo) {
		return nil, cfgErr("tracker.provider.repo must be owner/name")
	}
	source, err := str("state_source", "")
	if err != nil {
		return nil, err
	}
	if source != "labels" {
		if source == "project" {
			return nil, cfgErr("tracker.provider.state_source: project is not implemented yet, use labels")
		}
		return nil, cfgErr("tracker.provider.state_source must be project or labels")
	}
	prefix, err := str("state_label_prefix", defaultPrefix)
	if err != nil {
		return nil, err
	}
	if norm(prefix) == "" {
		return nil, cfgErr("tracker.provider.state_label_prefix must not be empty")
	}
	bot, err := str("bot_login", "")
	if err != nil {
		return nil, err
	}
	if !loginRE.MatchString(bot) {
		return nil, cfgErr("tracker.provider.bot_login is required with state_source: labels: the login the service acts as, for example my-app[bot]")
	}
	apiURL, err := str("api_url", defaultAPI)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(apiURL)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, cfgErr("tracker.provider.api_url is not a valid URL")
	}
	if base.Scheme != "https" && !(base.Scheme == "http" && isLoopback(base.Hostname())) {
		return nil, cfgErr("tracker.provider.api_url must use https")
	}

	ts := opt.Token
	if ts == nil {
		ref, err := str("token", "$GITHUB_TOKEN")
		if err != nil {
			return nil, err
		}
		m := varRE.FindStringSubmatch(ref)
		if m == nil {
			// A token written into the workflow file would end up in the
			// repository. Only a reference to the environment is accepted.
			return nil, cfgErr("tracker.provider.token must be a $VARIABLE reference, not a literal value")
		}
		var tok string
		if opt.Env != nil {
			tok = opt.Env(m[1])
		}
		if tok == "" {
			return nil, &tracker.Error{Category: tracker.MissingSecret, Msg: "environment variable " + m[1] + " is empty"}
		}
		ts = func(context.Context) (string, error) { return tok, nil }
	}
	if len(cfg.Tracker.AllowedAuthors) == 0 {
		return nil, cfgErr("tracker.allowed_authors must not be empty")
	}
	if len(cfg.Tracker.ActiveStates) == 0 || len(cfg.Tracker.TerminalStates) == 0 {
		return nil, cfgErr("tracker.active_states and tracker.terminal_states must not be empty")
	}

	hc := opt.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	log := opt.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	owner, name, _ := strings.Cut(repo, "/")
	a := &Adapter{
		c: newClient(base, hc, ts), owner: owner, repo: name, prefix: norm(prefix), bot: norm(bot),
		worker: cfg.Worker.Name, log: log, now: now,
		required:    cfg.Tracker.RequiredLabels,
		firstActive: norm(cfg.Tracker.ActiveStates[0]),
		terminal:    cfg.Tracker.TerminalStates[0],
		authors:     loginSet(cfg.Tracker.AllowedAuthors),
		commenters:  loginSet(cfg.Tracker.AllowedCommenters),
		ownedStates: map[string]string{},
		numbers:     map[string]int{}, cache: map[int]*cachedIssue{},
	}
	// The states the orchestrator itself writes (SPEC 7.3).
	for _, s := range []string{cfg.Tracker.InProgressState, cfg.Tracker.HandoffState, cfg.Tracker.BlockedState,
		cfg.Tracker.DoneState, reworkState} {
		if s != "" {
			a.ownedStates[norm(s)] = s
		}
	}
	if a.ownedStates[a.firstActive] != "" {
		return nil, cfgErr("the first active state %q is the approval state and cannot be one the service writes", cfg.Tracker.ActiveStates[0])
	}
	return a, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (a *Adapter) path(format string, args ...any) string {
	return "/repos/" + a.owner + "/" + a.repo + fmt.Sprintf(format, args...)
}

// ---- wire types -----------------------------------------------------------

type ghUser struct {
	Login string `json:"login"`
}

type ghLabel struct {
	Name string `json:"name"`
}

type ghIssue struct {
	NodeID      string          `json:"node_id"`
	Number      int             `json:"number"`
	Title       string          `json:"title"`
	Body        *string         `json:"body"`
	State       string          `json:"state"`
	HTMLURL     string          `json:"html_url"`
	User        *ghUser         `json:"user"`
	Assignee    *ghUser         `json:"assignee"`
	Labels      []ghLabel       `json:"labels"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
	PullRequest json.RawMessage `json:"pull_request"`
}

type ghEvent struct {
	Event string   `json:"event"`
	Actor *ghUser  `json:"actor"`
	Label *ghLabel `json:"label"`
}

type ghComment struct {
	ID        int64   `json:"id"`
	Body      string  `json:"body"`
	User      *ghUser `json:"user"`
	CreatedAt string  `json:"created_at"`
}

// ---- reads ----------------------------------------------------------------

// FetchIssuesByStates returns open issues carrying the state label of one of
// the given states.
func (a *Adapter) FetchIssuesByStates(ctx context.Context, states []string) ([]issue.Issue, error) {
	var out []issue.Issue
	seen := map[int]bool{}
	for _, state := range states {
		q := url.Values{"state": {"open"}, "labels": {a.prefix + norm(state)}}
		err := a.c.list(ctx, a.path("/issues"), q, func(page json.RawMessage) error {
			var raws []json.RawMessage
			if err := json.Unmarshal(page, &raws); err != nil {
				return terr(tracker.Response, "issue list is not an array")
			}
			for _, raw := range raws {
				var gi ghIssue
				if err := json.Unmarshal(raw, &gi); err != nil {
					a.log.Warn("malformed issue record skipped")
					continue
				}
				if len(gi.PullRequest) > 0 || seen[gi.Number] {
					continue
				}
				is, err := a.normalize(ctx, gi)
				if err != nil {
					if isTransport(err) {
						return err
					}
					a.log.Warn("malformed issue record skipped", "number", gi.Number, "error", err)
					continue
				}
				// An issue with a second state label has no state at all.
				if norm(is.State) != norm(state) {
					continue
				}
				seen[gi.Number] = true
				out = append(out, is)
			}
			return nil
		})
		if errors.Is(err, errNotFound) {
			return nil, terr(tracker.Status, "repository not found, or the token cannot read it")
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

var _ tracker.Tracker = (*Adapter)(nil)

// FetchIssuesByIDs returns current snapshots of issues this adapter has
// returned before. An issue that no longer exists is omitted.
func (a *Adapter) FetchIssuesByIDs(ctx context.Context, ids []string) ([]issue.Issue, error) {
	var out []issue.Issue
	for _, id := range ids {
		a.mu.Lock()
		n, ok := a.numbers[id]
		a.mu.Unlock()
		if !ok {
			continue
		}
		var gi ghIssue
		_, err := a.c.do(ctx, http.MethodGet, a.c.url(a.path("/issues/%d", n), nil), nil, &gi, false)
		if errors.Is(err, errNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		// A transferred or replaced issue is not the one that was approved.
		if gi.NodeID != id {
			continue
		}
		is, err := a.normalize(ctx, gi)
		if err != nil {
			return nil, err
		}
		out = append(out, is)
	}
	return out, nil
}

func isTransport(err error) bool {
	switch tracker.CategoryOf(err) {
	case tracker.Request, tracker.Status, tracker.RateLimited, tracker.MissingSecret, tracker.Pagination:
		return true
	}
	return false
}

var priorityRE = regexp.MustCompile(`^(?:p|priority:)([1-4])$`)

func (a *Adapter) normalize(ctx context.Context, gi ghIssue) (issue.Issue, error) {
	if gi.NodeID == "" || gi.Number <= 0 || strings.TrimSpace(gi.Title) == "" || gi.User == nil || gi.User.Login == "" {
		return issue.Issue{}, terr(tracker.Response, "issue record lacks a required field")
	}
	is := issue.Issue{
		ID: gi.NodeID, Identifier: "GH-" + strconv.Itoa(gi.Number), Title: gi.Title, URL: gi.HTMLURL,
		AuthorID:  gi.User.Login,
		NativeRef: map[string]any{"number": gi.Number, "node_id": gi.NodeID},
	}
	if gi.Body != nil {
		is.Description = *gi.Body
	}
	if gi.Assignee != nil {
		is.AssigneeID = gi.Assignee.Login
	}
	for _, f := range []struct {
		raw string
		dst *time.Time
	}{{gi.CreatedAt, &is.CreatedAt}, {gi.UpdatedAt, &is.UpdatedAt}} {
		if f.raw != "" {
			t, err := time.Parse(time.RFC3339, f.raw)
			if err != nil {
				return issue.Issue{}, terr(tracker.Response, "issue record has a malformed timestamp")
			}
			*f.dst = t
		}
	}
	var stateLabels []string
	for _, l := range gi.Labels {
		name := norm(l.Name)
		if name == "" {
			continue
		}
		is.Labels = append(is.Labels, name)
		if m := priorityRE.FindStringSubmatch(name); m != nil && is.Priority == nil {
			p, _ := strconv.Atoi(m[1])
			is.Priority = &p
		}
		if strings.HasPrefix(name, a.prefix) {
			stateLabels = append(stateLabels, strings.TrimSpace(l.Name)[len(a.prefix):])
		}
	}
	sort.Strings(is.Labels)
	is.ContentHash = issue.ContentHash(is.Title, is.Description)

	switch {
	case gi.State == "closed":
		is.State = a.terminal
	case len(stateLabels) == 1 && strings.TrimSpace(stateLabels[0]) != "":
		is.State = strings.TrimSpace(stateLabels[0])
	default:
		is.State = NoState
	}

	a.mu.Lock()
	a.numbers[gi.NodeID] = gi.Number
	a.mu.Unlock()

	if gi.State == "closed" || is.State == NoState || !a.authors[norm(gi.User.Login)] {
		return is, nil // Dispatchable stays false; no further requests are spent
	}
	approved, blockers, err := a.derived(ctx, gi, is)
	if err != nil {
		return issue.Issue{}, err
	}
	is.BlockedBy = blockers
	is.Dispatchable = approved
	if norm(is.State) == a.firstActive {
		for _, b := range blockers {
			if b.State != "closed" {
				is.Dispatchable = false
			}
		}
	}
	return is, nil
}

// derived returns the approval provenance and the blockers of an issue. The
// provenance is cached until the issue changes; blockers for a short time.
func (a *Adapter) derived(ctx context.Context, gi ghIssue, is issue.Issue) (bool, []issue.Blocker, error) {
	a.mu.Lock()
	c := a.cache[gi.Number]
	a.mu.Unlock()
	if c == nil || c.updatedAt != gi.UpdatedAt || gi.UpdatedAt == "" {
		ok, err := a.provenance(ctx, gi.Number, is)
		if err != nil {
			return false, nil, err
		}
		c = &cachedIssue{updatedAt: gi.UpdatedAt, provenance: ok}
	}
	if norm(is.State) == a.firstActive && (c.blockersAt.IsZero() || a.now().Sub(c.blockersAt) > blockerTTL) {
		blockers, err := a.blockers(ctx, gi.Number)
		if err != nil {
			return false, nil, err
		}
		c.blockers, c.blockersAt = blockers, a.now()
	}
	a.mu.Lock()
	a.cache[gi.Number] = c
	a.mu.Unlock()
	return c.provenance, c.blockers, nil
}

// provenance checks who applied the labels that approve and route the
// issue. For every required label and for the state label, the most recent
// "labeled" event must come from an allowlisted account. The service's own
// account is accepted only for the states the service itself writes, never
// for the approval state and never for a routing label.
func (a *Adapter) provenance(ctx context.Context, number int, is issue.Issue) (bool, error) {
	stateLabel := a.prefix + norm(is.State)
	need := map[string]bool{stateLabel: true}
	for _, l := range a.required {
		need[norm(l)] = true
	}
	last := map[string]string{} // label -> login of the most recent labeler
	err := a.c.list(ctx, a.path("/issues/%d/timeline", number), nil, func(page json.RawMessage) error {
		var events []ghEvent
		if err := json.Unmarshal(page, &events); err != nil {
			return terr(tracker.Response, "timeline is not an array")
		}
		for _, e := range events {
			if e.Event != "labeled" || e.Label == nil {
				continue
			}
			name := norm(e.Label.Name)
			if !need[name] {
				continue
			}
			login := ""
			if e.Actor != nil {
				login = norm(e.Actor.Login)
			}
			last[name] = login // events are in chronological order
		}
		return nil
	})
	if errors.Is(err, errNotFound) {
		return false, terr(tracker.Status, "timeline of issue %d is not readable", number)
	}
	if err != nil {
		return false, err
	}
	for name := range need {
		login, found := last[name]
		if !found || login == "" {
			return false, nil // no record of who applied it: not approved
		}
		if a.authors[login] {
			continue
		}
		if name == stateLabel && login == a.bot && a.ownedStates[norm(is.State)] != "" {
			continue
		}
		return false, nil
	}
	return true, nil
}

func (a *Adapter) blockers(ctx context.Context, number int) ([]issue.Blocker, error) {
	var out []issue.Blocker
	err := a.c.list(ctx, a.path("/issues/%d/dependencies/blocked_by", number), nil, func(page json.RawMessage) error {
		var items []ghIssue
		if err := json.Unmarshal(page, &items); err != nil {
			return terr(tracker.Response, "blocker list is not an array")
		}
		for _, b := range items {
			out = append(out, issue.Blocker{ID: b.NodeID, Identifier: "GH-" + strconv.Itoa(b.Number), State: b.State})
		}
		return nil
	})
	if errors.Is(err, errNotFound) {
		a.blockersOnce.Do(func() {
			a.log.Warn("the issue dependencies API is not available for this repository, blockers are not checked")
		})
		return nil, nil
	}
	return out, err
}

// ---- writes ---------------------------------------------------------------

func numberOf(is issue.Issue) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(is.Identifier, "GH-"))
	if err != nil || n <= 0 || !strings.HasPrefix(is.Identifier, "GH-") {
		return 0, terr(tracker.WriteRejected, "issue identifier %q is not a GitHub issue", is.Identifier)
	}
	return n, nil
}

// SetState replaces the issue's state label. Only the states the
// orchestrator owns can be written.
func (a *Adapter) SetState(ctx context.Context, is issue.Issue, state string) error {
	if a.ownedStates[norm(state)] == "" {
		return terr(tracker.WriteRejected, "the service does not write the state %q", state)
	}
	n, err := numberOf(is)
	if err != nil {
		return err
	}
	var gi ghIssue
	if _, err := a.c.do(ctx, http.MethodGet, a.c.url(a.path("/issues/%d", n), nil), nil, &gi, false); err != nil {
		if errors.Is(err, errNotFound) {
			return terr(tracker.WriteRejected, "issue %s no longer exists", is.Identifier)
		}
		return err
	}
	if gi.NodeID != is.ID {
		return terr(tracker.WriteRejected, "issue %s is not the issue that was dispatched", is.Identifier)
	}
	target := a.prefix + norm(state)
	has := false
	var stale []string
	for _, l := range gi.Labels {
		switch name := norm(l.Name); {
		case name == target:
			has = true
		case strings.HasPrefix(name, a.prefix):
			stale = append(stale, l.Name)
		}
	}
	if !has {
		body := map[string][]string{"labels": {a.prefix + state}}
		if _, err := a.c.do(ctx, http.MethodPost, a.c.url(a.path("/issues/%d/labels", n), nil), body, nil, true); err != nil {
			return err
		}
	}
	for _, name := range stale {
		u := a.c.url(a.path("/issues/%d/labels/", n), nil) + url.PathEscape(name)
		if _, err := a.c.do(ctx, http.MethodDelete, u, nil, nil, true); err != nil && !errors.Is(err, errNotFound) {
			return err
		}
	}
	a.mu.Lock()
	delete(a.cache, n)
	a.mu.Unlock()
	return nil
}

func (a *Adapter) marker() string { return "<!-- symphony-status:" + a.worker + " -->" }

func (a *Adapter) comments(ctx context.Context, n int, q url.Values, each func(ghComment)) error {
	if err := a.c.list(ctx, a.path("/issues/%d/comments", n), q, func(page json.RawMessage) error {
		var cs []ghComment
		if err := json.Unmarshal(page, &cs); err != nil {
			return terr(tracker.Response, "comment list is not an array")
		}
		for _, c := range cs {
			each(c)
		}
		return nil
	}); errors.Is(err, errNotFound) {
		return terr(tracker.Status, "comments of issue %d are not readable", n)
	} else {
		return err
	}
}

// UpsertStatusComment keeps one status comment per issue for this worker.
// Only a comment written by the service's own account is ever edited, so a
// marker planted by someone else is ignored.
func (a *Adapter) UpsertStatusComment(ctx context.Context, is issue.Issue, body string) error {
	n, err := numberOf(is)
	if err != nil {
		return err
	}
	var existing int64
	err = a.comments(ctx, n, nil, func(c ghComment) {
		if existing == 0 && c.User != nil && norm(c.User.Login) == a.bot && strings.HasPrefix(c.Body, a.marker()) {
			existing = c.ID
		}
	})
	if err != nil {
		return err
	}
	payload := map[string]string{"body": a.marker() + "\n" + body}
	if existing != 0 {
		_, err = a.c.do(ctx, http.MethodPatch, a.c.url(a.path("/issues/comments/%d", existing), nil), payload, nil, true)
		return err
	}
	_, err = a.c.do(ctx, http.MethodPost, a.c.url(a.path("/issues/%d/comments", n), nil), payload, nil, true)
	return err
}

// FetchFeedback returns comments made after since by allowlisted
// commenters. Comments by anyone else, including bots, are never returned.
func (a *Adapter) FetchFeedback(ctx context.Context, is issue.Issue, since time.Time) ([]tracker.Feedback, error) {
	n, err := numberOf(is)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	var out []tracker.Feedback
	err = a.comments(ctx, n, q, func(c ghComment) {
		if c.User == nil || !a.commenters[norm(c.User.Login)] {
			return
		}
		at, err := time.Parse(time.RFC3339, c.CreatedAt)
		if err != nil || !at.After(since) {
			return // "since" on the API also matches edits of older comments
		}
		out = append(out, tracker.Feedback{Author: c.User.Login, Body: c.Body, CreatedAt: at})
	})
	return out, err
}
