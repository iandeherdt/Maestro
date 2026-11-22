package prompt

import (
	"fmt"
	"strings"

	"github.com/iandeherdt/Maestro/internal/issue"
)

// The design stage (SPEC 27) has its own instructions too. A design agent
// writes files, but only in one folder, and nothing it writes is code that
// anyone runs outside a browser.
const designPreamble = `You are a design agent started by an orchestration service. No person is watching this session and nobody can answer questions. The owner of the repository has an idea and a plan of tasks for it. Your job is to make the design the tasks will be built from, so that the owner can see what they will get before any of it is built. You do not build the tasks.

Your situation and limits:
- The current directory is a clone of the repository on a branch made for this design. Read whatever helps: the existing pages, styles, components, assets and conventions. A design that fits what is already there is worth more than a new style.
- Write only inside the folder named under DESIGN_FOLDER below. Commits that change anything outside it are rejected as a whole. Commit your work; uncommitted changes are lost.
- Everything must work offline, opened straight from disk in a browser: no web fonts, no CDNs, no images or scripts from other sites, no build step, no package installs. Use plain HTML, CSS, SVG and, where it helps, a little plain JavaScript.
- No binary files: no PNG, JPEG, fonts or archives. Draw what you need with CSS or SVG, and use simple placeholders for real photos.
- Put an index.html at the top of the folder that is the overview: what the design covers and a link to every other page. The service takes a picture of index.html and of up to five other pages, in alphabetical order of their path, at 1280 by 800 pixels, so make the first screen of each page count.
- Write a short README.md in the folder for the agents that build the tasks: which page belongs to which task, the colours, type sizes, spacing and components to reuse, and anything the pictures cannot show (states, hover, motion, behaviour on small screens).
- You cannot push, open pull requests or update the ticket. The service does that after checking your commits.

What to make depends on the project, named under DESIGN_KIND:
- web: mock-ups of the pages and screens the plan adds or changes, as real HTML and CSS at realistic content and size. Show the important states (empty, filled, error) where they matter, and how a page looks on a phone if the project has users on phones.
- game: a game design. index.html describes the core loop, the controls, the rules and mechanics, progression or levels, and the art direction with a colour palette. Add mock screens of the main moments (title, play, pause, win or lose, menus) drawn with HTML, CSS, SVG or a canvas, and, where it makes an idea clear, a tiny playable sketch in plain JavaScript.
- auto: decide from the repository and the idea which of the two fits, and say in index.html which one you chose.

End your final message with one line: "MAESTRO_STATUS: done", "MAESTRO_STATUS: continue" or "MAESTRO_STATUS: blocked", followed by one line giving the reason.`

// DesignInput is what the first prompt of a design run is built from.
type DesignInput struct {
	Issue    issue.Issue
	RepoName string
	Kind     string
	Folder   string
	// Tasks are the plan as title and description. Untrusted.
	Tasks []DesignTask
	// History is what was said since the plan, oldest first: the service's
	// note on an earlier design (source "designer"), gate feedback, and the
	// owner's comments.
	History []issue.Feedback
}

// DesignTask is one task of the plan a design is for.
type DesignTask struct{ Title, Description string }

// Design builds the first prompt of a design run.
func (b *Builder) Design(in DesignInput) (string, error) {
	tok, err := b.token()
	if err != nil {
		return "", err
	}
	var out strings.Builder
	out.WriteString(designPreamble)
	out.WriteString("\n\n")
	fmt.Fprintf(&out, dataRule, tok)
	fmt.Fprintf(&out, "\n\nThe repository is %s.\n\n", safeWord(strings.ReplaceAll(in.RepoName, "/", ".")))
	// The folder and the kind come from the service's settings, not from
	// the ticket.
	fmt.Fprintf(&out, "DESIGN_FOLDER: %s\nDESIGN_KIND: %s\n\n", in.Folder, safeWord(in.Kind))
	block(&out, tok, "IDEA_TITLE", "", in.Issue.Title)
	block(&out, tok, "IDEA_DESCRIPTION", "", in.Issue.Description)
	var plan strings.Builder
	for i, t := range in.Tasks {
		fmt.Fprintf(&plan, "Task %d: %s\n%s\n\n", i+1, t.Title, t.Description)
	}
	block(&out, tok, "PLAN", "", strings.TrimSpace(plan.String()))
	if len(in.History) > 0 {
		out.WriteString("What was said since the plan, oldest first. EARLIER_DESIGN is the service's note on an earlier design; GATE_FEEDBACK is why the service refused earlier commits; OWNER_COMMENT is what the owner wrote. Where the owner asks for changes, make them.\n\n")
	}
	for _, f := range in.History {
		switch f.Source {
		case "designer":
			block(&out, tok, "EARLIER_DESIGN", "", f.Body)
		case "tracker", "pull_request", "review":
			meta, body := "", f.Body
			if f.Author != "" {
				meta = "author=" + safeWord(f.Author)
			}
			if f.Path != "" {
				// A path is untrusted text too. It goes inside the block.
				body = fmt.Sprintf("path: %s\nline: %d\n\n%s", f.Path, f.Line, f.Body)
			}
			block(&out, tok, "OWNER_COMMENT", meta, body)
		default:
			block(&out, tok, "GATE_FEEDBACK", "", f.Body)
		}
	}
	return strings.TrimRight(out.String(), "\n") + "\n", nil
}
