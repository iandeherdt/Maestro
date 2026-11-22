package github

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/tracker"
)

// This file implements tracker.Planner (SPEC 22): comments that talk to
// the owner, and the tickets an approved plan is turned into. It needs a
// board, because a task is routed and approved through its card.

var _ tracker.Planner = (*Adapter)(nil)

const addCardMutation = `
mutation($project: ID!, $content: ID!) {
  addProjectV2ItemById(input: {projectId: $project, contentId: $content}) { item { id } }
}`

func (a *Adapter) needBoard() error {
	if a.board == nil {
		return terr(tracker.InvalidConfig, "planning needs a board: tracker.provider.state_source must be project")
	}
	return nil
}

// Workers lists the choices of the board's Worker field.
func (a *Adapter) Workers(ctx context.Context) ([]string, error) {
	if err := a.needBoard(); err != nil {
		return nil, err
	}
	m, err := a.meta(ctx, false)
	if err != nil {
		return nil, err
	}
	if len(m.workers) == 0 {
		return nil, terr(tracker.Status, "%s has no single-select field named %q with at least one choice", a.boardName(), a.board.workerField)
	}
	return append([]string(nil), m.workers...), nil
}

// PostComment adds a comment to the issue.
func (a *Adapter) PostComment(ctx context.Context, is issue.Issue, body string) error {
	n, err := numberOf(is)
	if err != nil {
		return err
	}
	_, err = a.c.do(ctx, http.MethodPost, a.c.url(a.path("/issues/%d/comments", n), nil), map[string]string{"body": body}, nil, true)
	return err
}

// CreateTask opens an issue. The text comes from a plan and was made
// harmless by the caller.
func (a *Adapter) CreateTask(ctx context.Context, t tracker.Task) (tracker.TaskRef, error) {
	if err := a.needBoard(); err != nil {
		return tracker.TaskRef{}, err
	}
	if strings.TrimSpace(t.Title) == "" {
		return tracker.TaskRef{}, terr(tracker.WriteRejected, "a task needs a title")
	}
	var out struct {
		ID     int64  `json:"id"`
		NodeID string `json:"node_id"`
		Number int    `json:"number"`
	}
	_, err := a.c.do(ctx, http.MethodPost, a.c.url(a.path("/issues"), nil), map[string]string{"title": t.Title, "body": t.Body}, &out, true)
	if err != nil {
		return tracker.TaskRef{}, err
	}
	if out.ID <= 0 || out.NodeID == "" || out.Number <= 0 {
		return tracker.TaskRef{}, terr(tracker.Response, "the new issue came back without its identifiers")
	}
	return tracker.TaskRef{ID: out.NodeID, Identifier: "GH-" + strconv.Itoa(out.Number), NativeID: out.ID}, nil
}

func refNumber(ref tracker.TaskRef) (int, error) {
	return numberOf(issue.Issue{Identifier: ref.Identifier})
}

// alreadyDone reports whether a write was refused because what it asks for
// is already the case. GitHub answers 422 for a sub-issue or a dependency
// that exists.
func alreadyDone(err error) bool {
	var te *tracker.Error
	return errors.As(err, &te) && te.Category == tracker.WriteRejected && strings.HasSuffix(te.Msg, "status 422")
}

// PlaceTask makes the issue a sub-issue of its parent and puts its card on
// the board. The column is set last: until then the card is in no state,
// so a half-placed task is never picked up.
func (a *Adapter) PlaceTask(ctx context.Context, parent issue.Issue, ref tracker.TaskRef, worker, state string) error {
	if err := a.needBoard(); err != nil {
		return err
	}
	pn, err := numberOf(parent)
	if err != nil {
		return err
	}
	if ref.ID == "" || ref.NativeID <= 0 {
		return terr(tracker.WriteRejected, "the task has no identifiers")
	}
	m, err := a.meta(ctx, false)
	if err != nil {
		return err
	}
	if m.options[norm(state)] == "" || m.workerOptions[norm(worker)] == "" {
		if m, err = a.meta(ctx, true); err != nil {
			return err
		}
	}
	column, machine := m.options[norm(state)], m.workerOptions[norm(worker)]
	switch {
	case column == "":
		return terr(tracker.WriteRejected, "%s has no column %q", a.boardName(), state)
	case machine == "":
		return terr(tracker.WriteRejected, "%s has no worker %q in the field %q", a.boardName(), worker, a.board.workerField)
	}

	_, err = a.c.do(ctx, http.MethodPost, a.c.url(a.path("/issues/%d/sub_issues", pn), nil), map[string]int64{"sub_issue_id": ref.NativeID}, nil, true)
	if errors.Is(err, errNotFound) {
		return terr(tracker.WriteRejected, "issue %s no longer exists, or sub-issues are not available for this repository", parent.Identifier)
	}
	if err != nil && !alreadyDone(err) {
		return err
	}
	var added struct {
		AddProjectV2ItemByID struct {
			Item *struct {
				ID string `json:"id"`
			} `json:"item"`
		} `json:"addProjectV2ItemById"`
	}
	if err := a.c.graphql(ctx, addCardMutation, map[string]any{"project": m.id, "content": ref.ID}, &added, true); err != nil {
		return err
	}
	if added.AddProjectV2ItemByID.Item == nil || added.AddProjectV2ItemByID.Item.ID == "" {
		return terr(tracker.Response, "the board did not say which card it created")
	}
	item := added.AddProjectV2ItemByID.Item.ID
	for _, set := range []struct{ field, option string }{{m.workerID, machine}, {m.fieldID, column}} {
		err := a.c.graphql(ctx, moveMutation, map[string]any{"project": m.id, "item": item, "field": set.field, "option": set.option}, nil, true)
		if err != nil {
			return err
		}
	}
	return nil
}

// AddBlocker records that task waits for blocker.
func (a *Adapter) AddBlocker(ctx context.Context, task, blocker tracker.TaskRef) error {
	n, err := refNumber(task)
	if err != nil {
		return err
	}
	if blocker.NativeID <= 0 {
		return terr(tracker.WriteRejected, "the blocking task has no identifier")
	}
	_, err = a.c.do(ctx, http.MethodPost, a.c.url(a.path("/issues/%d/dependencies/blocked_by", n), nil), map[string]int64{"issue_id": blocker.NativeID}, nil, true)
	switch {
	case alreadyDone(err):
		return nil
	case errors.Is(err, errNotFound):
		// Without dependencies the tasks of a plan would all start at once.
		return terr(tracker.Status, "issue dependencies are not available for this repository, so the order of the tasks cannot be recorded")
	}
	return err
}

// MarkWorking adds a label to the issue. GitHub creates a label that does
// not exist yet.
func (a *Adapter) MarkWorking(ctx context.Context, is issue.Issue, label string) error {
	n, err := numberOf(is)
	if err != nil {
		return err
	}
	_, err = a.c.do(ctx, http.MethodPost, a.c.url(a.path("/issues/%d/labels", n), nil), map[string][]string{"labels": {label}}, nil, true)
	a.mu.Lock()
	delete(a.cache, n)
	a.mu.Unlock()
	return err
}

// UnmarkWorking removes the label from the issue.
func (a *Adapter) UnmarkWorking(ctx context.Context, is issue.Issue, label string) error {
	n, err := numberOf(is)
	if err != nil {
		return err
	}
	u := a.c.url(a.path("/issues/%d/labels/", n), nil) + url.PathEscape(label)
	_, err = a.c.do(ctx, http.MethodDelete, u, nil, nil, true)
	a.mu.Lock()
	delete(a.cache, n)
	a.mu.Unlock()
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}

var _ tracker.Activity = (*Adapter)(nil)
