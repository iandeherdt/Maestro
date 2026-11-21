// Package mirror imports the agent's commits into the control zone's bare
// repository and computes the diff the policy judges (SPEC sections 15.1
// and 15.2).
//
// Everything that arrives here was written by the sandbox, so it is treated
// as hostile: the bundle file may be a link or a device, the history may be
// unrelated, and paths and commit messages may contain anything. Nothing is
// checked out and nothing from the workspace is executed.
package mirror

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/iandeherdt/Symphony-beads/internal/policy"
)

// Rejected is returned when the hand-off itself is not acceptable. It
// counts as a failure attempt (category import_rejected). The reason never
// contains content from the bundle.
type Rejected struct{ Reason string }

func (r *Rejected) Error() string { return "import rejected: " + r.Reason }

func reject(format string, a ...any) error { return &Rejected{Reason: fmt.Sprintf(format, a...)} }

// Mirror is a bare repository owned by the control zone.
type Mirror struct {
	// Dir is the bare repository.
	Dir string
	// Git is the git binary. Default "git".
	Git string
	// OwnerUID, when not negative, is the only user a bundle may belong to.
	OwnerUID int
	// MaxBundleBytes bounds the bundle file.
	MaxBundleBytes int64
	// MaxCommits bounds the number of new commits. Default 500.
	MaxCommits int
}

var (
	shaRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	keyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// IncomingRef is the ref an imported head is stored under.
func IncomingRef(workspaceKey string) string { return "refs/symphony/incoming/" + workspaceKey }

// git runs one git command in the mirror with a configuration that takes
// nothing from the machine or the user and can execute nothing.
func (m *Mirror) git(ctx context.Context, stdin io.Reader, limit int64, args ...string) ([]byte, error) {
	bin := m.Git
	if bin == "" {
		bin = "git"
	}
	full := append([]string{
		"--git-dir=" + m.Dir, "--literal-pathspecs", "--no-pager", "--no-optional-locks",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "core.attributesFile=/dev/null",
		"-c", "protocol.allow=never",
		"-c", "transfer.fsckObjects=true",
		"-c", "fetch.fsckObjects=true",
		"-c", "fetch.recurseSubmodules=false",
		"-c", "gc.auto=0",
		"-c", "maintenance.auto=false",
		"-c", "diff.external=",
		"-c", "core.pager=cat",
		"-c", "credential.helper=",
	}, args...)
	cmd := exec.CommandContext(ctx, bin, full...)
	cmd.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/nonexistent", "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_ATTR_NOSYSTEM=1", "GIT_ASKPASS=/bin/false", "GIT_NO_REPLACE_OBJECTS=1",
	}
	cmd.Stdin = stdin
	out := &capped{limit: limit}
	var stderr capped
	stderr.limit = 4096
	cmd.Stdout, cmd.Stderr = out, &stderr
	err := cmd.Run()
	if err != nil {
		name := args[0]
		if name == "-c" && len(args) > 2 {
			name = args[2]
		}
		return out.buf.Bytes(), &gitError{args: name, err: err}
	}
	return out.buf.Bytes(), nil
}

// gitError deliberately leaves out git's output: it can quote ref names and
// paths chosen by the agent.
type gitError struct {
	args string
	err  error
}

func (e *gitError) Error() string { return "git " + e.args + ": " + e.err.Error() }
func (e *gitError) Unwrap() error { return e.err }

func exited(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee)
}

// capped keeps at most limit bytes and counts the rest.
type capped struct {
	buf   bytes.Buffer
	limit int64 // 0 means 64 MiB
	over  bool
}

func (c *capped) Write(p []byte) (int, error) {
	limit := c.limit
	if limit == 0 {
		limit = 64 << 20
	}
	if room := limit - int64(c.buf.Len()); int64(len(p)) > room {
		c.over = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

// Imported describes the commits that were taken in.
type Imported struct {
	Head    string
	Commits []policy.Commit // oldest first
}

// Import copies the bundle into the control zone, verifies it and stores
// its head under IncomingRef(workspaceKey).
func (m *Mirror) Import(ctx context.Context, bundlePath, workspaceKey, baseSHA string) (Imported, error) {
	var none Imported
	if !keyRE.MatchString(workspaceKey) || strings.Contains(workspaceKey, "..") || strings.HasSuffix(workspaceKey, ".lock") {
		return none, fmt.Errorf("invalid workspace key")
	}
	if !shaRE.MatchString(baseSHA) {
		return none, fmt.Errorf("invalid base commit")
	}
	if m.MaxBundleBytes <= 0 {
		return none, fmt.Errorf("MaxBundleBytes is not set")
	}

	tmp, err := os.MkdirTemp(m.Dir, "incoming-")
	if err != nil {
		return none, err
	}
	defer os.RemoveAll(tmp)
	local := filepath.Join(tmp, "in.bundle")
	if err := m.copyBundle(bundlePath, local); err != nil {
		return none, err
	}

	if _, err := m.git(ctx, nil, 0, "bundle", "verify", "--quiet", local); err != nil {
		if exited(err) {
			return none, reject("the bundle does not verify against the mirror")
		}
		return none, err
	}
	ref := IncomingRef(workspaceKey)
	// The leading + lets a later attempt replace an earlier head. Git
	// treats a bundle as the "file" transport, so that one transport is
	// allowed for this one command. The source is the service's own copy.
	if _, err := m.git(ctx, nil, 0, "-c", "protocol.file.allow=always", "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules",
		local, "+HEAD:"+ref); err != nil {
		if exited(err) {
			return none, reject("the bundle could not be fetched or failed object checks")
		}
		return none, err
	}
	out, err := m.git(ctx, nil, 0, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		if exited(err) {
			return none, reject("the bundle head is not a commit")
		}
		return none, err
	}
	head := strings.TrimSpace(string(out))
	fail := func(err error) (Imported, error) {
		// Do not leave a rejected head where a later step could pick it up.
		_, _ = m.git(context.WithoutCancel(ctx), nil, 0, "update-ref", "-d", ref)
		return none, err
	}
	if !shaRE.MatchString(head) {
		return fail(reject("the bundle head is not a commit"))
	}
	if head == baseSHA {
		return fail(reject("there are no new commits"))
	}
	if _, err := m.git(ctx, nil, 0, "merge-base", "--is-ancestor", baseSHA, head); err != nil {
		if exited(err) {
			return fail(reject("the commits do not build on the base commit"))
		}
		return fail(err)
	}

	maxCommits := m.MaxCommits
	if maxCommits <= 0 {
		maxCommits = 500
	}
	// One record per commit: the commit, its parents, then the identity
	// and the message. The message is last, so it may contain anything
	// but NUL, which a commit cannot contain.
	out, err = m.git(ctx, nil, 0, "log", "-z", "--reverse", "--max-count="+strconv.Itoa(maxCommits+1),
		"--format=%H%x1f%P%x1f%an%x1f%ae%x1f%cn%x1f%ce%x1f%B", baseSHA+".."+head, "--")
	if err != nil {
		return fail(err)
	}
	var commits []policy.Commit
	for _, rec := range bytes.Split(bytes.TrimSuffix(out, []byte{0}), []byte{0}) {
		f := strings.SplitN(string(rec), "\x1f", 7)
		if len(f) != 7 || !shaRE.MatchString(f[0]) {
			return fail(reject("a commit record could not be read"))
		}
		// Exactly one parent: no merge can bring in foreign history, and
		// no new root commit can start an unrelated one.
		if len(strings.Fields(f[1])) != 1 {
			return fail(reject("the new commits are not a single line of history"))
		}
		commits = append(commits, policy.Commit{SHA: f[0], AuthorName: f[2], AuthorEmail: f[3],
			CommitterName: f[4], CommitterEmail: f[5], Message: f[6]})
	}
	if len(commits) == 0 {
		return fail(reject("there are no new commits"))
	}
	if len(commits) > maxCommits {
		return fail(reject("more than %d new commits", maxCommits))
	}
	return Imported{Head: head, Commits: commits}, nil
}

// copyBundle copies the hand-off file without following a link and without
// trusting its size to stay what it was.
func (m *Mirror) copyBundle(src, dst string) error {
	// O_NONBLOCK: opening a FIFO planted in place of the bundle must not
	// hang the service.
	fd, err := syscall.Open(src, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return reject("no bundle was handed over")
		}
		return reject("the bundle cannot be opened as a regular file")
	}
	in := os.NewFile(uintptr(fd), "bundle")
	defer in.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return reject("the bundle is not a regular file")
	}
	if m.OwnerUID >= 0 && int(st.Uid) != m.OwnerUID {
		return reject("the bundle is not owned by the sandbox user")
	}
	if st.Size > m.MaxBundleBytes {
		return reject("the bundle is larger than %d bytes", m.MaxBundleBytes)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, m.MaxBundleBytes+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n > m.MaxBundleBytes {
		return reject("the bundle is larger than %d bytes", m.MaxBundleBytes)
	}
	return nil
}

// Limits bound how much of the diff is read. They are the policy's limits:
// beyond them the policy denies anyway, so nothing more is loaded.
type Limits struct {
	MaxChangedFiles int64
	MaxDiffBytes    int64
	MaxFileBytes    int64
}

// Diff is the change between two commits in the form the policy reads.
type Diff struct {
	Changes   []policy.Change
	BasePaths []string
}

// Diff computes the change between base and head from objects alone.
// Renames are not detected: a rename is a deletion and an addition, so the
// new file is scanned in full.
func (m *Mirror) Diff(ctx context.Context, baseSHA, head string, lim Limits) (Diff, error) {
	var d Diff
	if !shaRE.MatchString(baseSHA) || !shaRE.MatchString(head) {
		return d, fmt.Errorf("invalid commit id")
	}
	out, err := m.git(ctx, nil, 0, "ls-tree", "-r", "-z", "--name-only", baseSHA)
	if err != nil {
		return d, err
	}
	for _, p := range bytes.Split(bytes.TrimSuffix(out, []byte{0}), []byte{0}) {
		if len(p) > 0 {
			d.BasePaths = append(d.BasePaths, string(p))
		}
	}

	out, err = m.git(ctx, nil, 0, "diff-tree", "-r", "-z", "--raw", "--no-renames", "--no-abbrev", "--no-ext-diff",
		"--no-textconv", baseSHA, head, "--")
	if err != nil {
		return d, err
	}
	type entry struct {
		newSHA string
		idx    int
	}
	var blobs []entry
	fields := bytes.Split(bytes.TrimSuffix(out, []byte{0}), []byte{0})
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(string(fields[i]), ":"))
		if len(meta) != 5 || len(meta[4]) != 1 {
			return d, fmt.Errorf("unexpected diff-tree output")
		}
		oldMode, err1 := strconv.ParseUint(meta[0], 8, 32)
		newMode, err2 := strconv.ParseUint(meta[1], 8, 32)
		if err1 != nil || err2 != nil {
			return d, fmt.Errorf("unexpected diff-tree output")
		}
		c := policy.Change{Path: string(fields[i+1]), Status: meta[4][0], OldMode: uint32(oldMode), NewMode: uint32(newMode)}
		d.Changes = append(d.Changes, c)
		if c.Status != 'D' && c.NewMode != policy.ModeGitlink && c.NewMode != 0 {
			blobs = append(blobs, entry{meta[3], len(d.Changes) - 1})
		}
	}
	if len(fields) == 1 && len(fields[0]) == 0 {
		return d, nil
	}
	// Over the file limit the policy denies on the count alone.
	if int64(len(d.Changes)) > lim.MaxChangedFiles {
		return d, nil
	}

	// Sizes, in one process.
	var ids bytes.Buffer
	for _, b := range blobs {
		ids.WriteString(b.newSHA + "\n")
	}
	out, err = m.git(ctx, &ids, 0, "cat-file", "--batch-check=%(objecttype) %(objectsize)")
	if err != nil {
		return d, err
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(blobs) > 0 && len(lines) != len(blobs) {
		return d, fmt.Errorf("unexpected cat-file output")
	}
	for i, b := range blobs {
		var typ string
		var size int64
		if _, err := fmt.Sscanf(lines[i], "%s %d", &typ, &size); err != nil || typ != "blob" {
			return d, fmt.Errorf("unexpected cat-file output")
		}
		d.Changes[b.idx].NewSize = size
	}

	// Content. --text shows every file as text, so no attribute in the
	// repository can hide lines from the scan. Binary files are detected
	// here instead, the way git does it: a NUL byte near the start.
	budget := lim.MaxDiffBytes + 1
	for i := range d.Changes {
		c := &d.Changes[i]
		if c.NewMode == policy.ModeGitlink || c.OldMode == policy.ModeGitlink {
			continue
		}
		if c.Status != 'D' && c.NewSize > lim.MaxFileBytes {
			c.DiffBytes = c.NewSize // denied on size; the content is not loaded
			continue
		}
		if budget <= 0 {
			c.DiffBytes = 1 // keeps the total over the limit without reading more
			continue
		}
		patch, err := m.git(ctx, nil, budget, "diff-tree", "-r", "-p", "--text", "-U0", "--no-renames", "--no-ext-diff",
			"--no-textconv", "--no-color", "--full-index", baseSHA, head, "--", c.Path)
		if err != nil {
			return d, err
		}
		c.DiffBytes = int64(len(patch))
		budget -= c.DiffBytes
		body := patch
		if at := bytes.Index(patch, []byte("\n@@ ")); at >= 0 {
			body = patch[at+1:]
		} else {
			body = nil
		}
		head8k := body
		if len(head8k) > 8000 {
			head8k = head8k[:8000]
		}
		if bytes.IndexByte(head8k, 0) >= 0 {
			c.Binary = true
			continue
		}
		for _, line := range strings.Split(string(body), "\n") {
			switch {
			case strings.HasPrefix(line, "+"):
				c.Added = append(c.Added, line[1:])
			case strings.HasPrefix(line, "-"):
				c.Removed = append(c.Removed, line[1:])
			}
		}
	}
	return d, nil
}
