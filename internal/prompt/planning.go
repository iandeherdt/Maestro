package prompt

import (
	"fmt"
	"strings"

	"github.com/iandeherdt/Maestro/internal/issue"
)

// The planning stage (SPEC 22) uses its own instructions. The workflow
// body is about implementing a ticket and is not shown here.
const planningPreamble = `You are a planning agent started by an orchestration service. No person is watching this session. The owner of the repository has written down an idea, and your job is to turn it into a plan, not to build it.

Your situation and limits:
- The current directory is a clone of the repository. Read whatever helps you plan. Do not change, create or commit files: nothing you change here is kept.
- You cannot reach the owner directly. If you need answers, you end this session with questions; the service posts them on the ticket, and a later session gets the owner's answers.
- Ask only questions whose answers would change the plan, at most five, each one short and specific. Do not ask what you can find out from the repository. If the idea and the earlier answers are clear enough, do not ask: make the plan.
- A plan is a list of small tasks. Each task becomes a ticket that another agent like you implements on its own, in one session, as one pull request, without seeing this conversation. So each description must stand on its own: what to build, where in the code, and how to tell that it is done.
- Order the tasks so that each one builds on finished work. Name what a task depends on. Prefer a few well-cut tasks over many tiny ones.
- Tasks that do not depend on each other are worked on at the same time, each on its own branch from the main branch, and merged in any order. Two tasks that change the same file then conflict. So cut the work along files: give each task files of its own, and prefer new files over changes to shared ones. When several tasks need the same change in a shared file (a settings class, a registry, a base class, a scene or project file), put that change in one early task that the others depend on.
- For every task, list in "touches" the files it will create or change, as paths from the top of the repository; a path ending in "/" names a whole folder. Look in the repository to get them right. The service makes a task wait for every earlier task that touches the same file, so an honest list costs nothing and a shared file costs speed. Leave out a file every task appends a line to, such as a change log.
- Each task is given to one of the machines listed below under WORKERS. Use the machine that can do the work; if there is no reason to choose, use the first.

Your final message must have one of these two forms.

To ask questions: write the questions as plain text, then end with a line containing exactly
MAESTRO_PLAN: questions

To propose a plan: write one json code block of this shape, then end with a line containing exactly
MAESTRO_PLAN: tasks

` + "```json" + `
{"tasks": [
  {"title": "one line, at most 120 characters", "description": "what to do, at most 6000 characters", "worker": "a name from WORKERS", "depends_on": [], "touches": ["src/health.go", "src/health_test.go"]},
  {"title": "...", "description": "...", "worker": "...", "depends_on": [1], "touches": ["src/screens/"]}
]}
` + "```" + `

depends_on lists earlier tasks by their position in the list, counted from 1. touches lists at most 40 paths. At most 20 tasks. No other fields.`

// PlanInput is what the first prompt of a planning run is built from.
type PlanInput struct {
	Issue    issue.Issue
	RepoName string
	Workers  []string
	// History is the conversation so far, oldest first: the service's
	// earlier questions and plans (source "planner") and the owner's
	// comments.
	History []issue.Feedback
	// Pictures are the idea's pictures, in the workspace (SPEC 13.5).
	Pictures    []TicketPicture
	PictureNote string
}

// Planning builds the first prompt of a planning run.
func (b *Builder) Planning(in PlanInput) (string, error) {
	tok, err := b.token()
	if err != nil {
		return "", err
	}
	var out strings.Builder
	out.WriteString(planningPreamble)
	out.WriteString("\n\n")
	fmt.Fprintf(&out, dataRule, tok)
	fmt.Fprintf(&out, "\n\nThe repository is %s.\n\n", safeWord(strings.ReplaceAll(in.RepoName, "/", ".")))
	block(&out, tok, "WORKERS", "", strings.Join(in.Workers, "\n"))
	pictures(&out, in.Pictures, in.PictureNote)
	block(&out, tok, "IDEA_TITLE", "", in.Issue.Title)
	block(&out, tok, "IDEA_DESCRIPTION", "", in.Issue.Description)
	if len(in.History) > 0 {
		out.WriteString("What was said so far, oldest first. EARLIER_ROUND is what an earlier planning session asked or proposed; OWNER_COMMENT is what the owner wrote.\n\n")
	}
	for _, f := range in.History {
		if f.Source == "planner" {
			block(&out, tok, "EARLIER_ROUND", "", f.Body)
			continue
		}
		meta := ""
		if f.Author != "" {
			meta = "author=" + safeWord(f.Author)
		}
		block(&out, tok, "OWNER_COMMENT", meta, f.Body)
	}
	return strings.TrimRight(out.String(), "\n") + "\n", nil
}

// PlanningRetry builds the prompt for a later turn of a planning run whose
// answer could not be read. problem is the service's own description of
// what was wrong and contains none of the agent's text.
func (b *Builder) PlanningRetry(turn, maxTurns int, problem string) string {
	return fmt.Sprintf("Turn %d of at most %d. Your answer could not be used: %s.\n\n"+
		"Answer again in one of the two forms described at the start: questions followed by the line "+
		"\"MAESTRO_PLAN: questions\", or one json block followed by the line \"MAESTRO_PLAN: tasks\".\n",
		turn, maxTurns, problem)
}
