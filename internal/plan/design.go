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

// The design stage (SPEC 27) is optional and comes after a plan. The owner
// moves an idea with a plan to the design state, an agent makes a design
// for the plan in a folder of the repository, and the service opens a pull
// request with it and pictures of every page. Merging that pull request is
// the owner's approval of the design: it puts the design into the
// repository, where the agents that build the tasks read it.

// DesignSuffix names the design's own workspace and branch, next to the
// idea's: maestro/GH-12-design. A merge of the design never counts as a
// merge of the idea itself.
const DesignSuffix = "-design"

// DesignIssue is the idea as the design's workspace, branch and pull
// request see it. Only the identifier differs.
func DesignIssue(is issue.Issue) issue.Issue {
	is.Identifier += DesignSuffix
	return is
}

// DesignFolder is the folder of one idea's design in the repository.
func DesignFolder(base, identifier string) string {
	key, err := issue.WorkspaceKey(identifier)
	if err != nil {
		return ""
	}
	return strings.Trim(base, "/") + "/" + key
}

// Design is a design the service published for the owner to review.
type Design struct {
	PRNumber int    `json:"pr_number"`
	Branch   string `json:"branch"`
	HeadSHA  string `json:"head_sha"`
	Folder   string `json:"folder"`
	Pictures int    `json:"pictures"`
}

// DesignRound is the latest design of an idea.
type DesignRound struct {
	At     time.Time `json:"at"`
	Design Design    `json:"design"`
	// ForRound is the plan round the design was made for, counted from 0.
	// A design for an earlier plan is not used.
	ForRound int `json:"for_round"`
	// Use is decided once, when the first task is created: true when the
	// design pull request was merged, false when it was closed.
	Use *bool `json:"use,omitempty"`
}

// DesignState is where the design pull request of an idea stands.
type DesignState int

const (
	DesignOpen DesignState = iota
	DesignMerged
	DesignClosed
)

// DesignReviews reads where an idea's design pull request stands.
type DesignReviews interface {
	DesignState(ctx context.Context, is issue.Issue) (DesignState, error)
}

// WaitingForDesign is returned by Materialize while the design pull request
// of the approved plan is still open.
type WaitingForDesign struct{ PRNumber int }

func (e *WaitingForDesign) Error() string {
	return fmt.Sprintf("the design in pull request #%d is not merged yet", e.PRNumber)
}

// ErrNoPlanToDesign: an idea is in the design state without a plan.
var ErrNoPlanToDesign = errors.New("there is no plan to make a design for")

// DesignBrief is what a design run is given besides the idea.
type DesignBrief struct {
	// Tasks are the plan the design is for. Untrusted, like all text the
	// planning agent wrote.
	Tasks []Task
	// History is what the owner wrote since the plan was proposed, oldest
	// first, and a note on the earlier design if there was one.
	History []issue.Feedback
	// Folder is where in the repository the design goes.
	Folder string
}

// HasPlan reports whether the idea's latest round is a plan the owner saw.
func (d *Desk) HasPlan(issueID string) bool {
	rec, err := d.Store.Load(issueID)
	if err != nil || rec == nil || len(rec.Rounds) == 0 {
		return false
	}
	last := rec.Rounds[len(rec.Rounds)-1]
	return last.Posted && len(last.Plan.Tasks) > 0
}

// HasDesign reports whether a design was published for the latest plan.
func (d *Desk) HasDesign(issueID string) bool {
	rec, err := d.Store.Load(issueID)
	return err == nil && rec != nil && rec.Design != nil && rec.Design.ForRound == len(rec.Rounds)-1
}

// DesignBrief collects what a design run needs.
func (d *Desk) DesignBrief(ctx context.Context, is issue.Issue) (DesignBrief, error) {
	rec, err := d.Store.Load(is.ID)
	if err != nil {
		return DesignBrief{}, err
	}
	if rec == nil || len(rec.Rounds) == 0 {
		return DesignBrief{}, ErrNoPlanToDesign
	}
	last := rec.Rounds[len(rec.Rounds)-1]
	if !last.Posted || len(last.Plan.Tasks) == 0 {
		return DesignBrief{}, ErrNoPlanToDesign
	}
	folder := DesignFolder(d.DesignFolder, is.Identifier)
	if folder == "" || d.DesignFolder == "" {
		return DesignBrief{}, errors.New("the design folder cannot be named")
	}
	comments, err := d.Tracker.FetchFeedback(ctx, is, time.Time{})
	if err != nil {
		return DesignBrief{}, err
	}
	type entry struct {
		at time.Time
		f  issue.Feedback
	}
	var all []entry
	if rec.Design != nil && rec.Design.ForRound == len(rec.Rounds)-1 {
		all = append(all, entry{rec.Design.At, issue.Feedback{Source: "designer",
			Body: fmt.Sprintf("An earlier session already made a design, in pull request #%d. It is on the current branch; change it rather than starting over.", rec.Design.Design.PRNumber)}})
	}
	for _, c := range comments {
		// What was said before the plan belongs to planning.
		if c.CreatedAt.Before(last.At) {
			continue
		}
		all = append(all, entry{c.CreatedAt, issue.Feedback{Source: "tracker", Author: c.Author, Body: c.Body}})
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].at.Before(all[b].at) })
	out := DesignBrief{Tasks: last.Plan.Tasks, Folder: folder}
	for _, e := range all {
		out.History = append(out.History, e.f)
	}
	return out, nil
}

// Designed remembers a published design and tells the owner what to do
// with it.
func (d *Desk) Designed(ctx context.Context, is issue.Issue, ds Design) error {
	rec, err := d.Store.Load(is.ID)
	if err != nil {
		return err
	}
	if rec == nil || len(rec.Rounds) == 0 {
		return ErrNoPlanToDesign
	}
	rec.Design = &DesignRound{At: d.now().UTC(), Design: ds, ForRound: len(rec.Rounds) - 1}
	if err := d.Store.Save(rec); err != nil {
		return err
	}
	pictures := "It has no pictures; the pull request says why."
	if ds.Pictures > 0 {
		pictures = fmt.Sprintf("It shows %d picture(s) of the design.", ds.Pictures)
	}
	return d.Planner.PostComment(ctx, is, fmt.Sprintf("The design for the plan is in pull request #%d, in the folder `%s`. %s\n\n"+
		"- **To approve it**, merge the pull request and move this card to **%s**. The tasks are then created and told to follow the design.\n"+
		"- **To change it**, comment on the pull request (or here and move the card back to **%s**).\n"+
		"- **To go on without it**, close the pull request and move this card to **%s**.",
		ds.PRNumber, tracker.Neutral(ds.Folder, 200), pictures, d.PlannedState, d.DesignState, d.PlannedState))
}

// designNote is added to every task of a plan whose design was approved.
func designNote(ds Design) string {
	return fmt.Sprintf("\n\nDesign: the owner approved a design for this work. It is in the folder `%s` of the repository "+
		"(pull request #%d). Read it before you start and follow it: the layout, the look and the behaviour it shows "+
		"are what the owner expects. Do not change the design files unless this task says so.", ds.Folder, ds.PRNumber)
}

// designDecision decides once whether the tasks follow the design. It
// returns the note to add to each task, which is empty without a design.
func (d *Desk) designDecision(ctx context.Context, is issue.Issue, rec *Record) (string, error) {
	ds := rec.Design
	if ds == nil || ds.ForRound != len(rec.Rounds)-1 {
		return "", nil
	}
	if ds.Use == nil {
		// Once a task exists the choice is fixed, so that all tasks of one
		// plan agree.
		started := false
		for _, p := range rec.Progress {
			started = started || p.Ref.ID != ""
		}
		if started {
			no := false
			ds.Use = &no
		} else {
			if d.Designs == nil {
				return "", errors.New("the state of the design pull request cannot be read")
			}
			st, err := d.Designs.DesignState(ctx, is)
			if err != nil {
				return "", err
			}
			switch st {
			case DesignOpen:
				return "", &WaitingForDesign{PRNumber: ds.Design.PRNumber}
			case DesignMerged:
				yes := true
				ds.Use = &yes
			default:
				no := false
				ds.Use = &no
			}
		}
		if err := d.Store.Save(rec); err != nil {
			return "", err
		}
	}
	if !*ds.Use {
		return "", nil
	}
	return designNote(ds.Design), nil
}
