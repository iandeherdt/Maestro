package pins

import (
	"strconv"
	"strings"
)

// shape splits a version or tag into its numbers and the text between
// them. Two tags with the same text between the numbers are versions of
// the same line: 22-bookworm-slim and 24-bookworm-slim, or
// ubuntu-6000.3.29f1-base-4.0.0 and ubuntu-6000.3.30f1-base-4.0.0, but not
// 22-bookworm-slim and 22.11-bookworm-slim or 6000.4.0a3.
func shape(v string) (skeleton string, nums []int) {
	var sk strings.Builder
	for i := 0; i < len(v); {
		if v[i] >= '0' && v[i] <= '9' {
			j := i
			for j < len(v) && v[j] >= '0' && v[j] <= '9' {
				j++
			}
			n, err := strconv.Atoi(v[i:j])
			if err != nil {
				n = 0
			}
			nums = append(nums, n)
			sk.WriteByte('#')
			i = j
			continue
		}
		sk.WriteByte(v[i])
		i++
	}
	return sk.String(), nums
}

// compareNums orders two lists of numbers; a missing number counts as 0.
func compareNums(a, b []int) int {
	for i := 0; i < max(len(a), len(b)); i++ {
		x, y := 0, 0
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// newest returns the highest candidate of the same shape as current, or ""
// when none is higher. keep, when set, drops candidates.
func newest(current string, candidates []string, keep func(nums []int) bool) string {
	sk, cur := shape(current)
	best, bestNums := "", cur
	for _, c := range candidates {
		csk, nums := shape(c)
		if csk != sk || (keep != nil && !keep(nums)) {
			continue
		}
		if compareNums(nums, bestNums) > 0 {
			best, bestNums = c, nums
		}
	}
	return best
}

// truncate shortens a full version to the precision of a pin: 9 for
// eslint@9, so that 9.39.1 is no update but 10.0.0 is.
func truncate(full, pin string) string {
	_, pn := shape(pin)
	parts := strings.Split(strings.SplitN(full, "-", 2)[0], ".")
	if len(pn) < len(parts) {
		parts = parts[:len(pn)]
	}
	return strings.Join(parts, ".")
}
