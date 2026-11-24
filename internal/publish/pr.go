package publish

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/iandeherdt/Maestro/internal/tracker"
)

const (
	maxTitleRunes = 120
	footer        = "Opened by Maestro. A person reviews and merges; the service never does."
)

// clean removes control and invisible characters and caps the length.
func clean(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || r == unicode.ReplacementChar {
			continue
		}
		if n >= max {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// title builds the pull request title. The issue title is the board's
// text: mentions and references in it are defused.
func title(identifier, issueTitle string) string {
	t := clean(identifier+": "+issueTitle, maxTitleRunes)
	t = strings.NewReplacer("@", "@\u200b", "#", "#\u200b").Replace(t)
	return t
}

// code renders untrusted text as inline code that cannot break out.
func code(s string) string {
	// GitHub does not link mentions inside code, the break is a second layer.
	return "`" + strings.NewReplacer("`", "'", "@", "@\u200b").Replace(clean(s, 300)) + "`"
}

// Proof is what the service itself established about the work.
type Proof struct {
	VerifyCommand string
	VerifyStatus  int
	Worker        string
	Commits       int
	Files         int
	// Screenshots the service took, and what it has to say about them.
	Screenshots    []shotLink
	ScreenshotNote string
}

// body builds the pull request description from a fixed template. Nothing
// the agent wrote is placed outside inline code or a fenced block.
func body(closes string, flags []flagLine, p Proof, summary string) string {
	var b strings.Builder
	if closes != "" {
		b.WriteString(closes + "\n\n")
	}
	b.WriteString("## Review flags\n\n")
	if len(flags) == 0 {
		b.WriteString("None.\n")
	}
	for _, f := range flags {
		b.WriteString("- **" + f.rule + "** " + f.detail)
		if f.path != "" {
			b.WriteString(": " + code(f.path))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n## Checked by the service\n\n")
	if strings.TrimSpace(p.VerifyCommand) == "" {
		b.WriteString("- No verify command is configured.\n")
	} else {
		fmt.Fprintf(&b, "- Verify command exited with status %d:\n\n", p.VerifyStatus)
		b.WriteString(indent(tracker.Fence(p.VerifyCommand)) + "\n")
	}
	fmt.Fprintf(&b, "- %d commit(s), %d file(s) changed. Diff policy and secret scan passed.\n", p.Commits, p.Files)
	if p.Worker != "" {
		b.WriteString("- Worker: " + code(p.Worker) + "\n")
	}
	if len(p.Screenshots) > 0 || p.ScreenshotNote != "" {
		b.WriteString("\n## Screenshots\n\n")
		if len(p.Screenshots) > 0 {
			b.WriteString("Taken by the service after the checks passed, from the commits in this pull request. " +
				"They show what the code draws; nobody has judged them yet.\n")
		}
		for i, s := range p.Screenshots {
			fmt.Fprintf(&b, "\n%s\n\n![Screenshot %d](%s)\n", code(s.caption), i+1, s.url)
		}
		if p.ScreenshotNote != "" {
			if len(p.Screenshots) > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.ScreenshotNote + "\n")
		}
	}
	if strings.TrimSpace(summary) != "" {
		b.WriteString("\n## Agent's summary (not verified)\n\n" + tracker.Fence(summary) + "\n")
	}
	b.WriteString("\n---\n" + footer + "\n")
	return b.String()
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}

type flagLine struct{ rule, path, detail string }
