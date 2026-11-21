// Package plan is the planning stage (SPEC section 22): an idea becomes
// questions for the owner, then a plan of small tasks, and after the owner
// approved the plan, tickets on the board.
//
// The planning agent only ever produces text. This package reads that text
// strictly, shows the owner exactly what it understood, and creates tickets
// from what was shown and from nothing else.
package plan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/iandeherdt/Maestro/internal/tracker"
)

// Limits on what a planning agent may hand in. The whole plan has to fit
// in one comment the owner can read.
const (
	MaxTasks      = 20
	MaxTitleChars = 120
	MaxBodyChars  = 6000
	MaxPlanChars  = 40000
	MaxQuestions  = 6000
)

// Task is one task of a plan. All text in it is untrusted.
type Task struct {
	Title  string `json:"title"`
	Body   string `json:"description"`
	Worker string `json:"worker"`
	// DependsOn lists the tasks that have to be finished first, by their
	// position in the plan, counted from 1. Only earlier tasks.
	DependsOn []int `json:"depends_on,omitempty"`
}

// Plan is the result of one planning run: questions, or tasks.
type Plan struct {
	Questions string `json:"questions,omitempty"`
	Tasks     []Task `json:"tasks,omitempty"`
}

// FormatError says what is wrong with a planning agent's answer, in words
// that are sent back to the agent. It never quotes the agent's text.
type FormatError struct{ Msg string }

func (e *FormatError) Error() string { return e.Msg }

func bad(format string, a ...any) error { return &FormatError{Msg: fmt.Sprintf(format, a...)} }

var (
	markerRE = regexp.MustCompile(`(?m)^[ \t]*SYMPHONY_PLAN:[ \t]*(questions|tasks)[ \t]*\r?$`)
	jsonRE   = regexp.MustCompile("(?s)```json[ \\t]*\\r?\\n(.*?)\\r?\\n[ \\t]*```")
)

// Parse reads the final message of a planning run. workers are the
// machines a task may be given to.
func Parse(result string, workers []string) (*Plan, error) {
	marks := markerRE.FindAllStringSubmatchIndex(result, -1)
	if len(marks) == 0 {
		return nil, bad(`the answer has no line "SYMPHONY_PLAN: questions" or "SYMPHONY_PLAN: tasks"`)
	}
	last := marks[len(marks)-1]
	kind, before := result[last[2]:last[3]], result[:last[0]]

	if kind == "questions" {
		q := strings.TrimSpace(before)
		switch {
		case q == "":
			return nil, bad("the questions have to come before the SYMPHONY_PLAN line, and there are none")
		case utf8.RuneCountInString(q) > MaxQuestions:
			return nil, bad("the questions are longer than %d characters; ask fewer and shorter questions", MaxQuestions)
		}
		return &Plan{Questions: q}, nil
	}

	blocks := jsonRE.FindAllStringSubmatch(before, -1)
	if len(blocks) == 0 {
		return nil, bad("there is no ```json block before the SYMPHONY_PLAN line")
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(blocks[len(blocks)-1][1])))
	dec.DisallowUnknownFields()
	var in struct {
		Tasks []Task `json:"tasks"`
	}
	if err := dec.Decode(&in); err != nil || dec.More() {
		return nil, bad(`the json block is not valid: it must be one object {"tasks": [{"title", "description", "worker", "depends_on"}]} with no other fields`)
	}
	if len(in.Tasks) == 0 || len(in.Tasks) > MaxTasks {
		return nil, bad("a plan has between 1 and %d tasks, this one has %d", MaxTasks, len(in.Tasks))
	}
	total := 0
	for i := range in.Tasks {
		t := &in.Tasks[i]
		n := i + 1
		t.Title, t.Body, t.Worker = strings.TrimSpace(t.Title), strings.TrimSpace(t.Body), strings.TrimSpace(t.Worker)
		switch {
		case t.Title == "" || strings.ContainsAny(t.Title, "\r\n") || utf8.RuneCountInString(t.Title) > MaxTitleChars:
			return nil, bad("task %d: the title must be one line of at most %d characters", n, MaxTitleChars)
		case t.Body == "" || utf8.RuneCountInString(t.Body) > MaxBodyChars:
			return nil, bad("task %d: the description must not be empty and at most %d characters", n, MaxBodyChars)
		}
		total += utf8.RuneCountInString(t.Title) + utf8.RuneCountInString(t.Body)
		worker := ""
		for _, w := range workers {
			if strings.EqualFold(w, t.Worker) || t.Worker == "" && len(workers) == 1 {
				worker = w
			}
		}
		if worker == "" {
			return nil, bad("task %d: the worker must be one of: %s", n, strings.Join(workers, ", "))
		}
		t.Worker = worker
		seen := map[int]bool{}
		for _, d := range t.DependsOn {
			if d < 1 || d >= n || seen[d] {
				return nil, bad("task %d: depends_on may only name earlier tasks by number, each once (1 to %d)", n, n-1)
			}
			seen[d] = true
		}
	}
	if total > MaxPlanChars {
		return nil, bad("the plan is longer than %d characters in total; make the descriptions shorter", MaxPlanChars)
	}
	return &Plan{Tasks: in.Tasks}, nil
}

// Comment renders the plan for the owner. Everything the agent wrote is
// inside fenced blocks. The states are named so that the comment says what
// to do next.
func (p *Plan) Comment(planning, planned, taskState string) string {
	var b strings.Builder
	if len(p.Tasks) == 0 {
		b.WriteString("The planning agent has questions before it can make a plan:\n\n")
		b.WriteString(tracker.FenceMax(p.Questions, MaxQuestions))
		fmt.Fprintf(&b, "\n\nAnswer in a comment on this issue, then move the card back to **%s**.", planning)
		return b.String()
	}
	fmt.Fprintf(&b, "The planning agent proposes %d task(s).\n", len(p.Tasks))
	for i, t := range p.Tasks {
		fmt.Fprintf(&b, "\n**Task %d of %d**, for the machine `%s`", i+1, len(p.Tasks), tracker.Neutral(strings.ReplaceAll(t.Worker, "`", ""), 60))
		if len(t.DependsOn) > 0 {
			b.WriteString(", after task ")
			for j, d := range t.DependsOn {
				if j > 0 {
					b.WriteString(", ")
				}
				fmt.Fprint(&b, d)
			}
		}
		b.WriteString("\n\n")
		b.WriteString(tracker.FenceMax(t.Title+"\n\n"+t.Body, MaxTitleChars+MaxBodyChars+2))
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nTo approve, move this card to **%s**. The tasks are then created exactly as shown and placed in **%s**, "+
		"where the agents pick them up in the order given. To change the plan, write a comment and move the card back to **%s**.",
		planned, taskState, planning)
	return b.String()
}
