package policy

import "strings"

// Match reports whether a slash-separated path matches a pattern. "*" and
// "?" match within one path segment, "**" matches any number of segments.
// A pattern without a slash is matched against every segment's position,
// that is, "*.sh" matches "a/b/run.sh". Matching ignores case, because a
// protected file must stay protected on a case-insensitive checkout.
func Match(pattern, path string) bool {
	pattern, path = strings.ToLower(pattern), strings.ToLower(path)
	if !strings.Contains(pattern, "/") {
		pattern = "**/" + pattern
	}
	return matchSegs(strings.Split(pattern, "/"), strings.Split(path, "/"))
}

func matchSegs(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(segs); i++ {
				if matchSegs(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 || !matchSeg(pat[0], segs[0]) {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

func matchSeg(pat, s string) bool {
	p, n := []rune(pat), []rune(s)
	var rec func(i, j int) bool
	rec = func(i, j int) bool {
		for i < len(p) {
			switch p[i] {
			case '*':
				for k := j; k <= len(n); k++ {
					if rec(i+1, k) {
						return true
					}
				}
				return false
			case '?':
				if j >= len(n) {
					return false
				}
			default:
				if j >= len(n) || p[i] != n[j] {
					return false
				}
			}
			i, j = i+1, j+1
		}
		return j == len(n)
	}
	return rec(0, 0)
}

// MatchAny reports whether path matches one of the patterns.
func MatchAny(patterns []string, path string) bool { return matchAny(patterns, path) }

func matchAny(patterns []string, path string) bool {
	for _, p := range patterns {
		if Match(p, path) {
			return true
		}
	}
	return false
}
