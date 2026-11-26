package dockerbox

import (
	"context"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// LowDisk is the free space below which the engine's disk counts as
// nearly full. A Unity image alone takes several gigabytes to unpack, and
// CI on the same engine needs room too.
const LowDisk = 20 << 30

// ErrNoSpace is FreeSpace's answer when the engine could not even start a
// container because its disk is full.
var ErrNoSpace = errors.New("no space left on device")

// FreeSpace is how many bytes are free where the container engine keeps
// images and containers. On a Mac that is the disk of the engine's virtual
// machine, not the Mac's own, and it fills up quietly: a large image such
// as Unity's then fails to unpack with "no space left on device". It runs
// df in a throwaway container of the sandbox image, with no network.
func (b *Box) FreeSpace(ctx context.Context) (int64, error) {
	args := append([]string{"run", "--rm", "--network", "none", "--entrypoint", "df"}, b.engineFlags()...)
	out, err := b.cli(ctx, append(args, b.Image, "-Pk", "/")...)
	if err != nil {
		if strings.Contains(err.Error(), "no space left on device") {
			return 0, ErrNoSpace
		}
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

// PruneBuildCache removes the engine's build cache that no build refers
// to any more, and the rest of it that was not used within keep (0: all
// of it that no running build uses). Building the sandbox images leaves
// it behind, and on a Mac mini it grew to twice the size of the images.
// It is a cache, so a later build is only slower. It answers how many
// bytes were freed, as far as the engine said. Podman keeps no such
// cache, so it does nothing there.
func (b *Box) PruneBuildCache(ctx context.Context, keep time.Duration) (int64, error) {
	if b.Podman {
		return 0, nil
	}
	var freed int64
	runs := [][]string{{"builder", "prune", "--force"}}
	all := []string{"builder", "prune", "--force", "--all"}
	if keep > 0 {
		all = append(all, "--filter", "until="+strconv.Itoa(int(keep.Hours()))+"h")
	}
	runs = append(runs, all)
	for _, args := range runs {
		out, err := b.cli(ctx, args...)
		if err != nil {
			return freed, err
		}
		if m := reclaimedRE.FindStringSubmatch(out); m != nil {
			freed += parseSize(m[1])
		}
	}
	return freed, nil
}

// parseSize reads a size as Docker writes it, such as 17.8GB or 512kB, in
// powers of 1000. Anything else is 0.
func parseSize(s string) int64 {
	i := strings.IndexFunc(s, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	if i <= 0 {
		return 0
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0
	}
	mult := map[string]float64{"B": 1, "kB": 1e3, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12}[s[i:]]
	return int64(math.Round(n * mult))
}
