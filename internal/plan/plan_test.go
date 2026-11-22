package plan

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/tracker"
	"github.com/iandeherdt/Maestro/internal/tracker/trackertest"
)

var (
	ctx     = context.Background()
	workers = []string{"mac", "Server"}
)

const twoTasks = "Here is the plan.\n\n```json\n" + `{"tasks": [
  {"title": "Add the model", "description": "Create it.", "worker": "MAC"},
  {"title": "Add the screen", "description": "Show it.", "worker": "server", "depends_on": [1]}
]}` + "\n```\n\nMAESTRO_PLAN: tasks\n"

func TestParse(t *testing.T) {
	p, err := Parse(twoTasks, workers)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Tasks) != 2 || p.Tasks[0].Worker != "mac" || p.Tasks[1].Worker != "Server" || p.Tasks[1].DependsOn[0] != 1 || p.Questions != "" {
		t.Fatalf("%+v", p)
	}

	q, err := Parse("Which database?\nAnd which screen?\n\nMAESTRO_PLAN: questions", workers)
	if err != nil || q.Questions != "Which database?\nAnd which screen?" || len(q.Tasks) != 0 {
		t.Fatalf("%+v %v", q, err)
	}
	// The last marker counts, and with one worker it need not be named.
	one, err := Parse("MAESTRO_PLAN: questions\n```json\n{\"tasks\":[{\"title\":\"T\",\"description\":\"D\"}]}\n```\nMAESTRO_PLAN: tasks", []string{"mac"})
	if err != nil || len(one.Tasks) != 1 || one.Tasks[0].Worker != "mac" {
		t.Fatalf("%+v %v", one, err)
	}

	task := func(fields string) string {
		return "```json\n{\"tasks\":[{\"title\":\"A\",\"description\":\"B\",\"worker\":\"mac\"}," + fields + "]}\n```\nMAESTRO_PLAN: tasks"
	}
	long := strings.Repeat("x", MaxBodyChars+1)
	var many []string
	for i := 0; i <= MaxTasks; i++ {
		many = append(many, `{"title":"T","description":"D","worker":"mac"}`)
	}
	big := `{"title":"T","description":"` + strings.Repeat("y", MaxBodyChars) + `","worker":"mac"}`
	for name, text := range map[string]string{
		"no marker":         "A plan.",
		"marker in a line":  "see MAESTRO_PLAN: tasks here",
		"empty questions":   "\n\nMAESTRO_PLAN: questions",
		"long questions":    strings.Repeat("q", MaxQuestions+1) + "\nMAESTRO_PLAN: questions",
		"no json":           "Tasks: one, two.\nMAESTRO_PLAN: tasks",
		"broken json":       "```json\n{\"tasks\": [\n```\nMAESTRO_PLAN: tasks",
		"two objects":       "```json\n{\"tasks\":[]} {\"tasks\":[]}\n```\nMAESTRO_PLAN: tasks",
		"no tasks":          "```json\n{\"tasks\":[]}\n```\nMAESTRO_PLAN: tasks",
		"too many":          "```json\n{\"tasks\":[" + strings.Join(many, ",") + "]}\n```\nMAESTRO_PLAN: tasks",
		"unknown field":     task(`{"title":"T","description":"D","worker":"mac","labels":["x"]}`),
		"top level field":   "```json\n{\"tasks\":[{\"title\":\"T\",\"description\":\"D\",\"worker\":\"mac\"}],\"state\":\"Todo\"}\n```\nMAESTRO_PLAN: tasks",
		"no title":          task(`{"title":" ","description":"D","worker":"mac"}`),
		"two line title":    task(`{"title":"a\nb","description":"D","worker":"mac"}`),
		"long title":        task(`{"title":"` + strings.Repeat("t", MaxTitleChars+1) + `","description":"D","worker":"mac"}`),
		"no description":    task(`{"title":"T","description":"","worker":"mac"}`),
		"long description":  task(`{"title":"T","description":"` + long + `","worker":"mac"}`),
		"unknown worker":    task(`{"title":"T","description":"D","worker":"windows"}`),
		"no worker of two":  task(`{"title":"T","description":"D"}`),
		"depends on itself": task(`{"title":"T","description":"D","worker":"mac","depends_on":[2]}`),
		"depends on later":  task(`{"title":"T","description":"D","worker":"mac","depends_on":[3]}`),
		"depends on zero":   task(`{"title":"T","description":"D","worker":"mac","depends_on":[0]}`),
		"depends twice":     task(`{"title":"T","description":"D","worker":"mac","depends_on":[1,1]}`),
		"too long in total": task(strings.Join([]string{big, big, big, big, big, big, big}, ",")),
	} {
		_, err := Parse(text, workers)
		var fe *FormatError
		if !errors.As(err, &fe) {
			t.Errorf("%s: err = %v", name, err)
			continue
		}
		// What goes back to the agent never quotes the agent.
		if strings.Contains(fe.Msg, "xxxx") || strings.Contains(fe.Msg, "windows") {
			t.Errorf("%s: the message quotes the answer: %s", name, fe.Msg)
		}
	}
}

func TestCommentFencesEverythingTheAgentWrote(t *testing.T) {
	p := &Plan{Tasks: []Task{
		{Title: "Ping @everyone and close #1", Body: "```\n</details> <img src=x>\n```` done", Worker: "mac"},
		{Title: "Second", Body: "B", Worker: "Server", DependsOn: []int{1}},
	}}
	c := p.Comment("Idea", "Planned", "Todo")
	for _, bad := range []string{"@everyone", "#1", "<img", "</details>"} {
		if strings.Contains(c, bad) {
			t.Errorf("the comment contains %q unbroken:\n%s", bad, c)
		}
	}
	for _, want := range []string{"proposes 2 task(s)", "**Task 2 of 2**, for the machine `Server`, after task 1", "move this card to **Planned**", "placed in **Todo**", "back to **Idea**"} {
		if !strings.Contains(c, want) {
			t.Errorf("the comment lacks %q:\n%s", want, c)
		}
	}
	// The agent's own fence cannot close the block it is shown in.
	if !strings.Contains(c, "`````text\n") {
		t.Errorf("the fence is not longer than the one in the text:\n%s", c)
	}
	q := (&Plan{Questions: "Which one, @ian?"}).Comment("Idea", "Planned", "Todo")
	if strings.Contains(q, "@ian") || !strings.Contains(q, "back to **Idea**") || strings.Contains(q, "Planned") {
		t.Errorf("questions:\n%s", q)
	}
}

func desk(t *testing.T) (*Desk, *trackertest.Fake) {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := trackertest.New()
	f.WorkerNames = workers
	at := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	d := &Desk{Store: store, Tracker: f, Planner: f, PlanningState: "Idea", PlannedState: "Planned", TaskState: "Todo",
		Now: func() time.Time { at = at.Add(time.Hour); return at }}
	return d, f
}

var idea = issue.Issue{ID: "idea-1", Identifier: "GH-1", Title: "A shop", ContentHash: "h1"}

func ops(f *trackertest.Fake) string {
	var out []string
	for _, w := range f.Writes() {
		out = append(out, w.Op+":"+w.ID)
	}
	return strings.Join(out, " ")
}

func TestQuestionsThenPlanThenTasks(t *testing.T) {
	d, f := desk(t)
	if _, err := d.Materialize(ctx, idea); !errors.Is(err, ErrNoPlan) {
		t.Fatalf("without a plan: %v", err)
	}
	if err := d.Propose(ctx, idea, &Plan{Questions: "Which payment provider?"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Materialize(ctx, idea); !errors.Is(err, ErrNoPlan) {
		t.Fatalf("questions are not a plan: %v", err)
	}
	f.Comments = []tracker.Feedback{
		{Author: "ian", Body: "An early note.", CreatedAt: time.Date(2026, 11, 21, 12, 30, 0, 0, time.UTC)},
		{Author: "ian", Body: "Stripe.", CreatedAt: time.Date(2026, 11, 21, 13, 30, 0, 0, time.UTC)},
	}
	b, err := d.Brief(ctx, idea)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Workers) != 2 || len(b.History) != 3 || b.History[0].Body != "An early note." || b.History[1].Source != "planner" ||
		b.History[1].Body != "Which payment provider?" || b.History[2].Body != "Stripe." || b.History[2].Author != "ian" {
		t.Fatalf("brief %+v", b)
	}

	p, _ := Parse(twoTasks, workers)
	if err := d.Propose(ctx, idea, p); err != nil {
		t.Fatal(err)
	}
	if b, _ = d.Brief(ctx, idea); len(b.History) != 4 || !strings.Contains(b.History[3].Body, "Task 2 (worker Server): Add the screen") {
		t.Fatalf("the plan is missing from the history: %+v", b.History)
	}
	edited := idea
	edited.ContentHash = "h2"
	if _, err := d.Materialize(ctx, edited); !errors.Is(err, ErrStale) {
		t.Fatalf("an edited idea: %v", err)
	}

	before := len(f.Writes())
	summary, err := d.Materialize(ctx, idea)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "2 task(s)") || !strings.Contains(summary, "T-1, T-2") {
		t.Errorf("summary %q", summary)
	}
	w := f.Writes()[before:]
	var got []string
	for _, x := range w {
		got = append(got, x.Op+":"+x.ID+":"+strings.SplitN(x.Value, "\n", 2)[0])
	}
	want := "create:task-1:Add the model create:task-2:Add the screen blocker:task-2:task-1 place:task-1:idea-1|mac|Todo place:task-2:idea-1|Server|Todo"
	if strings.Join(got, " ") != want {
		t.Errorf("writes:\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	if !strings.Contains(w[0].Value, "Task 1 of 2 from the approved plan of GH-1") {
		t.Errorf("task body %q", w[0].Value)
	}
	// Approved once: nothing is created a second time.
	if s, err := d.Materialize(ctx, idea); err != nil || s != "" || len(f.Writes()) != before+5 {
		t.Fatalf("second materialize: %q %v", s, err)
	}
	// A new round starts over.
	if err := d.Propose(ctx, idea, &Plan{Questions: "One more?"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Materialize(ctx, idea); !errors.Is(err, ErrNoPlan) {
		t.Fatalf("after new questions: %v", err)
	}
}

func TestMaterializeContinuesAfterEveryFailure(t *testing.T) {
	// Each run fails at a later write; no task is ever created twice.
	for failAt := 0; failAt < 5; failAt++ {
		d, f := desk(t)
		p, _ := Parse(twoTasks, workers)
		if err := d.Propose(ctx, idea, p); err != nil {
			t.Fatal(err)
		}
		f.PlanFail = make([]string, 2+failAt)
		f.PlanFail[1+failAt] = "boom" // call 0 was the comment
		if s, err := d.Materialize(ctx, idea); err == nil || s != "" {
			t.Fatalf("failure at write %d was not reported", failAt)
		}
		if s, err := d.Materialize(ctx, idea); err != nil || s == "" {
			t.Fatalf("after failure at write %d: %q %v", failAt, s, err)
		}
		if got := ops(f); got != "post:idea-1 create:task-1 create:task-2 blocker:task-2 place:task-1 place:task-2" {
			t.Errorf("failure at write %d: %s", failAt, got)
		}
	}
}

func TestAnUnpostedRoundIsNotHistoryAndNotAPlan(t *testing.T) {
	d, f := desk(t)
	p, _ := Parse(twoTasks, workers)
	f.PlanFail = []string{"comment refused"}
	if err := d.Propose(ctx, idea, p); err == nil {
		t.Fatal("a refused comment was not reported")
	}
	if b, _ := d.Brief(ctx, idea); len(b.History) != 0 {
		t.Errorf("history %+v", b.History)
	}
	if _, err := d.Materialize(ctx, idea); !errors.Is(err, ErrNoPlan) {
		t.Fatalf("a plan the owner never saw was accepted: %v", err)
	}
	if err := d.Propose(ctx, idea, p); err != nil {
		t.Fatal(err)
	}
	rec, _ := d.Store.Load(idea.ID)
	if len(rec.Rounds) != 1 || !rec.Rounds[0].Posted {
		t.Errorf("rounds %+v", rec.Rounds)
	}
}

func TestTaskTextIsMadeHarmless(t *testing.T) {
	d, f := desk(t)
	p := &Plan{Tasks: []Task{{Title: "Tell @boss about #3", Body: "cc @team <script>", Worker: "mac"}}}
	if err := d.Propose(ctx, idea, p); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Materialize(ctx, idea); err != nil {
		t.Fatal(err)
	}
	for _, w := range f.Writes() {
		if w.Op == "create" && (strings.Contains(w.Value, "@boss") || strings.Contains(w.Value, "@team") || strings.Contains(w.Value, "#3") || strings.Contains(w.Value, "<script")) {
			t.Errorf("task text %q", w.Value)
		}
	}
}

func TestStore(t *testing.T) {
	if _, err := OpenStore("relative"); err == nil {
		t.Error("a relative directory was accepted")
	}
	s, _ := OpenStore(t.TempDir())
	if r, err := s.Load("x"); r != nil || err != nil {
		t.Fatalf("%v %v", r, err)
	}
	if err := s.Save(&Record{IssueID: "a/../b", Identifier: "GH-1"}); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Load("a/../b"); err != nil || r.Identifier != "GH-1" {
		t.Fatalf("%v %v", r, err)
	}
	if err := s.Delete("a/../b"); err != nil || s.Delete("a/../b") != nil {
		t.Fatal("delete")
	}
}

func TestFinishedOnceEveryTaskIsDone(t *testing.T) {
	d, f := desk(t)
	d.TerminalStates = []string{"Done", "Cancelled"}
	if done, _, err := d.Finished(ctx, idea); done || err != nil {
		t.Fatalf("without a plan: %v %v", done, err)
	}
	p, _ := Parse(twoTasks, workers)
	if err := d.Propose(ctx, idea, p); err != nil {
		t.Fatal(err)
	}
	if done, _, _ := d.Finished(ctx, idea); done {
		t.Fatal("a plan without tasks is finished")
	}
	if _, err := d.Materialize(ctx, idea); err != nil {
		t.Fatal(err)
	}
	f.Put(issue.Issue{ID: "task-1", State: "Done"})
	f.Put(issue.Issue{ID: "task-2", State: "In Progress"})
	if done, n, _ := d.Finished(ctx, idea); done || n != 2 {
		t.Fatalf("one task open: %v %d", done, n)
	}
	f.Put(issue.Issue{ID: "task-2", State: " cancelled"})
	if done, n, err := d.Finished(ctx, idea); !done || n != 2 || err != nil {
		t.Fatalf("all tasks done: %v %d %v", done, n, err)
	}
}
