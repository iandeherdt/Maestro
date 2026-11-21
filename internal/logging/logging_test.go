package logging

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestUntrustedTextCannotForgeLines(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo)
	evil := "done\ntime=2026-01-01T00:00:00Z level=ERROR msg=\"dispatch paused\" reason=fake\r\n\x1b[2J"
	log.Info("agent message", Issue("id-1", "GH-1"), "message", evil, "error", errors.New(evil))
	log.Info(evil)
	out := buf.String()
	if n := strings.Count(out, "\n"); n != 2 {
		t.Fatalf("%d lines written, want 2:\n%s", n, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.HasPrefix(line, "time=") || strings.Count(line, " level=INFO ") != 1 {
			t.Errorf("line is not one well-formed event: %q", line)
		}
		if strings.ContainsAny(line, "\r\x1b") {
			t.Errorf("control character survived: %q", line)
		}
	}
	if !strings.Contains(out, "issue_id=id-1 issue_identifier=GH-1") {
		t.Errorf("issue context missing: %s", out)
	}
}

func TestValuesAreCappedAndTimesAreUTC(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo)
	local := time.FixedZone("x", 3600)
	log.Info("m", "big", strings.Repeat("é", 5000), "due", time.Date(2026, 11, 21, 12, 0, 0, 0, local))
	out := buf.String()
	if len(out) > MaxValueBytes+300 {
		t.Fatalf("line is %d bytes", len(out))
	}
	if !strings.Contains(out, "due=2026-11-21T11:00:00.000Z") {
		t.Errorf("time not in UTC: %s", out)
	}
	if !strings.HasSuffix(strings.Fields(out)[0], "Z") {
		t.Errorf("timestamp not in UTC: %s", out)
	}
}

func TestLevelFilter(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, slog.LevelWarn).Info("quiet")
	if buf.Len() != 0 {
		t.Fatal("info logged at warn level")
	}
}
