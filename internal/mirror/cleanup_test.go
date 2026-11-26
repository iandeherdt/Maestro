package mirror

import (
	"slices"
	"testing"
)

func TestCleanupHelpers(t *testing.T) {
	l := newLab(t)
	remote := l.remote()
	l.importNew("a")
	if err := l.mirror.Push(ctx, remote, "k", "maestro/gh-1", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := l.mirror.Sync(ctx, remote, "tok"); err != nil {
		t.Fatal(err)
	}
	branches, err := l.mirror.Branches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(branches, "main") || !slices.Contains(branches, "maestro/gh-1") {
		t.Fatalf("branches %v", branches)
	}
	keys, err := l.mirror.WorkspaceKeys(ctx)
	if err != nil || !slices.Equal(keys, []string{"k"}) {
		t.Fatalf("keys %v, %v", keys, err)
	}
	if err := l.mirror.ForgetWorkspace(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	// Twice is fine: nothing left to drop.
	if err := l.mirror.ForgetWorkspace(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if err := l.mirror.ForgetWorkspace(ctx, "../x"); err == nil {
		t.Fatal("a bad key was accepted")
	}
	if keys, _ := l.mirror.WorkspaceKeys(ctx); len(keys) != 0 {
		t.Fatalf("keys left: %v", keys)
	}
	if err := l.mirror.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	if !l.mirror.HasBranch(ctx, "main") {
		t.Fatal("compacting lost a branch")
	}
}
