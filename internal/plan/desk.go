package plan

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/tracker"
)

// Reasons a card in the planned state cannot be turned into tasks.
var (
	// ErrNoPlan: this machine has not proposed a plan for the idea.
	ErrNoPlan = errors.New("no plan was proposed for this idea on this machine")
	// ErrStale: the idea's text changed after the plan was proposed.
	ErrStale = errors.New("the idea was edited after the plan was proposed")
)

// Brief is what a planning run is given besides the idea itself.
type Brief struct {
	Workers []string
	// History is what was said so far, oldest first: the service's earlier
	// questions and plans, and the owner's comments.
	History []issue.Feedback
}

// Desk connects the planning stage to the board.
type Desk struct {
	Store   *Store
	Tracker tracker.Tracker
	Planner tracker.Planner
	// The names of the states, for the comments and for new tasks.
	PlanningState, PlannedState, TaskState string
	// DesignState and DesignFolder are set when the design stage is on
	// (SPEC 27). Designs reads the design pull requests.
	DesignState, DesignFolder string
	Designs                   DesignReviews
	// TerminalStates are the states a finished task is in.
	TerminalStates []string
	// Self is the worker name of this machine, the one that plans. The
	// idea was given to this machine, so every task of its plan goes to
	// it too: when the board lists Self, it is the only worker the
	// planner is offered.
	Self string
	Now  func() time.Time
}

func (d *Desk) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Brief collects the workers and the conversation for a planning run.
func (d *Desk) Brief(ctx context.Context, is issue.Issue) (Brief, error) {
	workers, err := d.Planner.Workers(ctx)
	if err != nil {
		return Brief{}, err
	}
	workers = onlySelf(workers, d.Self)
	rec, err := d.Store.Load(is.ID)
	if err != nil {
		return Brief{}, err
	}
	comments, err := d.Tracker.FetchFeedback(ctx, is, time.Time{})
	if err != nil {
		return Brief{}, err
	}
	type entry struct {
		at time.Time
		f  issue.Feedback
	}
	var all []entry
	if rec != nil {
		for _, r := range rec.Rounds {
			if !r.Posted {
				continue
			}
			body := r.Plan.Questions
			if len(r.Plan.Tasks) > 0 {
				var b strings.Builder
				b.WriteString("An earlier run proposed this plan:\n")
				for i, t := range r.Plan.Tasks {
					fmt.Fprintf(&b, "\nTask %d (worker %s): %s\n%s\n", i+1, t.Worker, t.Title, t.Body)
				}
				body = b.String()
			}
			all = append(all, entry{r.At, issue.Feedback{Source: "planner", Body: body}})
		}
	}
	for _, c := range comments {
		all = append(all, entry{c.CreatedAt, issue.Feedback{Source: "tracker", Author: c.Author, Body: c.Body}})
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].at.Before(all[b].at) })
	out := Brief{Workers: workers}
	for _, e := range all {
		out.History = append(out.History, e.f)
	}
	return out, nil
}

// Propose remembers the result of a planning run and shows it to the owner
// in a comment on the idea.
func (d *Desk) Propose(ctx context.Context, is issue.Issue, p *Plan) error {
	if p == nil || p.Questions == "" && len(p.Tasks) == 0 {
		return errors.New("plan: nothing to propose")
	}
	rec, err := d.Store.Load(is.ID)
	if err != nil {
		return err
	}
	if rec == nil {
		rec = &Record{IssueID: is.ID}
	}
	// A round that never reached the owner is replaced.
	if n := len(rec.Rounds); n > 0 && !rec.Rounds[n-1].Posted {
		rec.Rounds = rec.Rounds[:n-1]
	}
	rec.Identifier, rec.ContentHash = is.Identifier, is.ContentHash
	rec.Rounds = append(rec.Rounds, Round{At: d.now().UTC(), Plan: *p})
	// A design belongs to the plan it was made for.
	rec.Progress, rec.Done, rec.Design = nil, false, nil
	if err := d.Store.Save(rec); err != nil {
		return err
	}
	if err := d.Planner.PostComment(ctx, is, p.Comment(d.PlanningState, d.PlannedState, d.TaskState)); err != nil {
		return err
	}
	rec.Rounds[len(rec.Rounds)-1].Posted = true
	return d.Store.Save(rec)
}

// Materialize creates the tasks of the plan the owner approved by moving
// the idea to the planned state. It can be called again after a failure and
// continues where it stopped. The summary is non-empty once, when the last
// task has been placed.
//
// The order is: create every task, record every dependency, and only then
// put the cards on the board. A task is in no state before its card is
// placed, so nothing starts before the order of the work is recorded.
func (d *Desk) Materialize(ctx context.Context, is issue.Issue) (string, error) {
	rec, err := d.Store.Load(is.ID)
	if err != nil {
		return "", err
	}
	if rec == nil || len(rec.Rounds) == 0 {
		return "", ErrNoPlan
	}
	last := rec.Rounds[len(rec.Rounds)-1]
	if !last.Posted || len(last.Plan.Tasks) == 0 {
		return "", ErrNoPlan
	}
	if rec.Done {
		return "", nil
	}
	if rec.ContentHash != is.ContentHash {
		return "", ErrStale
	}
	tasks := last.Plan.Tasks
	if len(rec.Progress) != len(tasks) {
		rec.Progress = make([]Progress, len(tasks))
	}
	note, err := d.designDecision(ctx, is, rec)
	if err != nil {
		return "", err
	}
	for i, t := range tasks {
		if rec.Progress[i].Ref.ID != "" {
			continue
		}
		body := tracker.Neutral(t.Body, MaxBodyChars) + note +
			fmt.Sprintf("\n\n---\nTask %d of %d from the approved plan of %s.", i+1, len(tasks), is.Identifier)
		ref, err := d.Planner.CreateTask(ctx, tracker.Task{Title: tracker.Neutral(t.Title, MaxTitleChars), Body: body, Worker: t.Worker})
		if err != nil {
			return "", err
		}
		rec.Progress[i].Ref = ref
		// Saved at once: a task that exists must never be created twice.
		if err := d.Store.Save(rec); err != nil {
			return "", err
		}
	}
	for i, t := range tasks {
		if rec.Progress[i].Ordered {
			continue
		}
		for _, dep := range t.DependsOn {
			if err := d.Planner.AddBlocker(ctx, rec.Progress[i].Ref, rec.Progress[dep-1].Ref); err != nil {
				return "", err
			}
		}
		rec.Progress[i].Ordered = true
		if err := d.Store.Save(rec); err != nil {
			return "", err
		}
	}
	var names []string
	for i, t := range tasks {
		names = append(names, rec.Progress[i].Ref.Identifier)
		if rec.Progress[i].Placed {
			continue
		}
		if err := d.Planner.PlaceTask(ctx, is, rec.Progress[i].Ref, t.Worker, d.TaskState); err != nil {
			return "", err
		}
		rec.Progress[i].Placed = true
		if err := d.Store.Save(rec); err != nil {
			return "", err
		}
	}
	rec.Done = true
	if err := d.Store.Save(rec); err != nil {
		return "", err
	}
	with := ""
	if note != "" {
		with = fmt.Sprintf(" They follow the design of pull request #%d.", rec.Design.Design.PRNumber)
	}
	return fmt.Sprintf("The plan was approved. %d task(s) were created and placed in %s: %s.%s", len(tasks), d.TaskState, strings.Join(names, ", "), with), nil
}

// Finished reports whether every task of the approved plan of an idea is
// done, so that the idea itself can be moved to Done. It also returns the
// number of tasks. A task that is no longer visible, for example because
// it was deleted, counts as done: it cannot be finished any more.
func (d *Desk) Finished(ctx context.Context, is issue.Issue) (bool, int, error) {
	rec, err := d.Store.Load(is.ID)
	if err != nil || rec == nil || !rec.Done || len(rec.Progress) == 0 {
		return false, 0, err
	}
	ids := make([]string, 0, len(rec.Progress))
	for _, p := range rec.Progress {
		if p.Ref.ID == "" {
			return false, 0, nil
		}
		ids = append(ids, p.Ref.ID)
	}
	tasks, err := d.Tracker.FetchIssuesByIDs(ctx, ids)
	if err != nil {
		return false, 0, err
	}
	for _, t := range tasks {
		done := false
		for _, s := range d.TerminalStates {
			done = done || strings.EqualFold(strings.TrimSpace(t.State), strings.TrimSpace(s))
		}
		if !done {
			return false, len(ids), nil
		}
	}
	return true, len(ids), nil
}

// onlySelf narrows the workers to the one named self. The list is left
// as it is when self is not on it.
func onlySelf(workers []string, self string) []string {
	for _, w := range workers {
		if self != "" && strings.EqualFold(w, self) {
			return []string{w}
		}
	}
	return workers
}
