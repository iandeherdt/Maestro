package publish

import (
	"fmt"
	"strings"
	"testing"
)

func TestFailedDetailNamesTheChecks(t *testing.T) {
	if got := failedDetail([]string{"build", "test\u202e\x1b[31m"}); got != "2 required check(s) failed on the pull request: build, test[31m" {
		t.Errorf("got %q", got)
	}
	var many []string
	for i := range 12 {
		many = append(many, fmt.Sprintf("job-%02d", i))
	}
	got := failedDetail(many)
	if !strings.HasSuffix(got, "job-09 and 2 more") || strings.Contains(got, "job-10") {
		t.Errorf("got %q", got)
	}
}

func TestChecksPages(t *testing.T) {
	gh := &gitHub{web: "https://github.com", owner: "org", repo: "game"}
	gl := &gitLab{web: "https://gitlab.com", path: "group/game"}
	gt := &giteaHost{web: "https://git.example.com", owner: "org", repo: "game"}
	for got, want := range map[string]string{
		gh.checksPage(12): "https://github.com/org/game/pull/12/checks",
		gl.checksPage(12): "https://gitlab.com/group/game/-/merge_requests/12/pipelines",
		gt.checksPage(12): "https://git.example.com/org/game/pulls/12/checks",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if webLink("/tmp/remotes/org/game/pull/1/checks") != "" {
		t.Error("a local path became a link")
	}
}
