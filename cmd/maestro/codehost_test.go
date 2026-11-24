package main

import (
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/gitlab"
)

func TestGitLabUnguarded(t *testing.T) {
	good := gitlab.Protection{Protected: true, Push: 0, Merge: gitlab.Maintainer}
	if note := gitLabUnguarded(good, "https://gitlab.com", "g/app", "main"); note != "" {
		t.Fatalf("a well protected branch gets a note:\n%s", note)
	}
	for name, p := range map[string]gitlab.Protection{
		"unprotected":         {},
		"developers push":     {Protected: true, Push: gitlab.Developer, Merge: gitlab.Maintainer},
		"developers merge":    {Protected: true, Merge: gitlab.Developer},
		"force push allowed":  {Protected: true, Merge: gitlab.Maintainer, AllowForcePush: true},
		"bot allowed to push": {Protected: true, Merge: gitlab.Maintainer, UserCanPush: true},
	} {
		note := gitLabUnguarded(p, "https://gitlab.com", "g/app", "main")
		if !strings.Contains(note, "https://gitlab.com/g/app/-/settings/repository") {
			t.Errorf("%s: the note does not say where to protect the branch:\n%s", name, note)
		}
	}
}
