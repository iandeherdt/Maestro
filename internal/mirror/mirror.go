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

	"github.com/iandeherdt/Maestro/internal/policy"
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
	// AllowFileRemote lets Push use a local path as the remote. Tests only.
	AllowFileRemote bool
}

var (
	shaRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	keyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// IncomingRef is the ref an imported head is stored under.
func IncomingRef(workspaceKey string) string { return "refs/maestro/incoming/" + workspaceKey }

// git runs one git command in the mirror with a configuration that takes
// nothing from the machine or the user and can execute nothing.
func (m *Mirror) git(ctx context.Context, stdin io.Reader, limit int64, args ...string) ([]byte, error) {
	return m.gitEnv(ctx, stdin, limit, nil, args...)
}

func (m *Mirror) gitEnv(ctx context.Context, stdin io.Reader, limit int64, extraEnv []string, args ...string) ([]byte, error) {
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
	// Later entries win, so a caller can replace GIT_ASKPASS.
	cmd.Env = append(cmd.Env, extraEnv...)
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

// gitTo is git with the standard output kept in out, which says whether
// it was cut short.
func (m *Mirror) gitTo(ctx context.Context, out *capped, args ...string) error {
	raw, err := m.gitEnv(ctx, nil, out.limit+1, nil, args...)
	if int64(len(raw)) > out.limit {
		raw, out.over = raw[:out.limit], true
	}
	out.buf.Write(raw)
	return err
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
		// One parent, so that no new root commit can start an unrelated
		// history. The one merge that is allowed is the base branch merged
		// into the work: its second parent is a commit of the base, so it
		// brings in nothing but the owner's own history. The service makes
		// that merge when the base moved on, and an agent commits it after
		// resolving its conflicts.
		switch parents := strings.Fields(f[1]); {
		case len(parents) == 1:
		case len(parents) == 2:
			if _, err := m.git(ctx, nil, 0, "merge-base", "--is-ancestor", parents[1], baseSHA); err != nil {
				if exited(err) {
					return fail(reject("the new commits are not a single line of history: a merge commit brings in commits " +
						"that are not on the base branch. The only merge the work may hold is the base branch merged into it"))
				}
				return fail(err)
			}
		default:
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
	// BinaryPaths are where the owner allows binary files, up to
	// BinaryMaxFileBytes each. Only the start of such a file is read to
	// tell binary from text; a binary file there adds nothing to the diff
	// size. A text file there is read and limited like any other.
	BinaryPaths        []string
	BinaryMaxFileBytes int64
}

// Diff is the change between two commits in the form the policy reads.
type Diff struct {
	Changes   []policy.Change
	BasePaths []string
}

// binarySniff is how much of a file decides whether it is binary: a NUL
// byte in it, the way git decides.
const binarySniff = 8000

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
	blobOf := map[int]string{}
	for _, b := range blobs {
		blobOf[b.idx] = b.newSHA
	}
	for i := range d.Changes {
		c := &d.Changes[i]
		if c.NewMode == policy.ModeGitlink || c.OldMode == policy.ModeGitlink {
			continue
		}
		if sha := blobOf[i]; sha != "" && c.Status != 'D' && c.NewMode != policy.ModeSymlink &&
			policy.MatchAny(lim.BinaryPaths, c.Path) && c.NewSize <= lim.BinaryMaxFileBytes {
			start, err := m.git(ctx, nil, binarySniff, "cat-file", "blob", sha)
			if err != nil {
				return d, err
			}
			if bytes.IndexByte(start, 0) >= 0 {
				c.Binary = true
				continue
			}
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

// OnBranch reports whether sha is the tip of a branch of the mirror or a
// commit that branch builds on. The base commit of a workspace is reported
// by the sandbox, where the agent can change the note of it. A commit the
// agent made is never on the base branch, so this is what keeps the agent
// from choosing what its work is compared with.
//
// An empty branch means the base branch is not named in the settings. Any
// branch then counts, except those under notUnder, the prefix of the
// branches the service itself pushes.
func (m *Mirror) OnBranch(ctx context.Context, sha, branch, notUnder string) (bool, error) {
	if !shaRE.MatchString(sha) {
		return false, fmt.Errorf("invalid commit id")
	}
	if branch == "" {
		if notUnder == "" {
			return false, fmt.Errorf("neither a base branch nor a branch prefix is set")
		}
		out, err := m.git(ctx, nil, 0, "for-each-ref", "--format=%(refname)", "--contains", sha, "refs/heads/")
		if err != nil {
			if exited(err) {
				return false, nil // not a commit of the mirror
			}
			return false, err
		}
		for _, ref := range strings.Fields(string(out)) {
			if !strings.HasPrefix(ref, "refs/heads/"+notUnder) {
				return true, nil
			}
		}
		return false, nil
	}
	if !ValidBranch(branch) {
		return false, fmt.Errorf("invalid branch name")
	}
	if _, err := m.git(ctx, nil, 0, "merge-base", "--is-ancestor", sha, "refs/heads/"+branch); err != nil {
		if exited(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// HistoryAdded returns every line that a commit between base and head
// adds, and the names of the files it touches, also when a later commit
// takes the line out again. Diff shows the change from base to head only,
// and a push sends every commit. complete is false when the commits hold
// more than maxBytes of changes; the lines are then not all there.
func (m *Mirror) HistoryAdded(ctx context.Context, baseSHA, head string, maxBytes int64) (lines []string, complete bool, err error) {
	if !shaRE.MatchString(baseSHA) || !shaRE.MatchString(head) {
		return nil, false, fmt.Errorf("invalid commit id")
	}
	if maxBytes <= 0 {
		return nil, false, fmt.Errorf("no limit for the history is set")
	}
	out := &capped{limit: maxBytes}
	if err := m.gitTo(ctx, out, "log", "-p", "--text", "-U0", "--no-renames", "--no-ext-diff", "--no-textconv", "--no-color",
		"--full-index", "--format=", baseSHA+".."+head, "--"); err != nil {
		return nil, false, err
	}
	// The log shows nothing for a merge. What a merge of the base branch
	// adds on top of the base it merged is the work so far, with whatever
	// was typed while resolving conflicts: that is looked at too.
	merges, err := m.git(ctx, nil, 0, "rev-list", "--merges", baseSHA+".."+head, "--")
	if err != nil {
		return nil, false, err
	}
	for _, sha := range strings.Fields(string(merges)) {
		if !shaRE.MatchString(sha) {
			return nil, false, fmt.Errorf("a merge commit could not be read")
		}
		if out.over {
			break
		}
		rest := &capped{limit: out.limit - int64(out.buf.Len())}
		if rest.limit <= 0 {
			out.over = true
			break
		}
		if err := m.gitTo(ctx, rest, "diff", "--text", "-U0", "--no-renames", "--no-ext-diff", "--no-textconv", "--no-color",
			"--full-index", sha+"^2", sha, "--"); err != nil {
			return nil, false, err
		}
		out.buf.WriteString("\n")
		out.buf.Write(rest.buf.Bytes())
		out.over = out.over || rest.over
	}
	// A line of a file's content always follows a "@@" line and starts
	// with "+", "-", a space or a backslash, so "diff " at the start of a
	// line is git's own. Only between the two is "+++ " a file name.
	inHunk := false
	for _, line := range strings.Split(out.buf.String(), "\n") {
		switch {
		case strings.HasPrefix(line, "diff "):
			inHunk = false
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case inHunk && strings.HasPrefix(line, "+"):
			lines = append(lines, line[1:])
		case !inHunk && strings.HasPrefix(line, "+++ "):
			lines = append(lines, strings.TrimPrefix(line[4:], "b/"))
		}
	}
	return lines, !out.over, nil
}

// PushedRef records the head last pushed for a workspace. It is the lease
// for the next push.
func PushedRef(workspaceKey string) string { return "refs/maestro/pushed/" + workspaceKey }

// ErrBranchMoved means the remote branch is not where the service left it:
// someone else pushed to it. The service does not overwrite that.
var ErrBranchMoved = errors.New("the remote branch was changed by someone else since the last push")

var branchRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,200}$`)

// ValidBranch reports whether name is a branch name the service is willing
// to push: plain characters, no component git treats specially.
func ValidBranch(name string) bool {
	if !branchRE.MatchString(name) || strings.Contains(name, "..") || strings.Contains(name, "//") ||
		strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") || strings.HasSuffix(name, ".lock") {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if strings.HasPrefix(seg, ".") || strings.HasSuffix(seg, ".lock") {
			return false
		}
	}
	return true
}

// askpass answers git's credential prompts from the environment. The file
// holds no secret, so nothing is left on disk if the process dies.
const askpass = `#!/bin/sh
case "$1" in
Username*) printf '%s\n' x-access-token ;;
*) printf '%s\n' "$MAESTRO_PUSH_TOKEN" ;;
esac
`

// Push sends the imported head of a workspace to a branch of the remote.
// The first push requires that the branch does not exist. A later push
// replaces the branch only if it is still at the head pushed before.
func (m *Mirror) Push(ctx context.Context, remoteURL, workspaceKey, branch, token string) error {
	if !keyRE.MatchString(workspaceKey) || strings.Contains(workspaceKey, "..") {
		return fmt.Errorf("invalid workspace key")
	}
	if !ValidBranch(branch) {
		return fmt.Errorf("invalid branch name")
	}
	scheme := "https"
	switch {
	case strings.HasPrefix(remoteURL, "https://") && !strings.ContainsAny(remoteURL, "@ \t\n"):
	case m.AllowFileRemote && strings.HasPrefix(remoteURL, "/"):
		scheme = "file"
	default:
		return fmt.Errorf("the remote must be an https URL without credentials")
	}
	out, err := m.git(ctx, nil, 0, "rev-parse", "--verify", "--quiet", IncomingRef(workspaceKey)+"^{commit}")
	if err != nil {
		return fmt.Errorf("nothing was imported for this workspace: %w", err)
	}
	head := strings.TrimSpace(string(out))
	expected := ""
	if out, err := m.git(ctx, nil, 0, "rev-parse", "--verify", "--quiet", PushedRef(workspaceKey)+"^{commit}"); err == nil {
		expected = strings.TrimSpace(string(out))
	} else if !exited(err) {
		return err
	}
	if expected == head {
		return nil // already pushed
	}

	dir, err := os.MkdirTemp(m.Dir, "push-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	helper := filepath.Join(dir, "askpass")
	if err := os.WriteFile(helper, []byte(askpass), 0o700); err != nil {
		return err
	}
	ref := "refs/heads/" + branch
	// An empty expected value means "the branch must not exist".
	out, err = m.gitEnv(ctx, nil, 0, []string{"GIT_ASKPASS=" + helper, "MAESTRO_PUSH_TOKEN=" + token},
		"-c", "protocol."+scheme+".allow=always", "-c", "http.followRedirects=false",
		"push", "--porcelain", "--no-verify", "--no-recurse-submodules",
		"--force-with-lease="+ref+":"+expected, remoteURL, head+":"+ref)
	if err != nil {
		if exited(err) {
			if bytes.Contains(out, []byte("(stale info)")) {
				return ErrBranchMoved
			}
			return fmt.Errorf("the push was rejected by the remote")
		}
		return err
	}
	_, err = m.git(ctx, nil, 0, "update-ref", PushedRef(workspaceKey), head)
	return err
}

// ForgetPush drops the record of an earlier push, for a workspace whose
// branch was deleted on the remote (for example after a merge).
func (m *Mirror) ForgetPush(ctx context.Context, workspaceKey string) error {
	if !keyRE.MatchString(workspaceKey) || strings.Contains(workspaceKey, "..") {
		return fmt.Errorf("invalid workspace key")
	}
	_, err := m.git(ctx, nil, 0, "update-ref", "-d", PushedRef(workspaceKey))
	return err
}

// Sync creates the mirror if it does not exist and fetches the remote's
// branches into it (SPEC 9.2). Tags are not fetched. The token is handed
// to git through an ephemeral helper and never stored in the mirror.
// Branches named in skip are left out: they hold something other than
// code, and every workspace is cloned from the mirror.
func (m *Mirror) Sync(ctx context.Context, remoteURL, token string, skip ...string) error {
	refs := []string{"+refs/heads/*:refs/heads/*"}
	for _, b := range skip {
		if !ValidBranch(b) {
			return fmt.Errorf("invalid branch name")
		}
		refs = append(refs, "^refs/heads/"+b)
	}
	scheme := "https"
	switch {
	case strings.HasPrefix(remoteURL, "https://") && !strings.ContainsAny(remoteURL, "@ \t\n"):
	case m.AllowFileRemote && strings.HasPrefix(remoteURL, "/"):
		scheme = "file"
	default:
		return fmt.Errorf("the remote must be an https URL without credentials")
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "HEAD")); err != nil {
		if err := os.MkdirAll(m.Dir, 0o755); err != nil {
			return err
		}
		if _, err := m.git(ctx, nil, 0, "init", "--quiet", "--bare"); err != nil {
			return err
		}
	}
	dir, err := os.MkdirTemp(m.Dir, "sync-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	helper := filepath.Join(dir, "askpass")
	if err := os.WriteFile(helper, []byte(askpass), 0o700); err != nil {
		return err
	}
	args := append([]string{"-c", "protocol." + scheme + ".allow=always", "-c", "http.followRedirects=false",
		"fetch", "--quiet", "--prune", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", remoteURL}, refs...)
	if _, err := m.gitEnv(ctx, nil, 0, []string{"GIT_ASKPASS=" + helper, "MAESTRO_PUSH_TOKEN=" + token}, args...); err != nil {
		if exited(err) {
			return fmt.Errorf("the repository could not be fetched")
		}
		return err
	}
	// A skipped branch that an earlier fetch brought in is not pruned by
	// the fetch, so it is removed here.
	for _, b := range skip {
		if m.HasBranch(ctx, b) {
			if _, err := m.git(ctx, nil, 0, "update-ref", "-d", "refs/heads/"+b); err != nil {
				return err
			}
		}
	}
	return nil
}

// HasBranch reports whether the mirror has a branch of that name.
func (m *Mirror) HasBranch(ctx context.Context, name string) bool {
	if !ValidBranch(name) {
		return false
	}
	_, err := m.git(ctx, nil, 0, "rev-parse", "--verify", "--quiet", "refs/heads/"+name+"^{commit}")
	return err == nil
}

// BaseMerge is what MergeBase found or did.
type BaseMerge int

const (
	// BaseSkipped: there is nothing the service may merge into. Nothing
	// was pushed for the workspace, a branch is missing, or the remote
	// branch is not where the service left it.
	BaseSkipped BaseMerge = iota
	// BaseCurrent: the branch already holds the newest base.
	BaseCurrent
	// BaseMerged: a merge commit was made and waits to be pushed.
	BaseMerged
	// BaseConflict: the base does not merge without conflicts. Nothing
	// was changed.
	BaseConflict
)

// MergeBase merges the base branch into the pushed head of a workspace,
// in the mirror, without a working tree. It expects the mirror to have
// been fetched just before. A clean merge becomes a commit by name and
// email, stored as the workspace's imported head, so that Push sends it;
// the head it returns is that commit. A merge with conflicts changes
// nothing: resolving them is work for an agent.
//
// Both sides are commits the service already accepted: the pushed head
// went through the publish gate and the base is the owner's branch. No
// attribute, hook or merge driver of the repository takes part.
func (m *Mirror) MergeBase(ctx context.Context, workspaceKey, branch, baseBranch, name, email string) (BaseMerge, string, error) {
	if !keyRE.MatchString(workspaceKey) || strings.Contains(workspaceKey, "..") {
		return BaseSkipped, "", fmt.Errorf("invalid workspace key")
	}
	if !ValidBranch(branch) || !ValidBranch(baseBranch) || branch == baseBranch {
		return BaseSkipped, "", fmt.Errorf("invalid branch name")
	}
	if name == "" || email == "" || strings.ContainsAny(name+email, "<>\n\r") {
		return BaseSkipped, "", fmt.Errorf("invalid commit identity")
	}
	rev := func(ref string) (string, error) {
		out, err := m.git(ctx, nil, 0, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		if err != nil {
			if exited(err) {
				return "", nil
			}
			return "", err
		}
		return strings.TrimSpace(string(out)), nil
	}
	pushed, err := rev(PushedRef(workspaceKey))
	if err != nil {
		return BaseSkipped, "", err
	}
	remote, err := rev("refs/heads/" + branch)
	if err != nil {
		return BaseSkipped, "", err
	}
	base, err := rev("refs/heads/" + baseBranch)
	if err != nil {
		return BaseSkipped, "", err
	}
	// Only a branch that is exactly what the service pushed is touched.
	if pushed == "" || base == "" || remote != pushed {
		return BaseSkipped, "", nil
	}
	if _, err := m.git(ctx, nil, 0, "merge-base", "--is-ancestor", base, pushed); err == nil {
		return BaseCurrent, pushed, nil
	} else if !exited(err) {
		return BaseSkipped, "", err
	}
	out, err := m.git(ctx, nil, 0, "merge-tree", "--write-tree", "--no-messages", pushed, base)
	if err != nil {
		var ge *gitError
		var ee *exec.ExitError
		if errors.As(err, &ge) && errors.As(ge.err, &ee) && ee.ExitCode() == 1 {
			return BaseConflict, "", nil
		}
		return BaseSkipped, "", err
	}
	tree := strings.TrimSpace(string(out))
	if !shaRE.MatchString(tree) {
		return BaseSkipped, "", fmt.Errorf("the merge gave no tree")
	}
	env := []string{"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email}
	out, err = m.gitEnv(ctx, nil, 0, env, "commit-tree", tree, "-p", pushed, "-p", base,
		"-m", "Merge "+baseBranch+" into "+branch)
	if err != nil {
		return BaseSkipped, "", err
	}
	head := strings.TrimSpace(string(out))
	if !shaRE.MatchString(head) {
		return BaseSkipped, "", fmt.Errorf("the merge gave no commit")
	}
	if _, err := m.git(ctx, nil, 0, "update-ref", IncomingRef(workspaceKey), head); err != nil {
		return BaseSkipped, "", err
	}
	return BaseMerged, head, nil
}
