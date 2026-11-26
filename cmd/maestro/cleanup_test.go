package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/cleanup"
)

func TestWriteCleanup(t *testing.T) {
	var b bytes.Buffer
	writeCleanup(&b, cleanup.Report{DryRun: true,
		Removed: []cleanup.Item{{Kind: "branch", Name: "maestro/GH-1", Reason: "the ticket is Done"},
			{Kind: "audit", Name: "GH-1.jsonl", Reason: "older than 30 days", Bytes: 3 << 20}},
		Kept:     []cleanup.Item{{Kind: "branch", Name: "maestro/GH-2", Reason: "open pull request"}},
		Problems: []string{"ticket GH-3 could not be read\x1b[31m"}})
	out := b.String()
	for _, want := range []string{"Would remove:", "maestro/GH-1: the ticket is Done", "3.0 MiB", "Kept:", "maestro/GH-2", "Run it without --dry-run"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Error("a control character reached the terminal")
	}

	b.Reset()
	writeCleanup(&b, cleanup.Report{})
	if !strings.Contains(b.String(), "Nothing to clean up.") {
		t.Errorf("empty report: %s", b.String())
	}
}

func TestCleanupUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"cleanup", "extra"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "usage: maestro cleanup") {
		t.Fatalf("code %d, %s", code, errb.String())
	}
}
