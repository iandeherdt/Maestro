package budget

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMissingFileIsEmpty(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s.Failures("x") != 0 || !s.QuotaBlockedUntil().IsZero() {
		t.Fatal("not empty")
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 21, 23, 30, 0, 0, time.UTC)
	s, _ := Open(dir)
	s.IncFailure("a")
	s.IncFailure("a")
	s.MarkBlocked("a", now)
	s.AddRun(now)
	s.AddUsage(now, 1200, 90)
	s.Pause("maintenance")
	s.SetQuotaBlockedUntil(now.Add(time.Hour))
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, fileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v", info.Mode(), err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.tmp-*")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}

	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	paused, reason := r.Paused()
	d := r.Today(now)
	if r.Failures("a") != 2 || !r.BlockMarked("a") || !paused || reason != "maintenance" ||
		d.Runs != 1 || d.TotalTokens != 1200 || d.AgentSeconds != 90 ||
		!r.QuotaBlockedUntil().Equal(now.Add(time.Hour)) {
		t.Fatalf("record did not survive: %+v", r.rec)
	}
	if next := r.Today(now.Add(time.Hour)); next.Runs != 0 {
		t.Fatal("usage carried into the next day")
	}
	r.ClearIssue("a")
	if r.Failures("a") != 0 || r.BlockMarked("a") {
		t.Fatal("issue not cleared")
	}
}

func TestCorruptFileFailsInsteadOfResetting(t *testing.T) {
	for _, content := range []string{"{not json", `{"failure_attempts": 3}`, `{"surprise": true}`} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, fileName), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir); err == nil {
			t.Errorf("accepted %q", content)
		}
	}
}

func TestOldDaysArePruned(t *testing.T) {
	s, _ := Open(t.TempDir())
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 100; i++ {
		s.AddRun(start.AddDate(0, 0, i))
	}
	if n := len(s.rec.Days); n > keepDays+1 {
		t.Fatalf("days kept = %d", n)
	}
}

func TestTicketTokensAndLastFailurePersistAndClear(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.AddTicketTokens("a", 500)
	s.AddTicketTokens("a", 250)
	s.SetLastFailure("a", "sig")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.TicketTokens("a") != 750 || s.LastFailure("a") != "sig" {
		t.Fatalf("after reopen: tokens %d, last failure %q", s.TicketTokens("a"), s.LastFailure("a"))
	}
	s.ClearIssue("a")
	if s.TicketTokens("a") != 0 || s.LastFailure("a") != "" {
		t.Fatal("ClearIssue kept the tokens or the last failure")
	}
}
