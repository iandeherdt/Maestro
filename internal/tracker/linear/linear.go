// Package linear is the Linear adapter (SPEC section 12.6).
//
// The board is one Linear team, optionally narrowed to one project. An
// issue's state is its workflow state. A worker is chosen with a label,
// run:<worker>, because Linear has no field for it. An issue is
// dispatchable only when an allowlisted user wrote it, moved it into its
// current state and added each required label. Linear records who did each
// of those in the issue's history, which is what this adapter reads. The
// service's own user counts only for the states the service writes itself,
// and for the tasks it made from a plan the owner approved.
//
// The code stays on GitHub: this adapter is the board, nothing else.
package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/tracker"
)

const (
	// DefaultEndpoint is Linear's API.
	DefaultEndpoint = "https://api.linear.app/graphql"
	reworkState     = "Rework"
	blockerTTL      = 5 * time.Minute
)

// Options are the inputs that do not come from the workflow file.
type Options struct {
	// Env resolves the $VAR reference in provider.api_key.
	Env func(string) string
	// Key overrides provider.api_key, such as a key kept in the setup
	// folder.
	Key  KeySource
	HTTP *http.Client
	Log  *slog.Logger
	Now  func() time.Time
}

// Adapter implements tracker.Tracker for one Linear team.
type Adapter struct {
	c       *client
	team    string // team key, such as ENG
	project string // project slug id or name, or ""
	worker  string
	log     *slog.Logger
	now     func() time.Time

	required    []string
	firstActive string
	ownedStates map[string]string // normalized -> configured name
	authors     map[string]bool   // user ids and emails, lowercased
	commenters  map[string]bool

	mu      sync.Mutex
	me      *user             // the user the API key acts as
	teamID  string            // resolved from the key
	states  map[string]string // normalized state name -> id
	backlog string            // a state of type backlog, where new tasks wait
	cache   map[string]*cached

	// imageHost and imageHTTP replace Linear's upload host and the
	// download client in tests.
	imageHost func(*url.URL) bool
	imageHTTP *http.Client
}

type cached struct {
	updatedAt  string
	provenance bool
	blockers   []issue.Blocker
	blockersAt time.Time
}

var (
	teamRE = regexp.MustCompile(`^[A-Za-z0-9]{1,10}$`)
	varRE  = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)$`)
	// identRE is a Linear identifier, such as ENG-12.
	identRE = regexp.MustCompile(`^[A-Za-z0-9]{1,10}-[1-9][0-9]{0,8}$`)
)

func norm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func set(list []string) map[string]bool {
	m := map[string]bool{}
	for _, l := range list {
		if n := norm(l); n != "" {
			m[n] = true
		}
	}
	return m
}

func cfgErr(format string, a ...any) error {
	return &tracker.Error{Category: tracker.InvalidConfig, Msg: fmt.Sprintf(format, a...)}
}

// New validates tracker.provider and builds the adapter. It makes no
// request.
func New(cfg *config.Config, opt Options) (*Adapter, error) {
	p := cfg.Tracker.Provider
	known := map[string]bool{"endpoint": true, "api_key": true, "team_key": true, "project": true, "repo": true}
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
		return strings.TrimSpace(s), nil
	}
	team, err := str("team_key", "")
	if err != nil {
		return nil, err
	}
	if !teamRE.MatchString(team) {
		return nil, cfgErr("tracker.provider.team_key is required: the key of the Linear team, such as ENG")
	}
	project, err := str("project", "")
	if err != nil {
		return nil, err
	}
	endpoint, err := str("endpoint", DefaultEndpoint)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, cfgErr("tracker.provider.endpoint is not a valid URL")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())) {
		return nil, cfgErr("tracker.provider.endpoint must use https")
	}
	key := opt.Key
	if key == nil {
		ref, err := str("api_key", "$LINEAR_API_KEY")
		if err != nil {
			return nil, err
		}
		m := varRE.FindStringSubmatch(ref)
		if m == nil {
			// A key written into the workflow file would end up in the
			// repository.
			return nil, cfgErr("tracker.provider.api_key must be a $VARIABLE reference, not a literal value")
		}
		var v string
		if opt.Env != nil {
			v = opt.Env(m[1])
		}
		if v == "" {
			return nil, &tracker.Error{Category: tracker.MissingSecret, Msg: "environment variable " + m[1] + " is empty"}
		}
		key = func(context.Context) (string, error) { return v, nil }
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
	a := &Adapter{
		c: newClient(u, hc, key), team: strings.ToUpper(team), project: project,
		worker: cfg.Worker.Name, log: log, now: now,
		required:    cfg.Tracker.RequiredLabels,
		firstActive: norm(cfg.Tracker.ActiveStates[0]),
		ownedStates: map[string]string{},
		authors:     set(cfg.Tracker.AllowedAuthors),
		commenters:  set(cfg.Tracker.AllowedCommenters),
		cache:       map[string]*cached{},
	}
	for _, s := range []string{cfg.Tracker.InProgressState, cfg.Tracker.HandoffState, cfg.Tracker.BlockedState,
		cfg.Tracker.DoneState, reworkState, cfg.Tracker.DesignState} {
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

var _ tracker.Tracker = (*Adapter)(nil)

// ---- wire types -----------------------------------------------------------

type user struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

type state struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type lnIssue struct {
	ID          string  `json:"id"`
	Identifier  string  `json:"identifier"`
	Title       string  `json:"title"`
	Description *string `json:"description"`
	Priority    float64 `json:"priority"`
	URL         string  `json:"url"`
	BranchName  string  `json:"branchName"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
	Creator     *user   `json:"creator"`
	Assignee    *user   `json:"assignee"`
	State       *state  `json:"state"`
	Team        *struct {
		Key string `json:"key"`
	} `json:"team"`
	Project *struct {
		ID string `json:"id"`
	} `json:"project"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
}

const issueFields = `id identifier title description priority url branchName createdAt updatedAt
creator { id email } assignee { id email } state { id name type } team { key } project { id }
labels(first: 100) { nodes { name } }`

// ---- setup -----------------------------------------------------------------

// Key returns the API key, so that the service can keep it out of the
// agent's commits.
func (a *Adapter) Key(ctx context.Context) (string, error) { return a.c.key(ctx) }

// Who asks Linear whom key acts as, before anything is configured.
func Who(ctx context.Context, endpoint, key string) (id, email string, err error) {
	c, err := keyClient(endpoint, key)
	if err != nil {
		return "", "", err
	}
	a := &Adapter{c: c}
	return a.Me(ctx)
}

// Query runs one query or mutation with key, for setup, which works on a
// team before any settings exist. The data is decoded into out.
func Query(ctx context.Context, endpoint, key, query string, vars map[string]any, out any) error {
	c, err := keyClient(endpoint, key)
	if err != nil {
		return err
	}
	return c.do(ctx, query, vars, out)
}

func keyClient(endpoint, key string) (*client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname()))) {
		return nil, cfgErr("the Linear endpoint %q is not usable", endpoint)
	}
	return newClient(u, &http.Client{}, func(context.Context) (string, error) { return key, nil }), nil
}

// Me returns the Linear user the API key acts as.
func (a *Adapter) Me(ctx context.Context) (string, string, error) {
	me, err := a.viewer(ctx)
	if err != nil {
		return "", "", err
	}
	return me.ID, me.Email, nil
}

func (a *Adapter) viewer(ctx context.Context) (*user, error) {
	a.mu.Lock()
	me := a.me
	a.mu.Unlock()
	if me != nil {
		return me, nil
	}
	var out struct {
		Viewer user `json:"viewer"`
	}
	if err := a.c.do(ctx, `query { viewer { id email } }`, nil, &out); err != nil {
		return nil, err
	}
	if out.Viewer.ID == "" {
		return nil, terr(tracker.Response, "Linear did not say who the API key belongs to")
	}
	a.mu.Lock()
	a.me = &out.Viewer
	a.mu.Unlock()
	return &out.Viewer, nil
}

// teamStates resolves the team and its workflow states once.
func (a *Adapter) teamStates(ctx context.Context) (string, map[string]string, error) {
	a.mu.Lock()
	id, states := a.teamID, a.states
	a.mu.Unlock()
	if id != "" {
		return id, states, nil
	}
	var out struct {
		Teams struct {
			Nodes []struct {
				ID     string `json:"id"`
				Key    string `json:"key"`
				States struct {
					Nodes []state `json:"nodes"`
				} `json:"states"`
			} `json:"nodes"`
		} `json:"teams"`
	}
	q := `query($key: String!) { teams(filter: { key: { eq: $key } }) { nodes { id key states(first: 100) { nodes { id name type } } } } }`
	if err := a.c.do(ctx, q, map[string]any{"key": a.team}, &out); err != nil {
		return "", nil, err
	}
	if len(out.Teams.Nodes) != 1 {
		return "", nil, terr(tracker.Status, "the API key cannot see the Linear team %s", a.team)
	}
	t := out.Teams.Nodes[0]
	states = map[string]string{}
	backlog := ""
	for _, s := range t.States.Nodes {
		states[norm(s.Name)] = s.ID
		if s.Type == "backlog" && backlog == "" {
			backlog = s.ID
		}
	}
	a.mu.Lock()
	a.teamID, a.states, a.backlog = t.ID, states, backlog
	a.mu.Unlock()
	return t.ID, states, nil
}

// MissingStates returns the names among names that the team has no
// workflow state for, so that doctor can say which to add.
func (a *Adapter) MissingStates(ctx context.Context, names []string) ([]string, error) {
	_, states, err := a.teamStates(ctx)
	if err != nil {
		return nil, err
	}
	var missing []string
	seen := map[string]bool{}
	for _, n := range names {
		k := norm(n)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		if states[k] == "" {
			missing = append(missing, n)
		}
	}
	return missing, nil
}

// TeamMembers lists the e-mail addresses of the team's members, so that
// doctor can say who else could approve work.
func (a *Adapter) TeamMembers(ctx context.Context) ([]string, error) {
	id, _, err := a.teamStates(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	q := `query($id: String!, $after: String) { team(id: $id) { members(first: 50, after: $after) { nodes { id email } pageInfo { hasNextPage endCursor } } } }`
	err = a.c.pages(ctx, q, map[string]any{"id": id}, func(raw json.RawMessage) (pageInfo, error) {
		var d struct {
			Team struct {
				Members struct {
					Nodes    []user   `json:"nodes"`
					PageInfo pageInfo `json:"pageInfo"`
				} `json:"members"`
			} `json:"team"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			return pageInfo{}, terr(tracker.Response, "team members have an unexpected shape")
		}
		for _, u := range d.Team.Members.Nodes {
			out = append(out, u.Email)
		}
		return d.Team.Members.PageInfo, nil
	})
	return out, err
}

// ---- reads ----------------------------------------------------------------

// FetchIssuesByStates returns the team's issues in the given states.
func (a *Adapter) FetchIssuesByStates(ctx context.Context, states []string) ([]issue.Issue, error) {
	if len(states) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(states))
	for _, s := range states {
		names = append(names, strings.TrimSpace(s))
	}
	filter := map[string]any{
		"team":  map[string]any{"key": map[string]any{"eq": a.team}},
		"state": map[string]any{"name": map[string]any{"in": names}},
	}
	if a.project != "" {
		filter["project"] = map[string]any{"or": []any{
			map[string]any{"slugId": map[string]any{"eq": a.project}},
			map[string]any{"name": map[string]any{"eq": a.project}},
		}}
	}
	want := set(states)
	var raw []lnIssue
	q := `query($filter: IssueFilter, $after: String) { issues(filter: $filter, first: 50, after: $after) { nodes { ` +
		issueFields + ` } pageInfo { hasNextPage endCursor } } }`
	err := a.c.pages(ctx, q, map[string]any{"filter": filter}, func(data json.RawMessage) (pageInfo, error) {
		var d struct {
			Issues struct {
				Nodes    []json.RawMessage `json:"nodes"`
				PageInfo pageInfo          `json:"pageInfo"`
			} `json:"issues"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			return pageInfo{}, terr(tracker.Response, "issue list has an unexpected shape")
		}
		for _, n := range d.Issues.Nodes {
			var li lnIssue
			if err := json.Unmarshal(n, &li); err != nil {
				a.log.Warn("malformed issue record skipped")
				continue
			}
			raw = append(raw, li)
		}
		return d.Issues.PageInfo, nil
	})
	if err != nil {
		return nil, err
	}
	var out []issue.Issue
	for _, li := range raw {
		is, err := a.normalize(ctx, li)
		if err != nil {
			if isTransport(err) {
				return nil, err
			}
			a.log.Warn("malformed issue record skipped", "identifier", li.Identifier, "error", err)
			continue
		}
		// The filter is the API's; the answer is checked here too.
		if !want[norm(is.State)] {
			continue
		}
		out = append(out, is)
	}
	return out, nil
}

// FetchIssuesByIDs returns current snapshots. An issue that is gone, or
// was moved to another team, is omitted.
func (a *Adapter) FetchIssuesByIDs(ctx context.Context, ids []string) ([]issue.Issue, error) {
	var out []issue.Issue
	for _, id := range ids {
		li, err := a.read(ctx, id)
		if err == errNotFound {
			continue
		}
		if err != nil {
			return nil, err
		}
		if li.Team == nil || !strings.EqualFold(li.Team.Key, a.team) {
			continue
		}
		is, err := a.normalize(ctx, li)
		if err != nil {
			return nil, err
		}
		out = append(out, is)
	}
	return out, nil
}

func (a *Adapter) read(ctx context.Context, id string) (lnIssue, error) {
	var d struct {
		Issue *lnIssue `json:"issue"`
	}
	err := a.c.do(ctx, `query($id: String!) { issue(id: $id) { `+issueFields+` } }`, map[string]any{"id": id}, &d)
	if err != nil {
		// Linear answers an unknown id with an error, not with null.
		if te, ok := err.(*tracker.Error); ok && te.Category == tracker.Status && strings.Contains(strings.ToLower(te.Msg), "not found") {
			return lnIssue{}, errNotFound
		}
		return lnIssue{}, err
	}
	if d.Issue == nil {
		return lnIssue{}, errNotFound
	}
	return *d.Issue, nil
}

func isTransport(err error) bool {
	switch tracker.CategoryOf(err) {
	case tracker.Request, tracker.Status, tracker.RateLimited, tracker.MissingSecret, tracker.Pagination:
		return true
	}
	return false
}

// closedTypes are the workflow state types of finished work.
var closedTypes = map[string]bool{"completed": true, "canceled": true}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}

// normalize builds the issue model and decides whether it may be worked on.
func (a *Adapter) normalize(ctx context.Context, li lnIssue) (issue.Issue, error) {
	if li.ID == "" || !identRE.MatchString(li.Identifier) || strings.TrimSpace(li.Title) == "" ||
		li.State == nil || strings.TrimSpace(li.State.Name) == "" || li.Creator == nil || li.Creator.ID == "" {
		return issue.Issue{}, terr(tracker.Response, "issue record lacks a required field")
	}
	is := issue.Issue{
		ID: li.ID, Identifier: strings.ToUpper(li.Identifier), Title: li.Title, URL: li.URL,
		State: strings.TrimSpace(li.State.Name), BranchName: li.BranchName, AuthorID: li.Creator.ID,
		NativeRef: map[string]any{"linear_id": li.ID},
	}
	if li.Description != nil {
		is.Description = *li.Description
	}
	if li.Assignee != nil {
		is.AssigneeID = li.Assignee.ID
	}
	if p := int(li.Priority); p >= 1 && p <= 4 {
		is.Priority = &p
	}
	var err error
	if is.CreatedAt, err = parseTime(li.CreatedAt); err != nil {
		return issue.Issue{}, terr(tracker.Response, "issue record has a malformed timestamp")
	}
	if is.UpdatedAt, err = parseTime(li.UpdatedAt); err != nil {
		return issue.Issue{}, terr(tracker.Response, "issue record has a malformed timestamp")
	}
	for _, l := range li.Labels.Nodes {
		if n := norm(l.Name); n != "" {
			is.Labels = append(is.Labels, n)
		}
	}
	sort.Strings(is.Labels)
	is.ContentHash = issue.ContentHash(is.Title, is.Description)

	me, err := a.viewer(ctx)
	if err != nil {
		return issue.Issue{}, err
	}
	byMe := li.Creator.ID == me.ID
	if closedTypes[li.State.Type] || !(a.trusted(*li.Creator) || byMe) {
		return is, nil // Dispatchable stays false
	}
	approved, blockers, err := a.derived(ctx, li, is, byMe, me.ID)
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

func (a *Adapter) trusted(u user) bool {
	return a.authors[norm(u.ID)] || (u.Email != "" && a.authors[norm(u.Email)])
}

func (a *Adapter) derived(ctx context.Context, li lnIssue, is issue.Issue, byMe bool, me string) (bool, []issue.Blocker, error) {
	a.mu.Lock()
	c := a.cache[li.ID]
	a.mu.Unlock()
	if c == nil || c.updatedAt != li.UpdatedAt || li.UpdatedAt == "" {
		ok, err := a.provenance(ctx, li, is, byMe, me)
		if err != nil {
			return false, nil, err
		}
		c = &cached{updatedAt: li.UpdatedAt, provenance: ok}
	}
	if norm(is.State) == a.firstActive && (c.blockersAt.IsZero() || a.now().Sub(c.blockersAt) > blockerTTL) {
		b, err := a.blockers(ctx, li.ID)
		if err != nil {
			return false, nil, err
		}
		c.blockers, c.blockersAt = b, a.now()
	}
	a.mu.Lock()
	a.cache[li.ID] = c
	a.mu.Unlock()
	return c.provenance, c.blockers, nil
}

type historyEntry struct {
	CreatedAt string `json:"createdAt"`
	Actor     *user  `json:"actor"`
	BotActor  *struct {
		ID string `json:"id"`
	} `json:"botActor"`
	ToState     *state `json:"toState"`
	AddedLabels []struct {
		Name string `json:"name"`
	} `json:"addedLabels"`
}

// provenance checks who put the issue in its state and who added each
// required label: the latest such change must come from an allowlisted
// user. The service's own user counts for the states it writes itself, and
// for an issue it created (a task from an approved plan). A change Linear
// records without a user, such as one made by an integration, never
// counts. An issue still in the state it was created in has no state
// change: its creator put it there.
func (a *Adapter) provenance(ctx context.Context, li lnIssue, is issue.Issue, byMe bool, me string) (bool, error) {
	var entries []historyEntry
	q := `query($id: String!, $after: String) { issue(id: $id) { history(first: 50, after: $after) { nodes {
createdAt actor { id email } botActor { id } toState { id name type } addedLabels { name } }
pageInfo { hasNextPage endCursor } } } }`
	err := a.c.pages(ctx, q, map[string]any{"id": li.ID}, func(data json.RawMessage) (pageInfo, error) {
		var d struct {
			Issue struct {
				History struct {
					Nodes    []historyEntry `json:"nodes"`
					PageInfo pageInfo       `json:"pageInfo"`
				} `json:"history"`
			} `json:"issue"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			return pageInfo{}, terr(tracker.Response, "issue history has an unexpected shape")
		}
		entries = append(entries, d.Issue.History.Nodes...)
		return d.Issue.History.PageInfo, nil
	})
	if err != nil {
		return false, err
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].CreatedAt < entries[j].CreatedAt })

	ok := func(e historyEntry, owned bool) bool {
		if e.BotActor != nil || e.Actor == nil || e.Actor.ID == "" {
			return false
		}
		if a.trusted(*e.Actor) {
			return true
		}
		return e.Actor.ID == me && (owned || byMe)
	}
	// The state.
	var stateBy *historyEntry
	for i := range entries {
		if e := entries[i]; e.ToState != nil && norm(e.ToState.Name) == norm(is.State) {
			stateBy = &entries[i]
		}
	}
	owned := a.ownedStates[norm(is.State)] != ""
	if stateBy != nil {
		if !ok(*stateBy, owned) {
			return false, nil
		}
	} else {
		// Never moved into this state: the creator chose it.
		creator := li.Creator
		if !(a.trusted(*creator) || (byMe && (owned || creator.ID == me))) {
			return false, nil
		}
	}
	// The required labels. A label set when the issue was made shows as
	// added in its first history entry, or not at all; without any record
	// the creator added it.
	if !is.HasLabels(a.required) {
		return false, nil
	}
	for _, want := range a.required {
		var by *historyEntry
		for i := range entries {
			for _, l := range entries[i].AddedLabels {
				if norm(l.Name) == norm(want) {
					by = &entries[i]
				}
			}
		}
		if by == nil {
			if !(a.trusted(*li.Creator) || byMe) {
				return false, nil
			}
			continue
		}
		if !ok(*by, false) {
			return false, nil
		}
	}
	return true, nil
}

func (a *Adapter) blockers(ctx context.Context, id string) ([]issue.Blocker, error) {
	var out []issue.Blocker
	q := `query($id: String!, $after: String) { issue(id: $id) { inverseRelations(first: 50, after: $after) { nodes {
type issue { id identifier state { type } } } pageInfo { hasNextPage endCursor } } } }`
	err := a.c.pages(ctx, q, map[string]any{"id": id}, func(data json.RawMessage) (pageInfo, error) {
		var d struct {
			Issue struct {
				Inverse struct {
					Nodes []struct {
						Type  string `json:"type"`
						Issue struct {
							ID         string `json:"id"`
							Identifier string `json:"identifier"`
							State      *state `json:"state"`
						} `json:"issue"`
					} `json:"nodes"`
					PageInfo pageInfo `json:"pageInfo"`
				} `json:"inverseRelations"`
			} `json:"issue"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			return pageInfo{}, terr(tracker.Response, "issue relations have an unexpected shape")
		}
		for _, r := range d.Issue.Inverse.Nodes {
			if r.Type != "blocks" {
				continue
			}
			st := "open"
			if r.Issue.State != nil && closedTypes[r.Issue.State.Type] {
				st = "closed"
			}
			out = append(out, issue.Blocker{ID: r.Issue.ID, Identifier: r.Issue.Identifier, State: st})
		}
		return d.Issue.Inverse.PageInfo, nil
	})
	return out, err
}

// ---- writes ---------------------------------------------------------------

// SetState moves the issue to one of the states the service owns.
func (a *Adapter) SetState(ctx context.Context, is issue.Issue, st string) error {
	if a.ownedStates[norm(st)] == "" {
		return terr(tracker.WriteRejected, "the service does not write the state %q", st)
	}
	li, err := a.read(ctx, is.ID)
	if err == errNotFound {
		return terr(tracker.WriteRejected, "issue %s no longer exists", is.Identifier)
	}
	if err != nil {
		return err
	}
	if li.Team == nil || !strings.EqualFold(li.Team.Key, a.team) || !strings.EqualFold(li.Identifier, is.Identifier) {
		return terr(tracker.WriteRejected, "issue %s is not the issue that was dispatched", is.Identifier)
	}
	if li.State != nil && norm(li.State.Name) == norm(st) {
		return nil
	}
	_, states, err := a.teamStates(ctx)
	if err != nil {
		return err
	}
	sid := states[norm(st)]
	if sid == "" {
		return terr(tracker.WriteRejected, "the Linear team %s has no state %q", a.team, st)
	}
	var out struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	q := `mutation($id: String!, $state: String!) { issueUpdate(id: $id, input: { stateId: $state }) { success } }`
	if err := a.c.do(ctx, q, map[string]any{"id": is.ID, "state": sid}, &out); err != nil {
		return err
	}
	if !out.IssueUpdate.Success {
		return terr(tracker.WriteRejected, "Linear did not move %s", is.Identifier)
	}
	a.mu.Lock()
	delete(a.cache, is.ID)
	a.mu.Unlock()
	return nil
}

// marker starts the status comment. Linear shows it; it names the worker
// so two machines keep a comment each.
func (a *Adapter) marker() string { return "**Maestro** (worker `" + a.worker + "`)" }

type comment struct {
	ID        string `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
	User      *user  `json:"user"`
	BotActor  *struct {
		ID string `json:"id"`
	} `json:"botActor"`
}

func (a *Adapter) comments(ctx context.Context, id string, each func(comment)) error {
	q := `query($id: String!, $after: String) { issue(id: $id) { comments(first: 50, after: $after) { nodes {
id body createdAt user { id email } botActor { id } } pageInfo { hasNextPage endCursor } } } }`
	return a.c.pages(ctx, q, map[string]any{"id": id}, func(data json.RawMessage) (pageInfo, error) {
		var d struct {
			Issue struct {
				Comments struct {
					Nodes    []comment `json:"nodes"`
					PageInfo pageInfo  `json:"pageInfo"`
				} `json:"comments"`
			} `json:"issue"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			return pageInfo{}, terr(tracker.Response, "comments have an unexpected shape")
		}
		for _, c := range d.Issue.Comments.Nodes {
			each(c)
		}
		return d.Issue.Comments.PageInfo, nil
	})
}

// UpsertStatusComment keeps one status comment per issue for this worker.
// Only a comment the service's own user wrote is ever edited.
func (a *Adapter) UpsertStatusComment(ctx context.Context, is issue.Issue, body string) error {
	me, err := a.viewer(ctx)
	if err != nil {
		return err
	}
	existing := ""
	err = a.comments(ctx, is.ID, func(c comment) {
		if existing == "" && c.User != nil && c.User.ID == me.ID && c.BotActor == nil && strings.HasPrefix(c.Body, a.marker()) {
			existing = c.ID
		}
	})
	if err != nil {
		return err
	}
	text := a.marker() + "\n\n" + body
	var out map[string]struct {
		Success bool `json:"success"`
	}
	if existing != "" {
		q := `mutation($id: String!, $body: String!) { commentUpdate(id: $id, input: { body: $body }) { success } }`
		err = a.c.do(ctx, q, map[string]any{"id": existing, "body": text}, &out)
	} else {
		q := `mutation($issue: String!, $body: String!) { commentCreate(input: { issueId: $issue, body: $body }) { success } }`
		err = a.c.do(ctx, q, map[string]any{"issue": is.ID, "body": text}, &out)
	}
	if err != nil {
		return err
	}
	for _, r := range out {
		if !r.Success {
			return terr(tracker.WriteRejected, "Linear did not save the status comment on %s", is.Identifier)
		}
	}
	return nil
}

// FetchFeedback returns comments made after since by allowlisted
// commenters. Comments by anyone else, by integrations and by the
// service itself are never returned.
func (a *Adapter) FetchFeedback(ctx context.Context, is issue.Issue, since time.Time) ([]tracker.Feedback, error) {
	var out []tracker.Feedback
	err := a.comments(ctx, is.ID, func(c comment) {
		if c.User == nil || c.BotActor != nil {
			return
		}
		if !(a.commenters[norm(c.User.ID)] || (c.User.Email != "" && a.commenters[norm(c.User.Email)])) {
			return
		}
		at, err := time.Parse(time.RFC3339, c.CreatedAt)
		if err != nil || !at.After(since) {
			return
		}
		who := c.User.Email
		if who == "" {
			who = c.User.ID
		}
		out = append(out, tracker.Feedback{Author: who, Body: c.Body, CreatedAt: at})
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, err
}
