package cleanup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/issue"
)

type fake struct {
	tickets  map[string]string // identifier -> state; absent = gone
	broken   map[string]bool   // lookup fails
	open     map[string]bool   // open pull request on the ticket's branch
	busy     map[string]bool
	branches []string
	spaces   []string
	mirror   []string

	deleted, removed, forgot []string
	compacted                bool
}

func (f *fake) deps() Deps {
	return Deps{
		Lookup: func(_ context.Context, id string) (issue.Issue, bool, error) {
			if id == "foreign" {
				return issue.Issue{}, false, ErrForeign
			}
			if f.broken[id] {
				return issue.Issue{}, false, errors.New("board down")
			}
			st, ok := f.tickets[id]
			if !ok {
				return issue.Issue{}, false, nil
			}
			return issue.Issue{ID: "id-" + id, Identifier: id, State: st}, true, nil
		},
		Finished:     func(is issue.Issue) bool { return is.State == "Done" || is.State == "Canceled" },
		Busy:         func(id string) bool { return f.busy[id] },
		Branches:     func(context.Context) ([]string, error) { return f.branches, nil },
		BranchPrefix: "maestro/", BaseBranch: "main",
		KeepBranches: []string{"maestro/screenshots"},
		OpenChange:   func(_ context.Context, is issue.Issue) (bool, error) { return f.open[is.Identifier], nil },
		DeleteBranch: func(_ context.Context, is issue.Issue) error {
			f.deleted = append(f.deleted, is.Identifier)
			return nil
		},
		Workspaces: func(context.Context) ([]string, error) { return f.spaces, nil },
		RemoveWorkspace: func(_ context.Context, id string) error {
			f.removed = append(f.removed, id)
			return nil
		},
		MirrorKeys: func(context.Context) ([]string, error) { return f.mirror, nil },
		ForgetMirror: func(_ context.Context, k string) error {
			f.forgot = append(f.forgot, k)
			return nil
		},
		CompactMirror: func(context.Context) error { f.compacted = true; return nil },
	}
}

func TestFinishedTicketsAreCleaned(t *testing.T) {
	f := &fake{
		tickets: map[string]string{
			"GH-1": "Done", "GH-2": "In Progress", "GH-3": "Done", "GH-4": "Human Review",
			"GH-6": "Canceled", "GH-7": "Done",
		},
		broken: map[string]bool{"GH-8": true},
		open:   map[string]bool{"GH-3": true},
		busy:   map[string]bool{"GH-7": true},
		branches: []string{"main", "maestro/screenshots", "maestro/GH-1", "maestro/GH-2", "maestro/GH-3",
			"maestro/GH-4", "maestro/GH-5", "maestro/GH-7", "maestro/GH-8", "feature/x", "maestro/a/b", "maestro/foreign"},
		spaces: []string{"GH-1", "GH-2", "GH-5", "GH-6", "GH-7", "GH-8", "GH_9-0123456789abcdef"},
		mirror: []string{"GH-1", "GH-3", "GH-4", "GH-6"},
	}
	r := Run(context.Background(), f.deps(), false)

	// GH-1 done, GH-5 gone: branch deleted. GH-3 has an open pull request.
	if want := []string{"GH-1", "GH-5"}; !slices.Equal(f.deleted, want) {
		t.Errorf("deleted branches %v, want %v", f.deleted, want)
	}
	if want := []string{"GH-1", "GH-5", "GH-6"}; !slices.Equal(f.removed, want) {
		t.Errorf("removed workspaces %v, want %v", f.removed, want)
	}
	// GH-3's branch stays, and so does the lease for its next push.
	if want := []string{"GH-1", "GH-6"}; !slices.Equal(f.forgot, want) {
		t.Errorf("forgot mirror refs %v, want %v", f.forgot, want)
	}
	if !f.compacted {
		t.Error("mirror not compacted")
	}
	if len(r.Kept) != 1 || r.Kept[0].Name != "maestro/GH-3" {
		t.Errorf("kept %+v, want the branch of GH-3", r.Kept)
	}
	if len(r.Problems) != 1 {
		t.Errorf("problems %v, want one for GH-8", r.Problems)
	}
}

func TestDryRunRemovesNothing(t *testing.T) {
	f := &fake{tickets: map[string]string{"GH-1": "Done"}, branches: []string{"maestro/GH-1"}, spaces: []string{"GH-1"}, mirror: []string{"GH-1"}}
	r := Run(context.Background(), f.deps(), true)
	if len(f.deleted)+len(f.removed)+len(f.forgot) > 0 || f.compacted {
		t.Fatalf("dry run changed something: %+v", f)
	}
	if len(r.Removed) != 3 || !r.DryRun {
		t.Fatalf("report %+v, want three items", r)
	}
}

func TestFailedBranchDeleteKeepsMirrorLease(t *testing.T) {
	f := &fake{tickets: map[string]string{"GH-1": "Done"}, branches: []string{"maestro/GH-1"}, mirror: []string{"GH-1"}}
	d := f.deps()
	d.DeleteBranch = func(context.Context, issue.Issue) error { return errors.New("403") }
	r := Run(context.Background(), d, false)
	if len(f.forgot) != 0 {
		t.Fatalf("mirror forgot the lease of a branch that is still there")
	}
	if r.Failed() != 1 {
		t.Fatalf("failed %d, want 1", r.Failed())
	}
}

func TestAuditRetention(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	write := func(name string, size int, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	write("GH-1.jsonl", 10, 40*24*time.Hour) // too old
	write("GH-2.jsonl", 10, 40*24*time.Hour) // too old, but busy
	write("GH-3.jsonl", 60, 3*24*time.Hour)  // oldest of the rest: over size
	write("GH-4.jsonl", 50, 2*24*time.Hour)
	write("GH-5.jsonl", 40, time.Hour)
	write("notes.txt", 10, 90*24*time.Hour) // not an audit file

	d := Deps{AuditDir: dir, AuditMaxBytes: 110, Now: func() time.Time { return now },
		Busy: func(id string) bool { return id == "GH-2" }}
	r := Run(context.Background(), d, false)
	var names []string
	for _, it := range r.Removed {
		names = append(names, it.Name)
	}
	// 10 (GH-2) + 50 + 40 = 100 <= 110 once GH-1 and GH-3 are gone.
	if want := []string{"GH-1.jsonl", "GH-3.jsonl"}; !slices.Equal(names, want) {
		t.Fatalf("removed %v, want %v", names, want)
	}
	left, _ := os.ReadDir(dir)
	if len(left) != 4 {
		t.Fatalf("%d files left, want 4", len(left))
	}
}

func TestImagesInUseAreLeftOut(t *testing.T) {
	d := Deps{
		StaleImages: func(context.Context) ([]Image, error) {
			return []Image{{ID: "a", Name: "a"}, {ID: "b", Name: "b"}}, nil
		},
		RemoveImage: func(_ context.Context, id string) error {
			if id == "b" {
				return ErrKept
			}
			return nil
		},
	}
	r := Run(context.Background(), d, false)
	if len(r.Removed) != 1 || r.Removed[0].Name != "a" {
		t.Fatalf("removed %+v, want only a", r.Removed)
	}
}

func TestMostlyGoneLooksLikeLostAccess(t *testing.T) {
	f := &fake{tickets: map[string]string{"GH-1": "Done"},
		spaces: []string{"GH-1", "GH-2", "GH-3", "GH-4"}}
	r := Run(context.Background(), f.deps(), false)
	if !slices.Equal(f.removed, []string{"GH-1"}) {
		t.Fatalf("removed %v, want only the done ticket's", f.removed)
	}
	if len(r.Problems) != 1 {
		t.Fatalf("problems %v", r.Problems)
	}
}

func TestBuildCacheIsPrunedAndReportedWhenItFreedSomething(t *testing.T) {
	calls := 0
	freed := int64(2_300_000_000)
	d := Deps{PruneBuildCache: func(context.Context) (int64, error) { calls++; return freed, nil }}
	if r := Run(context.Background(), d, true); calls != 0 || len(r.Removed) != 0 {
		t.Fatalf("a dry run pruned: %d %+v", calls, r.Removed)
	}
	r := Run(context.Background(), d, false)
	if calls != 1 || len(r.Removed) != 1 || r.Removed[0].Bytes != freed {
		t.Fatalf("%+v", r.Removed)
	}
	freed = 0
	if r := Run(context.Background(), d, false); len(r.Removed) != 0 {
		t.Fatalf("an empty prune was reported: %+v", r.Removed)
	}
}
