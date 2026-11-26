package dockerbox

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// FreeSpace is how many bytes are free where the container engine keeps
// images and containers. On a Mac that is the disk of the engine's virtual
// machine, not the Mac's own, and it fills up quietly: a large image such
// as Unity's then fails to unpack with "no space left on device". It runs
// df in a throwaway container of the sandbox image, with no network.
func (b *Box) FreeSpace(ctx context.Context) (int64, error) {
	args := append([]string{"run", "--rm", "--network", "none", "--entrypoint", "df"}, b.engineFlags()...)
	out, err := b.cli(ctx, append(args, b.Image, "-Pk", "/")...)
	if err != nil {
		return 0, err
	}
	return parseDF(out)
}

// parseDF reads the free kilobytes of `df -Pk` output with one file
// system.
func parseDF(out string) (int64, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	f := strings.Fields(lines[len(lines)-1])
	if len(lines) < 2 || len(f) < 6 {
		return 0, errors.New("df gave no figures")
	}
	kb, err := strconv.ParseInt(f[3], 10, 64)
	if err != nil {
		return 0, errors.New("df gave no figures")
	}
	return kb * 1024, nil
}

var reclaimedRE = regexp.MustCompile(`(?m)^Total(?: reclaimed space)?:\s*(\S+)\s*$`)

// PruneBuildCache removes the build cache that was not used for
// a week, of every build on the machine: building the sandbox image
// leaves it behind, and it grows with every pack and update. It is a
// cache, so a later build is only slower. It answers how much was freed,
// as the engine wrote it. Podman keeps no such cache, so it does nothing
// there.
func (b *Box) PruneBuildCache(ctx context.Context) (string, error) {
	if b.Podman {
		return "", nil
	}
	out, err := b.cli(ctx, "builder", "prune", "--force", "--filter", "until=168h")
	if err != nil {
		return "", err
	}
	if m := reclaimedRE.FindStringSubmatch(out); m != nil {
		return m[1], nil
	}
	return "", nil
}
