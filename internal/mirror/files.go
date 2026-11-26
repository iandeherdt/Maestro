package mirror

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// File is the content of one file the service itself writes into a
// commit, and whether it is executable.
type File struct {
	Content    []byte
	Executable bool
}

// maxReadBytes bounds a file read from a branch.
const maxReadBytes = 4 << 20

// FileAt reads a file of a branch. It reports false when the branch has no
// such file, or when the path is not a regular file there.
func (m *Mirror) FileAt(ctx context.Context, branch, path string) ([]byte, bool, error) {
	if !ValidBranch(branch) || !safePath(path) {
		return nil, false, fmt.Errorf("invalid branch or path")
	}
	spec := "refs/heads/" + branch + ":" + path
	out, err := m.git(ctx, nil, 0, "cat-file", "-t", spec)
	if err != nil {
		if exited(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if strings.TrimSpace(string(out)) != "blob" {
		return nil, false, nil
	}
	raw, err := m.git(ctx, nil, maxReadBytes+1, "cat-file", "blob", spec)
	if err != nil {
		return nil, false, err
	}
	if len(raw) > maxReadBytes {
		return nil, false, fmt.Errorf("%s is too large to read", path)
	}
	return raw, true, nil
}

// CommitFiles makes a commit on top of a branch that sets the given files
// and changes nothing else, without a working tree, and keeps it as the
// imported head of workspaceKey so that Push sends it. It reports which
// paths the commit changes; when none, no commit is made.
//
// The commit takes its date from the branch's head, so that the same
// files on the same branch always give the same commit: a later call
// finds it already pushed.
func (m *Mirror) CommitFiles(ctx context.Context, workspaceKey, branch string, files map[string]File, message, name, email string) (string, []string, error) {
	if !keyRE.MatchString(workspaceKey) || strings.Contains(workspaceKey, "..") {
		return "", nil, fmt.Errorf("invalid workspace key")
	}
	if !ValidBranch(branch) {
		return "", nil, fmt.Errorf("invalid branch name")
	}
	if name == "" || email == "" || strings.ContainsAny(name+email, "<>\n\r") {
		return "", nil, fmt.Errorf("invalid commit identity")
	}
	out, err := m.git(ctx, nil, 0, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return "", nil, fmt.Errorf("the branch %s is missing", branch)
	}
	base := strings.TrimSpace(string(out))
	if !shaRE.MatchString(base) {
		return "", nil, fmt.Errorf("the branch %s has no usable head", branch)
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		if !safePath(p) {
			return "", nil, fmt.Errorf("invalid path %q", p)
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)

	dir, err := os.MkdirTemp(m.Dir, "commit-")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(dir)
	idx := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}
	if _, err := m.gitEnv(ctx, nil, 0, idx, "read-tree", base); err != nil {
		return "", nil, err
	}
	for _, p := range paths {
		f := files[p]
		out, err := m.git(ctx, bytes.NewReader(f.Content), 0, "hash-object", "-w", "--no-filters", "--stdin")
		if err != nil {
			return "", nil, err
		}
		blob := strings.TrimSpace(string(out))
		if !shaRE.MatchString(blob) {
			return "", nil, fmt.Errorf("no object for %s", p)
		}
		mode := "100644"
		if f.Executable {
			mode = "100755"
		}
		if _, err := m.gitEnv(ctx, nil, 0, idx, "update-index", "--add", "--cacheinfo", mode+","+blob+","+p); err != nil {
			return "", nil, fmt.Errorf("%s cannot be written: %w", p, err)
		}
	}
	out, err = m.gitEnv(ctx, nil, 0, idx, "write-tree")
	if err != nil {
		return "", nil, err
	}
	tree := strings.TrimSpace(string(out))
	if !shaRE.MatchString(tree) {
		return "", nil, fmt.Errorf("no tree was written")
	}
	out, err = m.git(ctx, nil, 1<<20, "diff-tree", "-r", "--name-only", "--no-renames", "-z", base, tree)
	if err != nil {
		return "", nil, err
	}
	var changed []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			changed = append(changed, p)
		}
	}
	if len(changed) == 0 {
		return "", nil, nil
	}
	out, err = m.git(ctx, nil, 0, "log", "-1", "--format=%ct", base)
	if err != nil {
		return "", nil, err
	}
	when := "@" + strings.TrimSpace(string(out)) + " +0000"
	env := []string{"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email, "GIT_AUTHOR_DATE=" + when,
		"GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email, "GIT_COMMITTER_DATE=" + when}
	out, err = m.gitEnv(ctx, nil, 0, env, "commit-tree", tree, "-p", base, "-m", message)
	if err != nil {
		return "", nil, err
	}
	head := strings.TrimSpace(string(out))
	if !shaRE.MatchString(head) {
		return "", nil, fmt.Errorf("no commit was made")
	}
	if _, err := m.git(ctx, nil, 0, "update-ref", IncomingRef(workspaceKey), head); err != nil {
		return "", nil, err
	}
	return head, changed, nil
}

// safePath is a plain relative path inside the repository.
func safePath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.ContainsAny(p, "\x00\n\r\\") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.EqualFold(seg, ".git") {
			return false
		}
	}
	return true
}
