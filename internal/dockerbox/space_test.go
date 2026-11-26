package dockerbox

import (
	"context"
	"os"
	"testing"
)

func TestParseDF(t *testing.T) {
	out := "Filesystem 1024-blocks Used Available Capacity Mounted on\noverlay 61202244 58000000 3202244 95% /\n"
	if got, err := parseDF(out); err != nil || got != 3202244*1024 {
		t.Fatalf("got %d %v", got, err)
	}
	for _, bad := range []string{"", "Filesystem 1024-blocks Used Available Capacity Mounted on", "x\noverlay a b c d /"} {
		if _, err := parseDF(bad); err == nil {
			t.Errorf("no error for %q", bad)
		}
	}
}

func TestReclaimedSpace(t *testing.T) {
	for out, want := range map[string]string{
		"ID\tRECLAIMABLE\nabc\ttrue\nTotal:\t2.3GB": "2.3GB",
		"Total reclaimed space: 512MB":              "512MB",
	} {
		if m := reclaimedRE.FindStringSubmatch(out); m == nil || m[1] != want {
			t.Errorf("%q: %v", out, m)
		}
	}
}

func TestFreeSpaceAndPruneAskTheEngine(t *testing.T) {
	b, log := scriptDocker(t, `case "$1" in
run) printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\noverlay 100 90 10 90%% /\n';;
builder) printf 'ID\tRECLAIMABLE\tSIZE\nabc\ttrue\t1GB\nTotal:\t1.2GB\n';;
esac`)
	b.Image = "maestro-sandbox:1"
	if free, err := b.FreeSpace(context.Background()); err != nil || free != 10*1024 {
		t.Fatalf("free %d %v", free, err)
	}
	if freed, err := b.PruneBuildCache(context.Background()); err != nil || freed != "1.2GB" {
		t.Fatalf("freed %q %v", freed, err)
	}
	args, _ := os.ReadFile(log)
	want := "run --rm --network none --entrypoint df maestro-sandbox:1 -Pk /\nbuilder prune --force --filter until=168h\n"
	if string(args) != want {
		t.Errorf("args:\n%s", args)
	}
	b.Podman = true
	if freed, err := b.PruneBuildCache(context.Background()); err != nil || freed != "" {
		t.Errorf("podman: %q %v", freed, err)
	}
}
