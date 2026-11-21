package issue

import (
	"strings"
	"testing"
	"time"
)

func TestWorkspaceKey(t *testing.T) {
	for in, want := range map[string]string{
		"GH-123":  "GH-123",
		"ABC_1.2": "ABC_1.2",
	} {
		if got, err := WorkspaceKey(in); err != nil || got != want {
			t.Errorf("WorkspaceKey(%q) = %q, %v", in, got, err)
		}
	}
	a, _ := WorkspaceKey("team/ABC 1")
	b, _ := WorkspaceKey("team_ABC/1")
	if !strings.HasPrefix(a, "team_ABC_1-") || len(a) != len("team_ABC_1-")+16 {
		t.Errorf("sanitized key = %q", a)
	}
	if a == b {
		t.Error("identifiers that sanitize alike share a key")
	}
	if strings.ContainsAny(a, "/ ") {
		t.Errorf("unsafe character survived: %q", a)
	}
	for _, bad := range []string{"", ".", "..", "-rf", ".git", ".github", ".gitmodules"} {
		if _, err := WorkspaceKey(bad); err == nil {
			t.Errorf("WorkspaceKey(%q) accepted", bad)
		}
	}
	// A path traversal attempt is flattened, never passed through.
	if k, err := WorkspaceKey("../../etc"); err != nil || strings.Contains(k, "/") || strings.HasPrefix(k, "..") && !strings.HasPrefix(k, ".._") {
		t.Errorf("traversal key = %q, %v", k, err)
	}
}

func TestSortForDispatch(t *testing.T) {
	p := func(n int) *int { return &n }
	at := func(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }
	in := []Issue{
		{Identifier: "E", Priority: p(0), CreatedAt: at(1)},
		{Identifier: "D", CreatedAt: at(1)},
		{Identifier: "C", Priority: p(2), CreatedAt: at(3)},
		{Identifier: "B", Priority: p(2), CreatedAt: at(2)},
		{Identifier: "A", Priority: p(4), CreatedAt: at(1)},
		{Identifier: "F", Priority: p(2)},
		{Identifier: "B2", Priority: p(2), CreatedAt: at(2)},
	}
	SortForDispatch(in)
	var got []string
	for _, i := range in {
		got = append(got, i.Identifier)
	}
	if s := strings.Join(got, " "); s != "B B2 C F A D E" {
		t.Errorf("order = %s", s)
	}
}

func TestContentHashSeparatesFields(t *testing.T) {
	if ContentHash("ab", "c") == ContentHash("a", "bc") {
		t.Error("title and description are not separated")
	}
}
