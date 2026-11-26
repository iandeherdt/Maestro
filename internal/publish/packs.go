package publish

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/iandeherdt/Maestro/internal/conventions"
	"github.com/iandeherdt/Maestro/internal/mirror"
)

// Keeping the rule packs current (SPEC 23). The files of a pack are part
// of the binary. When an update of Maestro changes them, the repository
// still has the old ones until someone copies the new ones in. The service
// does that itself: it compares the repository's copy with its own and,
// when they differ, opens a pull request with its own files. The owner
// reviews and merges it like any other.

// PackMirror is the part of the mirror that writes the service's own
// files into a commit.
type PackMirror interface {
	FileAt(ctx context.Context, branch, path string) ([]byte, bool, error)
	CommitFiles(ctx context.Context, workspaceKey, branch string, files map[string]mirror.File, message, name, email string) (string, []string, error)
	HasBranch(ctx context.Context, name string) bool
	Push(ctx context.Context, remoteURL, workspaceKey, branch, token string) error
	ForgetPush(ctx context.Context, workspaceKey string) error
}

// packsKey names the update's branch and its record in the mirror.
const packsKey = "maestro-packs"

// PacksBranch is the branch of the pull request that updates the packs.
func PacksBranch(prefix string) string { return prefix + "packs" }

// PackUpdate is what UpdatePacks did.
type PackUpdate struct {
	// PRNumber is the pull request opened or updated, 0 for none.
	PRNumber int
	// Packs are the packs whose files changed.
	Packs []string
	// Files are the paths the pull request changes.
	Files []string
	// ByHand are files that differ but that the service may not write,
	// such as CI workflows. The pull request names them.
	ByHand []string
	// Declined is true when the owner closed a pull request with exactly
	// these files: it is not opened again until the files change.
	Declined bool
}

// workflowDir is where CI workflows live. The service's token has no right
// to change them, which is on purpose.
const workflowDir = ".github/workflows/"

// UpdatePacks opens or updates the pull request that brings the files of
// the switched-on packs in the repository up to this version. A pack whose
// files the repository does not have at all was never applied there and
// is left alone.
func (p *Publisher) UpdatePacks(ctx context.Context) (PackUpdate, error) {
	var out PackUpdate
	cfg := p.Config()
	pm, ok := p.Mirror.(PackMirror)
	if !ok || p.Fetch == nil || len(cfg.Conventions) == 0 || cfg.Workspace.BaseBranch == "" || p.BotName == "" || p.BotEmail == "" {
		return out, nil
	}
	packs, err := conventions.Resolve(cfg.Conventions)
	if err != nil {
		return out, err
	}
	if err := p.Fetch(ctx); err != nil {
		return out, fmt.Errorf("fetch: %w", err)
	}
	base := cfg.Workspace.BaseBranch
	files := map[string]mirror.File{}
	var ignore []string
	var byHand []string
	for _, pk := range packs {
		if _, applied, err := pm.FileAt(ctx, base, pk.GuidePath()); err != nil {
			return out, err
		} else if !applied {
			continue
		}
		own, err := pk.Files()
		if err != nil {
			return out, err
		}
		names := make([]string, 0, len(own))
		for n := range own {
			names = append(names, n)
		}
		sort.Strings(names)
		touched := false
		for _, n := range names {
			now, has, err := pm.FileAt(ctx, base, n)
			if err != nil {
				return out, err
			}
			if has && string(now) == string(own[n]) {
				continue
			}
			if strings.HasPrefix(n, workflowDir) {
				byHand = append(byHand, n)
				continue
			}
			files[n] = mirror.File{Content: own[n], Executable: strings.HasSuffix(n, ".sh")}
			touched = true
		}
		ignore = append(ignore, pk.Gitignore...)
		if touched {
			out.Packs = append(out.Packs, pk.Name)
		}
	}
	if len(files) == 0 && len(byHand) == 0 {
		return out, nil
	}
	if len(files) > 0 {
		// The lines the checks need in .gitignore, like apply adds them.
		old, _, err := pm.FileAt(ctx, base, ".gitignore")
		if err != nil {
			return out, err
		}
		if text, added := withIgnored(string(old), ignore); added {
			files[".gitignore"] = mirror.File{Content: []byte(text)}
		}
	}
	out.ByHand = byHand
	if len(files) == 0 {
		// Only files the service may not write: nothing to open.
		return out, nil
	}

	h, err := p.host()
	if err != nil {
		return out, err
	}
	branch := PacksBranch(cfg.Publish.BranchPrefix)
	if !pm.HasBranch(ctx, branch) {
		// The last update was merged and its branch deleted, or there
		// never was one.
		if err := pm.ForgetPush(ctx, packsKey); err != nil {
			return out, err
		}
	}
	msg := "Update the rule packs: " + strings.Join(out.Packs, ", ")
	head, changed, err := pm.CommitFiles(ctx, packsKey, base, files, msg, p.BotName, p.BotEmail)
	if err != nil {
		return out, err
	}
	if len(changed) == 0 {
		return out, nil
	}
	out.Files = changed
	prs, err := h.changes(ctx, branch)
	if err != nil {
		return out, err
	}
	if len(prs) > 0 && prs[0].State == changeClosed && prs[0].HeadSHA == head {
		out.Declined = true
		return out, nil
	}
	tok, err := p.Token(ctx)
	if err != nil {
		return out, fmt.Errorf("repository token: %w", err)
	}
	if err := pm.Push(ctx, h.gitURL(), packsKey, branch, tok); err != nil {
		if errors.Is(err, mirror.ErrBranchMoved) {
			return out, fmt.Errorf("someone else pushed to %s; the service leaves it alone. Delete the branch to get a fresh update", branch)
		}
		return out, fmt.Errorf("push: %w", err)
	}
	number, err := h.upsert(ctx, branch, base, msg, packsBody(out), false)
	if err != nil {
		return out, fmt.Errorf("pull request: %w", err)
	}
	out.PRNumber = number
	return out, nil
}

// withIgnored adds the lines .gitignore lacks, and reports whether it did.
func withIgnored(old string, lines []string) (string, bool) {
	have := map[string]bool{}
	for _, l := range strings.Split(old, "\n") {
		have[strings.TrimSpace(l)] = true
	}
	var add []string
	for _, l := range lines {
		if !have[l] && !have["/"+l] {
			add = append(add, l)
			have[l] = true
		}
	}
	if len(add) == 0 {
		return old, false
	}
	if old != "" && !strings.HasSuffix(old, "\n") {
		old += "\n"
	}
	return old + strings.Join(add, "\n") + "\n", true
}

// packsBody is the description of the pull request that updates the packs.
func packsBody(u PackUpdate) string {
	var b strings.Builder
	b.WriteString("This version of Maestro has newer files for the rule packs switched on for this repository (" +
		strings.Join(u.Packs, ", ") + "). This pull request puts them in, as `maestro conventions apply --replace` would. " +
		"It changes nothing else.\n\n")
	b.WriteString("- **Merge it** to use them. The checks of the next ticket run with the new files.\n")
	b.WriteString("- **Close it** to keep the files you have. It is not opened again until a later version changes them once more.\n")
	b.WriteString("\n## Files\n\n")
	for _, f := range u.Files {
		b.WriteString("- " + code(f) + "\n")
	}
	if len(u.ByHand) > 0 {
		b.WriteString("\n## To copy by hand\n\nThe service may not change CI workflows, so these newer files are not in this pull request. " +
			"Run `maestro conventions apply " + strings.Join(u.Packs, " ") + " --replace` in a checkout to get them:\n\n")
		for _, f := range u.ByHand {
			b.WriteString("- " + code(f) + "\n")
		}
	}
	b.WriteString("\n---\n" + footer + "\n")
	return b.String()
}

var _ PackMirror = (*mirror.Mirror)(nil)
