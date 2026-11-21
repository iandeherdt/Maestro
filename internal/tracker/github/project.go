package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iandeherdt/Symphony-beads/internal/issue"
	"github.com/iandeherdt/Symphony-beads/internal/tracker"
)

// This file implements state_source "project": the state of an issue is
// the column its card stands in on a board (a GitHub Project), and the
// card's Worker field says which machine may take it.
//
// GitHub does not record who moved a card (SPEC 12.5), so the board itself
// is the control: it has to be one that only allowlisted accounts can
// change. The author allowlist still applies to every issue.

const (
	defaultStatusField = "Status"
	defaultWorkerField = "Worker"
	boardTTL           = 5 * time.Minute
	maxMessage         = 200
)

// board is the configured Project and what is known about its fields.
type board struct {
	owner       string
	number      int
	statusField string
	workerField string

	mu     sync.Mutex
	meta   *boardMeta
	metaAt time.Time
	warned bool
}

// boardMeta are the IDs a card is moved with.
type boardMeta struct {
	id      string
	fieldID string
	options map[string]string // normalized column name -> option ID
	// The Worker field. workerID is empty when the board has none.
	workerID      string
	workerOptions map[string]string // normalized worker name -> option ID
	workers       []string          // worker names as written on the board
}

// card is where an issue stands on the board.
type card struct {
	item   string
	status string
	worker string
}

var fieldRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _-]{0,49}$`)

func newBoard(raw any, repoOwner string) (*board, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, cfgErr("tracker.provider.project is required with state_source: project, with at least the board's number")
	}
	b := &board{owner: repoOwner, statusField: defaultStatusField, workerField: defaultWorkerField}
	for k, v := range m {
		switch k {
		case "owner", "status_field", "worker_field":
			s, ok := v.(string)
			if !ok {
				return nil, cfgErr("tracker.provider.project.%s must be a string", k)
			}
			switch k {
			case "owner":
				b.owner = s
			case "status_field":
				b.statusField = s
			default:
				b.workerField = s
			}
		case "number":
			switch n := v.(type) {
			case int:
				b.number = n
			case int64:
				b.number = int(n)
			case uint64:
				b.number = int(n)
			case float64:
				if n == float64(int(n)) {
					b.number = int(n)
				}
			}
		default:
			return nil, cfgErr("tracker.provider.project.%s is not a known key", k)
		}
	}
	if b.number <= 0 {
		return nil, cfgErr("tracker.provider.project.number must be the board's number, the one in its address")
	}
	if !loginRE.MatchString(b.owner) || strings.HasSuffix(b.owner, "]") {
		return nil, cfgErr("tracker.provider.project.owner must be the account that owns the board")
	}
	if !fieldRE.MatchString(b.statusField) || !fieldRE.MatchString(b.workerField) {
		return nil, cfgErr("tracker.provider.project: status_field and worker_field must be plain field names")
	}
	if norm(b.statusField) == norm(b.workerField) {
		return nil, cfgErr("tracker.provider.project: status_field and worker_field must be two different fields")
	}
	return b, nil
}

// ---- queries --------------------------------------------------------------

const issueFields = `
fragment issueFields on Issue {
  id number title body state url createdAt updatedAt
  author { __typename login }
  assignees(first: 1) { nodes { login } }
  labels(first: 100) { nodes { name } }
  repository { nameWithOwner }
}`

const cardFields = `
fragment cardFields on ProjectV2Item {
  id isArchived
  status: fieldValueByName(name: $status) { ... on ProjectV2ItemFieldSingleSelectValue { name } }
  worker: fieldValueByName(name: $worker) { ... on ProjectV2ItemFieldSingleSelectValue { name } }
}`

// The fields are listed and picked by name: asking GitHub for a field by a
// name the board does not have fails the whole request.
const boardQuery = `
query($owner: String!, $number: Int!) {
  repositoryOwner(login: $owner) {
    ... on ProjectV2Owner {
      projectV2(number: $number) {
        id closed
        fields(first: 100) {
          nodes {
            __typename
            ... on ProjectV2FieldCommon { id name }
            ... on ProjectV2SingleSelectField { options { id name } }
          }
        }
        workflows(first: 30) { nodes { name enabled } }
      }
    }
  }
}`

const cardsQuery = `
query($owner: String!, $number: Int!, $status: String!, $worker: String!, $after: String) {
  repositoryOwner(login: $owner) {
    ... on ProjectV2Owner {
      projectV2(number: $number) {
        items(first: 100, after: $after) {
          pageInfo { hasNextPage endCursor }
          nodes {
            ...cardFields
            content { __typename ... on Issue { ...issueFields } }
          }
        }
      }
    }
  }
}` + cardFields + issueFields

const issueQuery = `
query($owner: String!, $repo: String!, $n: Int!, $status: String!, $worker: String!) {
  repository(owner: $owner, name: $repo) {
    issue(number: $n) {
      ...issueFields
      projectItems(first: 50) {
        nodes {
          ...cardFields
          project { number owner { __typename ... on Organization { login } ... on User { login } } }
        }
      }
    }
  }
}` + cardFields + issueFields

const moveMutation = `
mutation($project: ID!, $item: ID!, $field: ID!, $option: String!) {
  updateProjectV2ItemFieldValue(input: {projectId: $project, itemId: $item, fieldId: $field, value: {singleSelectOptionId: $option}}) {
    projectV2Item { id }
  }
}`

type gqlLogin struct {
	Typename string `json:"__typename"`
	Login    string `json:"login"`
}

// restLogin is the login as the REST API writes it. GraphQL names an App's
// account without the "[bot]" that REST, and so the configuration, use.
// Without this the tasks the service creates from an approved plan would
// look like the work of a stranger and never be picked up.
func (l gqlLogin) restLogin() string {
	if l.Typename == "Bot" && l.Login != "" && !strings.HasSuffix(l.Login, "[bot]") {
		return l.Login + "[bot]"
	}
	return l.Login
}

type gqlIssue struct {
	ID        string    `json:"id"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	URL       string    `json:"url"`
	CreatedAt string    `json:"createdAt"`
	UpdatedAt string    `json:"updatedAt"`
	Author    *gqlLogin `json:"author"`
	Assignees struct {
		Nodes []gqlLogin `json:"nodes"`
	} `json:"assignees"`
	Labels struct {
		Nodes []ghLabel `json:"nodes"`
	} `json:"labels"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
}

type gqlField struct {
	Typename string `json:"__typename"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	Options  []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"options"`
}

type gqlName struct {
	Name string `json:"name"`
}

type gqlCard struct {
	ID         string   `json:"id"`
	IsArchived bool     `json:"isArchived"`
	Status     *gqlName `json:"status"`
	Worker     *gqlName `json:"worker"`
	Content    *struct {
		Typename string `json:"__typename"`
		gqlIssue
	} `json:"content"`
	Project *struct {
		Number int       `json:"number"`
		Owner  *gqlLogin `json:"owner"`
	} `json:"project"`
}

func (c gqlCard) card() *card {
	out := &card{item: c.ID}
	if c.Status != nil {
		out.status = c.Status.Name
	}
	if c.Worker != nil {
		out.worker = c.Worker.Name
	}
	return out
}

// rest converts the GraphQL record into the REST shape normalize reads.
func (g gqlIssue) rest() ghIssue {
	body := g.Body
	gi := ghIssue{NodeID: g.ID, Number: g.Number, Title: g.Title, Body: &body, State: strings.ToLower(g.State),
		HTMLURL: g.URL, CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt, Labels: g.Labels.Nodes}
	if g.Author != nil {
		gi.User = &ghUser{Login: g.Author.restLogin()}
	}
	if len(g.Assignees.Nodes) > 0 {
		gi.Assignee = &ghUser{Login: g.Assignees.Nodes[0].Login}
	}
	return gi
}

func (a *Adapter) vars(extra map[string]any) map[string]any {
	v := map[string]any{"status": a.board.statusField, "worker": a.board.workerField}
	for k, x := range extra {
		v[k] = x
	}
	return v
}

func (a *Adapter) boardName() string {
	return "the board " + a.board.owner + " number " + strconv.Itoa(a.board.number)
}

// meta returns the board's IDs, read at most every boardTTL. fresh forces
// a read, for a column that was added a moment ago.
func (a *Adapter) meta(ctx context.Context, fresh bool) (*boardMeta, error) {
	b := a.board
	b.mu.Lock()
	defer b.mu.Unlock()
	if !fresh && b.meta != nil && a.now().Sub(b.metaAt) < boardTTL {
		return b.meta, nil
	}
	var out struct {
		RepositoryOwner *struct {
			ProjectV2 *struct {
				ID     string `json:"id"`
				Closed bool   `json:"closed"`
				Fields struct {
					Nodes []gqlField `json:"nodes"`
				} `json:"fields"`
				Field       *gqlField `json:"-"`
				WorkerField *gqlField `json:"-"`
				Workflows   struct {
					Nodes []struct {
						Name    string `json:"name"`
						Enabled bool   `json:"enabled"`
					} `json:"nodes"`
				} `json:"workflows"`
			} `json:"projectV2"`
		} `json:"repositoryOwner"`
	}
	err := a.c.graphql(ctx, boardQuery, a.vars(map[string]any{"owner": b.owner, "number": b.number}), &out, false)
	if errors.Is(err, errNotFound) || err == nil && (out.RepositoryOwner == nil || out.RepositoryOwner.ProjectV2 == nil) {
		return nil, terr(tracker.Status, "%s was not found, or the service's App may not read it. The App needs the organization permission Projects, read and write", a.boardName())
	}
	if err != nil {
		return nil, err
	}
	p := out.RepositoryOwner.ProjectV2
	for i := range p.Fields.Nodes {
		f := &p.Fields.Nodes[i]
		switch name := strings.TrimSpace(f.Name); {
		case p.Field == nil && strings.EqualFold(name, b.statusField):
			p.Field = f
		case p.WorkerField == nil && strings.EqualFold(name, b.workerField):
			p.WorkerField = f
		}
	}
	if p.Closed {
		return nil, terr(tracker.Status, "%s is closed", a.boardName())
	}
	if p.ID == "" || p.Field == nil || p.Field.ID == "" || p.Field.Typename != "ProjectV2SingleSelectField" {
		return nil, terr(tracker.Status, "%s has no single-select field named %q", a.boardName(), b.statusField)
	}
	m := &boardMeta{id: p.ID, fieldID: p.Field.ID, options: map[string]string{}, workerOptions: map[string]string{}}
	for _, o := range p.Field.Options {
		m.options[norm(o.Name)] = o.ID
	}
	if w := p.WorkerField; w != nil && w.ID != "" && w.Typename == "ProjectV2SingleSelectField" {
		m.workerID = w.ID
		for _, o := range w.Options {
			if name := strings.TrimSpace(o.Name); name != "" && m.workerOptions[norm(name)] == "" {
				m.workerOptions[norm(name)] = o.ID
				m.workers = append(m.workers, name)
			}
		}
	}
	if !b.warned {
		b.warned = true
		for _, w := range p.Workflows.Nodes {
			if n := norm(w.Name); w.Enabled && (n == "item added to project" || n == "auto-add to project") {
				a.log.Warn("the board has an automation switched on that places new cards by itself. "+
					"If it puts them in the approval column, tickets are approved without you moving them",
					"workflow", w.Name)
			}
		}
	}
	b.meta, b.metaAt = m, a.now()
	return m, nil
}

// mine reports whether an issue record belongs to the configured repository.
func (a *Adapter) mine(g gqlIssue) bool {
	return norm(g.Repository.NameWithOwner) == norm(a.owner+"/"+a.repo)
}

// boardIssuesByStates returns the issues of the configured repository
// whose card stands in one of the given columns. Closed issues on the
// board are reported in the terminal state.
func (a *Adapter) boardIssuesByStates(ctx context.Context, states []string) ([]issue.Issue, error) {
	if len(states) == 0 {
		return nil, nil
	}
	want := map[string]bool{}
	for _, s := range states {
		want[norm(s)] = true
	}
	// Reading the fields first turns a missing board or column field into
	// one clear error instead of an empty board.
	if _, err := a.meta(ctx, false); err != nil {
		return nil, err
	}
	var out []issue.Issue
	seen := map[int]bool{}
	after := any(nil)
	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, terr(tracker.Pagination, "the board has more than %d pages of cards", maxPages)
		}
		var res struct {
			RepositoryOwner *struct {
				ProjectV2 *struct {
					Items struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []*gqlCard `json:"nodes"`
					} `json:"items"`
				} `json:"projectV2"`
			} `json:"repositoryOwner"`
		}
		err := a.c.graphql(ctx, cardsQuery, a.vars(map[string]any{"owner": a.board.owner, "number": a.board.number, "after": after}), &res, false)
		if errors.Is(err, errNotFound) || err == nil && (res.RepositoryOwner == nil || res.RepositoryOwner.ProjectV2 == nil) {
			return nil, terr(tracker.Status, "%s was not found, or the service's App may not read it", a.boardName())
		}
		if err != nil {
			return nil, err
		}
		items := res.RepositoryOwner.ProjectV2.Items
		for _, c := range items.Nodes {
			// A card without readable content is a draft, a pull request,
			// or an issue in a repository the token cannot read.
			if c == nil || c.IsArchived || c.Content == nil || c.Content.Typename != "Issue" || !a.mine(c.Content.gqlIssue) {
				continue
			}
			gi, on := c.Content.rest(), c.card()
			if seen[gi.Number] {
				continue
			}
			if a.wroteRecently(gi.Number) {
				// The same caution as for the issue list: a card the
				// service has just moved is read on its own.
				fresh, freshOn, err := a.boardIssue(ctx, gi.Number)
				if errors.Is(err, errNotFound) {
					continue
				}
				if err != nil {
					return nil, err
				}
				if fresh.NodeID != gi.NodeID {
					continue
				}
				gi, on = fresh, freshOn
			}
			is, err := a.normalize(ctx, gi, on)
			if err != nil {
				if isTransport(err) {
					return nil, err
				}
				a.log.Warn("malformed issue record skipped", "number", gi.Number, "error", err)
				continue
			}
			if !want[norm(is.State)] {
				continue
			}
			seen[gi.Number] = true
			out = append(out, is)
		}
		if !items.PageInfo.HasNextPage || items.PageInfo.EndCursor == "" {
			return out, nil
		}
		after = items.PageInfo.EndCursor
	}
}

// boardIssue reads one issue and its card. The card is nil when the issue
// is not on the board or its card is archived.
func (a *Adapter) boardIssue(ctx context.Context, n int) (ghIssue, *card, error) {
	var res struct {
		Repository *struct {
			Issue *struct {
				gqlIssue
				ProjectItems struct {
					Nodes []*gqlCard `json:"nodes"`
				} `json:"projectItems"`
			} `json:"issue"`
		} `json:"repository"`
	}
	err := a.c.graphql(ctx, issueQuery, a.vars(map[string]any{"owner": a.owner, "repo": a.repo, "n": n}), &res, false)
	if err != nil {
		return ghIssue{}, nil, err
	}
	if res.Repository == nil || res.Repository.Issue == nil {
		return ghIssue{}, nil, errNotFound
	}
	is := res.Repository.Issue
	var on *card
	for _, c := range is.ProjectItems.Nodes {
		if c == nil || c.IsArchived || c.Project == nil || c.Project.Owner == nil {
			continue
		}
		if c.Project.Number == a.board.number && norm(c.Project.Owner.Login) == norm(a.board.owner) {
			on = c.card()
			break
		}
	}
	return is.rest(), on, nil
}

// moveCard puts the issue's card in the column of a state.
func (a *Adapter) moveCard(ctx context.Context, is issue.Issue, on *card, state string) error {
	if on == nil {
		return terr(tracker.WriteRejected, "issue %s has no card on %s", is.Identifier, a.boardName())
	}
	n, _ := numberOf(is)
	defer func() {
		a.mu.Lock()
		delete(a.cache, n)
		a.mu.Unlock()
	}()
	if norm(on.status) == norm(state) {
		return nil
	}
	m, err := a.meta(ctx, false)
	if err != nil {
		return err
	}
	option := m.options[norm(state)]
	if option == "" {
		if m, err = a.meta(ctx, true); err != nil {
			return err
		}
		if option = m.options[norm(state)]; option == "" {
			return terr(tracker.WriteRejected, "%s has no column %q. Add it to the field %q, or run `symphony setup` again",
				a.boardName(), state, a.board.statusField)
		}
	}
	return a.c.graphql(ctx, moveMutation, map[string]any{"project": m.id, "item": on.item, "field": m.fieldID, "option": option}, nil, true)
}

// ---- transport ------------------------------------------------------------

type gqlError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func plainMessage(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= maxMessage {
			b.WriteString("…")
			break
		}
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		b.WriteRune(r)
	}
	return b.String()
}

// graphql performs one GraphQL request. GitHub answers 200 even when the
// query failed, so the errors in the body are mapped to the same
// categories as the REST statuses.
func (c *client) graphql(ctx context.Context, query string, vars map[string]any, out any, write bool) error {
	var res struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors"`
	}
	h, err := c.do(ctx, http.MethodPost, c.url("/graphql", nil), map[string]any{"query": query, "variables": vars}, &res, write)
	if err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		e := res.Errors[0]
		for _, x := range res.Errors {
			if x.Type == "RATE_LIMITED" {
				return &RateLimitError{RetryAfter: retryAfter(h)}
			}
		}
		switch {
		case e.Type == "NOT_FOUND":
			return errNotFound
		case write:
			return terr(tracker.WriteRejected, "GitHub refused the change: %s", plainMessage(e.Message))
		case e.Type == "FORBIDDEN" || e.Type == "INSUFFICIENT_SCOPES":
			return terr(tracker.Status, "GitHub refused the request: %s", plainMessage(e.Message))
		default:
			return terr(tracker.Response, "GitHub could not answer the request: %s", plainMessage(e.Message))
		}
	}
	if out == nil {
		return nil
	}
	if len(res.Data) == 0 || string(res.Data) == "null" {
		return terr(tracker.Response, "GitHub answered without data")
	}
	if err := json.Unmarshal(res.Data, out); err != nil {
		return terr(tracker.Response, "malformed GraphQL response")
	}
	return nil
}
