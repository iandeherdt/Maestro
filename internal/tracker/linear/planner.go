package linear

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/tracker"
)

// The planning stage (SPEC 22) on Linear. An approved plan becomes
// sub-issues of the idea, in the idea's project, ordered with "blocks"
// relations. A task is created in the team's backlog without the worker
// label, so it cannot start before it is placed.

var _ tracker.Planner = (*Adapter)(nil)

const routePrefix = "run:"

type labelNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Team *struct {
		ID string `json:"id"`
	} `json:"team"`
}

// labels returns the labels this team can use whose name starts with
// prefix, or is name when exact is set.
func (a *Adapter) labels(ctx context.Context, name string, exact bool) ([]labelNode, error) {
	teamID, _, err := a.teamStates(ctx)
	if err != nil {
		return nil, err
	}
	cmp := "startsWithIgnoreCase"
	if exact {
		cmp = "eqIgnoreCase"
	}
	q := `query($name: String!, $after: String) { issueLabels(first: 50, after: $after, filter: { name: { ` + cmp + `: $name } }) {
nodes { id name team { id } } pageInfo { hasNextPage endCursor } } }`
	var out []labelNode
	err = a.c.pages(ctx, q, map[string]any{"name": name}, func(data json.RawMessage) (pageInfo, error) {
		var d struct {
			IssueLabels struct {
				Nodes    []labelNode `json:"nodes"`
				PageInfo pageInfo    `json:"pageInfo"`
			} `json:"issueLabels"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			return pageInfo{}, terr(tracker.Response, "labels have an unexpected shape")
		}
		for _, l := range d.IssueLabels.Nodes {
			// A workspace label, or one of this team.
			if l.Team == nil || l.Team.ID == teamID {
				out = append(out, l)
			}
		}
		return d.IssueLabels.PageInfo, nil
	})
	return out, err
}

// Workers lists the machines: the labels run:<name> the team can use.
func (a *Adapter) Workers(ctx context.Context) ([]string, error) {
	ls, err := a.labels(ctx, routePrefix, false)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, l := range ls {
		w := norm(strings.TrimPrefix(norm(l.Name), routePrefix))
		if w != "" && !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, terr(tracker.Status, "the Linear team %s has no label %s<machine>", a.team, routePrefix)
	}
	return out, nil
}

// PostComment adds a new comment to the issue.
func (a *Adapter) PostComment(ctx context.Context, is issue.Issue, body string) error {
	var out struct {
		CommentCreate struct {
			Success bool `json:"success"`
		} `json:"commentCreate"`
	}
	q := `mutation($issue: String!, $body: String!) { commentCreate(input: { issueId: $issue, body: $body }) { success } }`
	if err := a.c.do(ctx, q, map[string]any{"issue": is.ID, "body": body}, &out); err != nil {
		return err
	}
	if !out.CommentCreate.Success {
		return terr(tracker.WriteRejected, "Linear did not save the comment on %s", is.Identifier)
	}
	return nil
}

// CreateTask creates an issue in the team's backlog, without labels.
func (a *Adapter) CreateTask(ctx context.Context, t tracker.Task) (tracker.TaskRef, error) {
	if strings.TrimSpace(t.Title) == "" {
		return tracker.TaskRef{}, terr(tracker.WriteRejected, "a task needs a title")
	}
	teamID, _, err := a.teamStates(ctx)
	if err != nil {
		return tracker.TaskRef{}, err
	}
	a.mu.Lock()
	backlog := a.backlog
	a.mu.Unlock()
	input := map[string]any{"teamId": teamID, "title": t.Title, "description": t.Body}
	if backlog != "" {
		input["stateId"] = backlog
	}
	var out struct {
		IssueCreate struct {
			Success bool `json:"success"`
			Issue   *struct {
				ID         string `json:"id"`
				Identifier string `json:"identifier"`
			} `json:"issue"`
		} `json:"issueCreate"`
	}
	q := `mutation($input: IssueCreateInput!) { issueCreate(input: $input) { success issue { id identifier } } }`
	if err := a.c.do(ctx, q, map[string]any{"input": input}, &out); err != nil {
		return tracker.TaskRef{}, err
	}
	is := out.IssueCreate.Issue
	if !out.IssueCreate.Success || is == nil || is.ID == "" || !identRE.MatchString(is.Identifier) {
		return tracker.TaskRef{}, terr(tracker.Response, "the new issue came back without its identifiers")
	}
	return tracker.TaskRef{ID: is.ID, Identifier: is.Identifier}, nil
}

// routeLabel returns the id of the label run:<worker>, creating it in the
// team when it does not exist.
func (a *Adapter) routeLabel(ctx context.Context, worker string) (string, error) {
	name := routePrefix + norm(worker)
	ls, err := a.labels(ctx, name, true)
	if err != nil {
		return "", err
	}
	for _, l := range ls {
		if l.ID != "" {
			return l.ID, nil
		}
	}
	teamID, _, err := a.teamStates(ctx)
	if err != nil {
		return "", err
	}
	var out struct {
		IssueLabelCreate struct {
			Success    bool `json:"success"`
			IssueLabel *struct {
				ID string `json:"id"`
			} `json:"issueLabel"`
		} `json:"issueLabelCreate"`
	}
	q := `mutation($input: IssueLabelCreateInput!) { issueLabelCreate(input: $input) { success issueLabel { id } } }`
	if err := a.c.do(ctx, q, map[string]any{"input": map[string]any{"name": name, "teamId": teamID, "color": "#4EA7FC"}}, &out); err != nil {
		return "", err
	}
	if l := out.IssueLabelCreate.IssueLabel; out.IssueLabelCreate.Success && l != nil && l.ID != "" {
		return l.ID, nil
	}
	return "", terr(tracker.WriteRejected, "Linear did not create the label %s", name)
}

// PlaceTask makes the task a sub-issue of the idea, in the idea's
// project, adds the worker's label and moves it to state.
func (a *Adapter) PlaceTask(ctx context.Context, parent issue.Issue, ref tracker.TaskRef, worker, state string) error {
	if ref.ID == "" {
		return terr(tracker.WriteRejected, "the task has no identifiers")
	}
	idea, err := a.read(ctx, parent.ID)
	if err == errNotFound {
		return terr(tracker.WriteRejected, "issue %s no longer exists", parent.Identifier)
	}
	if err != nil {
		return err
	}
	if idea.Team == nil || !strings.EqualFold(idea.Team.Key, a.team) {
		return terr(tracker.WriteRejected, "issue %s is not on the team %s", parent.Identifier, a.team)
	}
	_, states, err := a.teamStates(ctx)
	if err != nil {
		return err
	}
	sid := states[norm(state)]
	if sid == "" {
		return terr(tracker.WriteRejected, "the Linear team %s has no state %q", a.team, state)
	}
	label, err := a.routeLabel(ctx, worker)
	if err != nil {
		return err
	}
	input := map[string]any{"parentId": idea.ID, "stateId": sid, "addedLabelIds": []string{label}}
	if idea.Project != nil && idea.Project.ID != "" {
		input["projectId"] = idea.Project.ID
	}
	var out struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	q := `mutation($id: String!, $input: IssueUpdateInput!) { issueUpdate(id: $id, input: $input) { success } }`
	if err := a.c.do(ctx, q, map[string]any{"id": ref.ID, "input": input}, &out); err != nil {
		return err
	}
	if !out.IssueUpdate.Success {
		return terr(tracker.WriteRejected, "Linear did not place %s", ref.Identifier)
	}
	a.mu.Lock()
	delete(a.cache, ref.ID)
	a.mu.Unlock()
	return nil
}

// AddBlocker records that task waits for blocker. A relation that exists
// is not made again.
func (a *Adapter) AddBlocker(ctx context.Context, task, blocker tracker.TaskRef) error {
	if task.ID == "" || blocker.ID == "" {
		return terr(tracker.WriteRejected, "the task has no identifiers")
	}
	have, err := a.blockers(ctx, task.ID)
	if err != nil {
		return err
	}
	for _, b := range have {
		if b.ID == blocker.ID {
			return nil
		}
	}
	var out struct {
		IssueRelationCreate struct {
			Success bool `json:"success"`
		} `json:"issueRelationCreate"`
	}
	q := `mutation($input: IssueRelationCreateInput!) { issueRelationCreate(input: $input) { success } }`
	in := map[string]any{"issueId": blocker.ID, "relatedIssueId": task.ID, "type": "blocks"}
	if err := a.c.do(ctx, q, map[string]any{"input": in}, &out); err != nil {
		return err
	}
	if !out.IssueRelationCreate.Success {
		return terr(tracker.WriteRejected, "Linear did not record that %s waits for %s", task.Identifier, blocker.Identifier)
	}
	a.mu.Lock()
	delete(a.cache, task.ID)
	a.mu.Unlock()
	return nil
}

// SubIssueIDs lists the sub-issues of an issue.
func (a *Adapter) SubIssueIDs(ctx context.Context, parent issue.Issue) ([]string, error) {
	var out []string
	q := `query($id: String!, $after: String) { issue(id: $id) { children(first: 100, after: $after) { nodes { id } pageInfo { hasNextPage endCursor } } } }`
	err := a.c.pages(ctx, q, map[string]any{"id": parent.ID}, func(raw json.RawMessage) (pageInfo, error) {
		var d struct {
			Issue *struct {
				Children struct {
					Nodes []struct {
						ID string `json:"id"`
					} `json:"nodes"`
					PageInfo pageInfo `json:"pageInfo"`
				} `json:"children"`
			} `json:"issue"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			return pageInfo{}, terr(tracker.Response, "sub-issues have an unexpected shape")
		}
		if d.Issue == nil {
			return pageInfo{}, nil
		}
		for _, c := range d.Issue.Children.Nodes {
			if c.ID != "" {
				out = append(out, c.ID)
			}
		}
		return d.Issue.Children.PageInfo, nil
	})
	return out, err
}
