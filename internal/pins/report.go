package pins

import (
	"fmt"
	"strings"
)

// Outdated counts the results with a newer release.
func Outdated(results []Result) int {
	n := 0
	for _, r := range results {
		if r.Err == nil && r.Latest != "" {
			n++
		}
	}
	return n
}

// Failed counts the lookups that did not get an answer.
func Failed(results []Result) int {
	n := 0
	for _, r := range results {
		if r.Err != nil {
			n++
		}
	}
	return n
}

// Report writes the results as Markdown for an issue. repoURL and ref,
// when set, make the places links.
func Report(results []Result, repoURL, ref string) string {
	var b strings.Builder
	b.WriteString("The scheduled check of the tool versions that the sandbox image and the convention packs pin.\n\n")
	if Outdated(results) > 0 {
		b.WriteString("## Newer releases\n\n| Tool | Pinned | Newest | Where |\n|---|---|---|---|\n")
		for _, r := range results {
			if r.Err != nil || r.Latest == "" {
				continue
			}
			latest := "`" + r.Latest + "`"
			if r.Line != "" {
				latest = "`" + r.Line + "` in the pinned line, " + latest + " overall"
			}
			if r.Note != "" {
				latest += " (" + r.Note + ")"
			}
			fmt.Fprintf(&b, "| %s | `%s` | %s | %s |\n", r.Pin.Kind+" "+r.Pin.Name, r.Pin.Version, latest, places(r.Pin, repoURL, ref))
		}
		b.WriteString("\nA pinned version is moved on purpose: try the new release in the sandbox, then change every place listed in one pull request.\n")
	} else {
		b.WriteString("Every pin is on its newest release.\n")
	}
	if Failed(results) > 0 {
		b.WriteString("\n## Not checked\n\n")
		for _, r := range results {
			if r.Err != nil {
				fmt.Fprintf(&b, "- %s %s `%s`: %v\n", r.Pin.Kind, r.Pin.Name, r.Pin.Version, r.Err)
			}
		}
	}
	b.WriteString("\n<details><summary>Every pin checked</summary>\n\n")
	for _, r := range results {
		state := "current"
		switch {
		case r.Err != nil:
			state = "not checked"
		case r.Line != "":
			state = "newer: " + r.Line + " in the pinned line, " + r.Latest + " overall"
		case r.Latest != "":
			state = "newer: " + r.Latest
		}
		fmt.Fprintf(&b, "- %s %s `%s`: %s\n", r.Pin.Kind, r.Pin.Name, r.Pin.Version, state)
	}
	b.WriteString("\n</details>\n")
	return b.String()
}

func places(p Pin, repoURL, ref string) string {
	var out []string
	for _, pl := range p.Places {
		label := fmt.Sprintf("%s:%d", pl.File, pl.Line)
		if repoURL != "" && ref != "" {
			label = fmt.Sprintf("[%s](%s/blob/%s/%s#L%d)", label, repoURL, ref, pl.File, pl.Line)
		}
		out = append(out, label)
	}
	return strings.Join(out, "<br>")
}
