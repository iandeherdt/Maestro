package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/background"
)

// releases is where `maestro update` downloads from. Tests replace it.
var releases = "https://github.com/iandeherdt/Maestro/releases"

var tagRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// maxArchive bounds what is downloaded and unpacked.
const maxArchive = 200 << 20

// runUpdate is `maestro update`: it does what install.sh does, from
// inside the binary (download the release for this machine, check it
// against SHA256SUMS, replace this binary), and then restarts the
// background services that run this binary, so they use the new one.
func runUpdate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	want := fs.String("version", "", "a release tag such as v0.5.19 (default: the latest)")
	noRestart := fs.Bool("no-restart", false, "replace the binary, but leave the background service running the old one")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: maestro update [--version vX.Y.Z] [--no-restart]")
		return 2
	}
	if *want != "" && !tagRE.MatchString(*want) {
		fmt.Fprintln(stderr, "--version must look like v1.2.3")
		return 2
	}
	ctx, stop := interruptible()
	defer stop()
	hc := &http.Client{Timeout: 5 * time.Minute}

	tag := *want
	if tag == "" {
		var err error
		if tag, err = latestTag(ctx, hc, releases); err != nil {
			return report(stderr, fmt.Errorf("finding the latest release: %w", err))
		}
	}
	if tag == version && *want == "" {
		fmt.Fprintf(stdout, "maestro %s is the latest release. Nothing to do.\n", version)
		return 0
	}
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stdout, "Downloading maestro %s (this is %s).\n", tag, version)
	bin, err := download(ctx, hc, releases+"/download/"+tag, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return report(stderr, fmt.Errorf("%w. Nothing was changed", err))
	}
	if err := replaceBinary(self, bin); err != nil {
		return report(stderr, fmt.Errorf("replacing %s: %w", self, err))
	}
	fmt.Fprintf(stdout, "Checksum ok. %s is now maestro %s.\n", self, tag)
	if *noRestart {
		fmt.Fprintln(stdout, "The background service keeps running the old version until it restarts.")
		return 0
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return report(stderr, err)
	}
	restarted, err := background.Restart(ctx, runtime.GOOS, home, os.Getuid(), self, nil)
	for _, l := range restarted {
		fmt.Fprintf(stdout, "Restarted the background service %s.\n", l)
	}
	if err != nil {
		return report(stderr, err)
	}
	if len(restarted) == 0 {
		fmt.Fprintln(stdout, "No background service runs this binary. A `maestro run` in a terminal needs a restart by hand.")
	} else {
		fmt.Fprintln(stdout, "An agent turn that was running was stopped with it. `maestro status` shows what the service does now.")
	}
	return 0
}

// latestTag reads the tag the "latest" release redirects to.
func latestTag(ctx context.Context, hc *http.Client, base string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/latest", nil)
	if err != nil {
		return "", err
	}
	c := *hc
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := c.Do(req)
	if err != nil {
		return "", err
	}
	res.Body.Close()
	loc, err := res.Location()
	if err != nil {
		return "", fmt.Errorf("GitHub answered %d without naming a release", res.StatusCode)
	}
	tag, err := url.PathUnescape(loc.Path[strings.LastIndex(loc.Path, "/")+1:])
	if err != nil || !tagRE.MatchString(tag) {
		return "", fmt.Errorf("the latest release is %q, which is not a version", loc.Path)
	}
	return tag, nil
}

func fetch(ctx context.Context, hc *http.Client, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: GitHub answered %d", u, res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxArchive+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxArchive {
		return nil, fmt.Errorf("%s is larger than expected", u)
	}
	return b, nil
}

// download returns the maestro binary of the release at url for this
// machine, after checking the archive against the release's SHA256SUMS.
func download(ctx context.Context, hc *http.Client, url, goos, goarch string) ([]byte, error) {
	if goos != "darwin" && goos != "linux" {
		return nil, errors.New("releases are built for macOS and Linux only")
	}
	archive := "maestro_" + goos + "_" + goarch + ".tar.gz"
	sums, err := fetch(ctx, hc, url+"/SHA256SUMS")
	if err != nil {
		return nil, fmt.Errorf("the checksum file could not be downloaded: %w", err)
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == archive {
			want = strings.ToLower(f[0])
		}
	}
	if want == "" {
		return nil, fmt.Errorf("the checksum file does not list %s", archive)
	}
	data, err := fetch(ctx, hc, url+"/"+archive)
	if err != nil {
		return nil, fmt.Errorf("the download failed: %w", err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != want {
		return nil, errors.New("the checksum does not match")
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s holds no maestro binary", archive)
		}
		if err != nil {
			return nil, err
		}
		if h.Name == "maestro" && h.Typeflag == tar.TypeReg {
			b, err := io.ReadAll(io.LimitReader(tr, maxArchive+1))
			if err != nil {
				return nil, err
			}
			if len(b) > maxArchive {
				return nil, errors.New("the binary is larger than expected")
			}
			return b, nil
		}
	}
}

// replaceBinary writes bin next to path and renames it over path, so a
// running copy is never half replaced.
func replaceBinary(path string, bin []byte) error {
	tmp := filepath.Join(filepath.Dir(path), ".maestro.new")
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
