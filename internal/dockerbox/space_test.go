package dockerbox

import (
	"context"
	"os"
	"testing"
	"time"
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
	if freed, err := b.PruneBuildCache(context.Background(), 7*24*time.Hour); err != nil || freed != 2*1_200_000_000 {
		t.Fatalf("freed %d %v", freed, err)
	}
	if _, err := b.PruneBuildCache(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(log)
	want := "run --rm --network none --entrypoint df maestro-sandbox:1 -Pk /\nbuilder prune --force\nbuilder prune --force --all --filter until=168h\n" +
		"builder prune --force\nbuilder prune --force --all\n"
	if string(args) != want {
		t.Errorf("args:\n%s", args)
	}
	b.Podman = true
	if freed, err := b.PruneBuildCache(context.Background(), 0); err != nil || freed != 0 {
		t.Errorf("podman: %d %v", freed, err)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"17.8GB": 17_800_000_000, "512kB": 512_000, "0B": 0, "3MB": 3_000_000, "": 0, "GB": 0, "1.5XB": 0} {
		if got := parseSize(in); got != want {
			t.Errorf("%q: %d", in, got)
		}
	}
}

func TestFreeSpaceOnAFullDisk(t *testing.T) {
	b, _ := scriptDocker(t, `echo 'write /var/lib/docker/x: no space left on device' >&2; exit 125`)
	if _, err := b.FreeSpace(context.Background()); err != ErrNoSpace {
		t.Fatalf("err %v", err)
	}
}
