package policy

import (
	"math"
	"regexp"
	"strings"
)

type secretPattern struct {
	name string
	re   *regexp.Regexp
}

var secretPatterns = []secretPattern{
	{"aws_access_key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"github_token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{50,})`)},
	{"gitlab_token", regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`)},
	{"private_key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
	{"slack_token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`)},
	{"google_api_key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"stripe_key", regexp.MustCompile(`\b[sr]k_live_[0-9A-Za-z]{20,}`)},
	{"anthropic_key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{"linear_key", regexp.MustCompile(`\blin_api_[A-Za-z0-9]{30,}`)},
}

var assignmentRE = regexp.MustCompile(`(?i)\b[\w.-]*(?:secret|token|passw(?:or)?d|api[_-]?key|private[_-]?key|credential)[\w.-]*["']?\s*[:=]\s*["']([^"'\s]{16,})["']`)

// minHeldSecret is the shortest held value searched for by exact match.
const minHeldSecret = 8

// scanText returns the names of the secret rules that match text. It never
// returns the matched text itself: findings are shown to the agent and on
// the pull request.
func scanText(text string, held []string) []string {
	var hits []string
	for _, p := range secretPatterns {
		if p.re.MatchString(text) {
			hits = append(hits, p.name)
		}
	}
	for _, m := range assignmentRE.FindAllStringSubmatch(text, -1) {
		if entropy(m[1]) >= 3.5 && !placeholder(m[1]) {
			hits = append(hits, "generic_assignment")
			break
		}
	}
	for _, h := range held {
		if len(h) >= minHeldSecret && strings.Contains(text, h) {
			hits = append(hits, "held_secret")
			break
		}
	}
	return hits
}

// entropy is the Shannon entropy of s in bits per character.
func entropy(s string) float64 {
	if s == "" {
		return 0
	}
	freq := map[rune]float64{}
	n := 0.0
	for _, r := range s {
		freq[r]++
		n++
	}
	e := 0.0
	for _, c := range freq {
		p := c / n
		e -= p * math.Log2(p)
	}
	return e
}

func placeholder(s string) bool {
	l := strings.ToLower(s)
	for _, w := range []string{"example", "placeholder", "changeme", "your-", "your_", "xxxx", "<", "${", "{{", "process.env", "os.environ"} {
		if strings.Contains(l, w) {
			return true
		}
	}
	return false
}
