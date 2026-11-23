package publish

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The sandbox reports the commit a workspace started from, and the agent
// can make it report one of its own commits. Work that was refused once
// must not get through by being compared with itself.
func TestBaseCommitMustBeOnTheBaseBranch(t *testing.T) {
	l := newLab(t)
	refused := l.change(".github/workflows/ci.yml", "on: push\n")
	if _, rej, err := l.pub.Publish(ctx, l.is, l.ws, passed); err != nil || rej == nil || rej.Category != PolicyDenied {
		t.Fatalf("%+v %v", rej, err)
	}
	// The next attempt builds on the refused commit and names it as base.
	full := filepath.Join(l.work, "notes.md")
	if err := os.WriteFile(full, []byte("harmless\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.git(l.work, "add", "-A")
	l.git(l.work, "commit", "-q", "-m", "harmless")
	bundle := filepath.Join(l.root, "handoff", l.ws.Key+".bundle")
	os.Remove(bundle)
	l.git(l.work, "bundle", "create", "-q", bundle, refused+"..HEAD")
	ws := l.ws
	ws.BaseSHA = refused
	rec, rej, err := l.pub.Publish(ctx, l.is, ws, passed)
	if err != nil || rec != nil || rej == nil || rej.Category != ImportRejected ||
		!strings.Contains(rej.Feedback[0].Body, "does not start from a commit of the branch main") {
		t.Fatalf("%+v %+v %v", rec, rej, err)
	}
	if l.remoteHead() != "" || len(l.hub.requests) != 0 {
		t.Errorf("something was published: branch %q, requests %v", l.remoteHead(), l.hub.requests)
	}
}

// A commit that adds a credential and a later one that removes it leave a
// clean diff, and both commits would be pushed.
func TestSecretInAnEarlierCommitIsDenied(t *testing.T) {
	l := newLab(t)
	l.pub.HeldSecrets = func() []string { return []string{token} }
	l.change("notes.md", "the value is "+token+"\n")
	l.git(l.work, "rm", "-q", "notes.md")
	l.git(l.work, "commit", "-q", "-m", "remove it")
	l.change("README.md", "hello again\n")
	_, rej, err := l.pub.Publish(ctx, l.is, l.ws, passed)
	if err != nil || rej == nil || rej.Category != PolicyDenied {
		t.Fatalf("%+v %v", rej, err)
	}
	if len(rej.Feedback) != 1 || !strings.Contains(rej.Feedback[0].Body, "an earlier commit adds something that looks like a credential") ||
		strings.Contains(rej.Feedback[0].Body, token) {
		t.Errorf("feedback %+v", rej.Feedback)
	}
	if l.remoteHead() != "" {
		t.Error("pushed")
	}
}

// A line that starts with "++ " looks like a file header in a patch. It
// is content, and is scanned.
func TestHistoryScanReadsLinesThatLookLikeHeaders(t *testing.T) {
	l := newLab(t)
	l.pub.HeldSecrets = func() []string { return []string{token} }
	l.change("notes.md", "++ b/"+token+"\n")
	l.git(l.work, "rm", "-q", "notes.md")
	l.git(l.work, "commit", "-q", "-m", "remove it")
	l.change("README.md", "hello again\n")
	if _, rej, err := l.pub.Publish(ctx, l.is, l.ws, passed); err != nil || rej == nil || rej.Category != PolicyDenied {
		t.Fatalf("%+v %v", rej, err)
	}
}

func TestSeveralHonestCommitsArePublished(t *testing.T) {
	l := newLab(t)
	l.change("a.txt", "one\n")
	l.change("a.txt", "two\n")
	head := l.change("b.txt", "three\n")
	rec, rej, err := l.pub.Publish(ctx, l.is, l.ws, passed)
	if err != nil || rej != nil || rec == nil || rec.HeadSHA != head {
		t.Fatalf("%+v %+v %v", rec, rej, err)
	}
}

func TestHistoryTooLargeToCheckIsDenied(t *testing.T) {
	l := newLab(t)
	l.cfg.Policy.MaxDiffBytes = 1 << 16 // the history may hold 1 MiB
	big := strings.Repeat("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcde\n", 9000)
	l.change("big.txt", big)
	l.change("big.txt", strings.ToUpper(big))
	l.git(l.work, "rm", "-q", "big.txt")
	l.git(l.work, "commit", "-q", "-m", "remove it")
	l.change("b.txt", "three\n")
	_, rej, err := l.pub.Publish(ctx, l.is, l.ws, passed)
	if err != nil || rej == nil || rej.Category != PolicyDenied || !strings.Contains(rej.Feedback[0].Body, "too much to check one by one") {
		t.Fatalf("%+v %v", rej, err)
	}
}

// What is pushed is what the export step named, which verify ran on.
func TestBundleMustHoldTheExportedCommit(t *testing.T) {
	l := newLab(t)
	head := l.change("a.txt", "one\n")
	l.pub.ExportedHead = func(string) string { return strings.Repeat("0", 40) }
	rec, rej, err := l.pub.Publish(ctx, l.is, l.ws, passed)
	if err != nil || rec != nil || rej == nil || rej.Category != ImportRejected ||
		!strings.Contains(rej.Feedback[0].Body, "does not hold the commit that was exported") {
		t.Fatalf("%+v %+v %v", rec, rej, err)
	}
	l.pub.ExportedHead = func(string) string { return head }
	if rec, rej, err := l.pub.Publish(ctx, l.is, l.ws, passed); err != nil || rej != nil || rec == nil {
		t.Fatalf("%+v %+v %v", rec, rej, err)
	}
}

// Without a base branch in the settings, a commit of any branch that is
// not the service's own counts, and a commit of the agent's still does not.
func TestBaseCommitWithoutANamedBaseBranch(t *testing.T) {
	l := newLab(t)
	l.cfg.Workspace.BaseBranch = ""
	refused := l.change(".github/workflows/ci.yml", "on: push\n")
	if _, rej, err := l.pub.Publish(ctx, l.is, l.ws, passed); err != nil || rej == nil || rej.Category != PolicyDenied {
		t.Fatalf("%+v %v", rej, err)
	}
	ws := l.ws
	ws.BaseSHA = refused
	l.change("notes.md", "harmless\n")
	if _, rej, err := l.pub.Publish(ctx, l.is, ws, passed); err != nil || rej == nil || rej.Category != ImportRejected {
		t.Fatalf("%+v %v", rej, err)
	}
}
