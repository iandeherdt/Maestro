package main

import (
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/dockerbox"
)

func TestDiskStateSaysSoOnlyWhenLow(t *testing.T) {
	var d diskState
	if d.low() || d.note() != "" {
		t.Fatal("low before any reading")
	}
	if wasLow := d.set(dockerbox.LowDisk + 1); wasLow || d.note() != "" {
		t.Fatal("low with room")
	}
	if wasLow := d.set(3 << 30); wasLow || !d.low() || !strings.Contains(d.note(), "nearly out of disk space (3.0 GiB free)") {
		t.Fatalf("note %q", d.note())
	}
	// Still low: already said, so the watcher stays quiet.
	if wasLow := d.set(2 << 30); !wasLow {
		t.Fatal("a second low reading counts as new")
	}
}
