// Package issue holds the normalized work item and the identifier rules
// shared by the scheduler, the workspace manager and the publisher
// (SPEC sections 4.1.1 and 4.2).
package issue

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"
)

// Blocker is another issue that must finish first.
type Blocker struct {
	ID         string
	Identifier string
	State      string
}

// Issue is the normalized tracker record. Zero values stand for "null":
// an empty string, a nil priority, a zero time.
type Issue struct {
	ID          string
	NativeRef   map[string]any
	Identifier  string
	Title       string
	Description string
	Priority    *int
	State       string
	BranchName  string
	URL         string
	AssigneeID  string
	Labels      []string // lowercase
	BlockedBy   []Blocker
	// Dispatchable is derived by the adapter (author allowlist, approval
	// provenance, blockers). The scheduler never sets it.
	Dispatchable bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
	AuthorID     string
	ContentHash  string
}

// NormalizeState trims and lowercases a state name, for comparison only.
func NormalizeState(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// ContentHash is the SHA-256 over the title, a zero byte and the description.
func ContentHash(title, description string) string {
	h := sha256.New()
	h.Write([]byte(title))
	h.Write([]byte{0})
	h.Write([]byte(description))
	return hex.EncodeToString(h.Sum(nil))
}

// HasLabels reports whether the issue carries every label in required.
// Both sides are expected to be normalized already.
func (i Issue) HasLabels(required []string) bool {
	have := make(map[string]bool, len(i.Labels))
	for _, l := range i.Labels {
		have[l] = true
	}
	for _, r := range required {
		if !have[r] {
			return false
		}
	}
	return true
}

// ErrInvalidWorkspaceKey marks an identifier that cannot name a workspace.
var ErrInvalidWorkspaceKey = errors.New("invalid workspace key")

// WorkspaceKey derives the directory and branch component for an identifier.
// Characters outside [A-Za-z0-9._-] become "_". If that changed anything, a
// hash of the original identifier is appended, so that two identifiers which
// sanitize alike still get distinct keys.
func WorkspaceKey(identifier string) (string, error) {
	if identifier == "" {
		return "", ErrInvalidWorkspaceKey
	}
	var b strings.Builder
	changed := false
	for _, r := range identifier {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
			changed = true
		}
	}
	key := b.String()
	if changed {
		sum := sha256.Sum256([]byte(identifier))
		key += "-" + hex.EncodeToString(sum[:])[:16]
	}
	if key == "." || key == ".." || strings.HasPrefix(key, "-") || strings.HasPrefix(key, ".git") {
		return "", ErrInvalidWorkspaceKey
	}
	return key, nil
}

// SortForDispatch orders issues by priority (1..4 ascending, anything else
// after), then oldest first, then identifier.
func SortForDispatch(issues []Issue) {
	rank := func(i Issue) int {
		if i.Priority != nil && *i.Priority >= 1 && *i.Priority <= 4 {
			return *i.Priority
		}
		return 5
	}
	sort.SliceStable(issues, func(a, b int) bool {
		x, y := issues[a], issues[b]
		if rx, ry := rank(x), rank(y); rx != ry {
			return rx < ry
		}
		if !x.CreatedAt.Equal(y.CreatedAt) {
			if x.CreatedAt.IsZero() {
				return false
			}
			if y.CreatedAt.IsZero() {
				return true
			}
			return x.CreatedAt.Before(y.CreatedAt)
		}
		return x.Identifier < y.Identifier
	})
}

// Feedback is one item carried into a retry or rework attempt: a gate
// result, or a comment by an allowlisted human (SPEC 13.3). Body is
// untrusted text in every case.
type Feedback struct {
	Source string // "ci", "policy", "verify", "review" or "tracker"
	Author string
	Body   string
	Path   string // review comments only
	Line   int    // review comments only, 0 when absent
}
