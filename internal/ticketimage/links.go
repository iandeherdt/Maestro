package ticketimage

import (
	"html"
	"regexp"
	"strings"
)

var (
	imgTagRE = regexp.MustCompile(`(?is)<img\b[^>]*?\bsrc\s*=\s*("([^"]*)"|'([^']*)')`)
	urlRE    = regexp.MustCompile(`https://[^\s()<>"'\[\]{}|\\^` + "`" + `]+`)
)

// HTMLImages returns the src of every img tag in html, in order. It is for
// HTML a board rendered and cleaned itself, such as GitHub's body_html.
func HTMLImages(s string) []string {
	var out []string
	for _, m := range imgTagRE.FindAllStringSubmatch(s, -1) {
		src := m[2]
		if src == "" {
			src = m[3]
		}
		if src = strings.TrimSpace(html.UnescapeString(src)); src != "" {
			out = append(out, src)
		}
	}
	return out
}

// Links returns every https link in markdown text, in order, with trailing
// punctuation that belongs to the sentence taken off.
func Links(s string) []string {
	var out []string
	for _, u := range urlRE.FindAllString(s, -1) {
		if u = strings.TrimRight(u, ".,;:!?*_~"); u != "" {
			out = append(out, u)
		}
	}
	return out
}
