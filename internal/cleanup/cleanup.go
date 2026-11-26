// Package cleanup removes what finished tickets leave behind (SPEC 8.6 and
// 16.2): the remote branch, the workspace and the mirror's records of a
// ticket that is done, closed or gone; audit files past their retention;
// and sandbox images that a newer build replaced.
//
// It is careful in one direction only. Anything it cannot place is kept: a
// ticket that cannot be read, a name it cannot map back to a ticket, a
// branch with an open pull request, anything of a ticket the service is
// working on. It never touches the base branch or a branch outside
// publish.branch_prefix.
package cleanup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/tracker"
)

// Retention of the audit log (SPEC 16.2).
const (
	AuditMaxAge   = 30 * 24 * time.Hour
	AuditMaxBytes = 2 << 30
)

// ErrForeign is what a Lookup answers for a name that cannot be one of its
// tracker's identifiers. Such a name is left alone without a word.
var ErrForeign = tracker.ErrForeignIdentifier

// ErrKept is what a removal answers for something that turned out to be
// in use. It is left out of the report.
var ErrKept = errors.New("still in use")

// hashedRE matches a key made from an identifier with characters a key
// cannot hold (issue.WorkspaceKey): it cannot be turned back into the
// identifier.
var hashedRE = regexp.MustCompile(`_.*-[0-9a-f]{16}$`)

// Image is a sandbox image that may go.
type Image struct {
	ID    string
	Name  string
	Bytes int64
}

// Deps are what a sweep may look at and remove. A nil function turns its
// part off.
type Deps struct {
	// Lookup reads a ticket by its identifier. found is false when the
	// tracker says there is no such ticket (any more).
	Lookup func(ctx context.Context, identifier string) (is issue.Issue, found bool, err error)
	// Finished reports whether a ticket is in a terminal state.
	Finished func(is issue.Issue) bool
	// Busy reports whether the service is working on the ticket: running,
	// waiting for a retry or for checks.
	Busy func(identifier string) bool

	// Branches lists the remote's branch names (as the mirror last
	// fetched them).
	Branches     func(ctx context.Context) ([]string, error)
	BranchPrefix string
	BaseBranch   string
	// KeepBranches are never deleted, such as the screenshots branch.
	KeepBranches []string
	// OpenChange reports whether the ticket's branch has an open pull
	// request.
	OpenChange   func(ctx context.Context, is issue.Issue) (bool, error)
	DeleteBranch func(ctx context.Context, is issue.Issue) error

	// Workspaces lists the workspace keys that exist.
	Workspaces      func(ctx context.Context) ([]string, error)
	RemoveWorkspace func(ctx context.Context, identifier string) error

	// MirrorKeys lists the workspace keys the mirror keeps refs for.
	MirrorKeys   func(ctx context.Context) ([]string, error)
	ForgetMirror func(ctx context.Context, key string) error
	// CompactMirror lets git drop objects nothing refers to any more.
	CompactMirror func(ctx context.Context) error

	// AuditDir holds one <workspace_key>.jsonl per ticket.
	AuditDir      string
	AuditMaxAge   time.Duration
	AuditMaxBytes int64

	StaleImages func(ctx context.Context) ([]Image, error)
	RemoveImage func(ctx context.Context, id string) error
	// PruneBuildCache removes the container engine's build cache that
	// went unused for a week, and answers how much that freed, as text.
	PruneBuildCache func(ctx context.Context) (freed string, err error)

	Now func() time.Time
	Log *slog.Logger
}

// Item is one thing removed, or one that would be in a dry run.
type Item struct {
	Kind   string `json:"kind"` // branch, workspace, mirror, audit, image, cache
	Name   string `json:"name"`
	Reason string `json:"reason"`
	Bytes  int64  `json:"bytes,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Report is what a sweep did.
type Report struct {
	DryRun  bool   `json:"dry_run"`
	Removed []Item `json:"removed"`
	// Kept lists what was left on purpose although it looked finished,
	// such as a branch with an open pull request.
	Kept []Item `json:"kept"`
	// Problems are parts that could not be looked at.
	Problems []string `json:"problems"`
}

// Failed counts the removals that failed.
func (r Report) Failed() int {
	n := 0
	for _, it := range r.Removed {
		if it.Error != "" {
			n++
		}
	}
	return n
}

type held struct{ branch, workspace, mirror bool }

// Run sweeps once. With dry set it removes nothing and reports what it
// would remove.
func Run(ctx context.Context, d Deps, dry bool) Report {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	r := Report{DryRun: dry, Removed: []Item{}, Kept: []Item{}, Problems: []string{}}
	busy := func(id string) bool { return d.Busy != nil && d.Busy(id) }

	if d.Lookup != nil && d.Finished != nil {
		sweepTickets(ctx, d, dry, busy, &r)
	}
	sweepAudit(d, dry, busy, &r)
	if d.StaleImages != nil && d.RemoveImage != nil {
		imgs, err := d.StaleImages(ctx)
		if err != nil {
			r.Problems = append(r.Problems, "sandbox images could not be listed: "+err.Error())
		}
		for _, img := range imgs {
			it := Item{Kind: "image", Name: img.Name, Bytes: img.Bytes, Reason: "replaced by a newer build"}
			if !dry {
				if err := d.RemoveImage(ctx, img.ID); errors.Is(err, ErrKept) {
					continue
				} else if err != nil {
					it.Error = err.Error()
				}
			}
			r.Removed = append(r.Removed, it)
		}
	}
	// The engine cannot say beforehand how much a prune would free, so a
	// dry run leaves the build cache out, and a prune that freed nothing
	// is not reported.
	if d.PruneBuildCache != nil && !dry {
		it := Item{Kind: "cache", Name: "container build cache", Reason: "not used for a week"}
		freed, err := d.PruneBuildCache(ctx)
		switch {
		case err != nil:
			it.Error = err.Error()
			r.Removed = append(r.Removed, it)
		case freed != "" && strings.TrimLeft(freed, "0.") != "B":
			it.Name += " (" + freed + ")"
			r.Removed = append(r.Removed, it)
		}
	}
	if !dry && d.CompactMirror != nil {
		if err := d.CompactMirror(ctx); err != nil {
			r.Problems = append(r.Problems, "the mirror could not be compacted: "+err.Error())
		}
	}
	for _, it := range r.Removed {
		if it.Error != "" {
			d.Log.Warn("cleanup: not removed", "kind", it.Kind, "name", it.Name, "error", it.Error)
		} else if !dry {
			d.Log.Info("cleanup: removed", "kind", it.Kind, "name", it.Name, "reason", it.Reason)
		}
	}
	return r
}

// sweepTickets removes the branch, workspace and mirror refs of finished
// tickets.
func sweepTickets(ctx context.Context, d Deps, dry bool, busy func(string) bool, r *Report) {
	keys := map[string]*held{}
	at := func(k string) *held {
		if keys[k] == nil {
			keys[k] = &held{}
		}
		return keys[k]
	}
	if d.Branches != nil && d.DeleteBranch != nil && d.OpenChange != nil && d.BranchPrefix != "" {
		names, err := d.Branches(ctx)
		if err != nil {
			r.Problems = append(r.Problems, "branches could not be listed: "+err.Error())
		}
		for _, b := range names {
			k, ok := strings.CutPrefix(b, d.BranchPrefix)
			if !ok || k == "" || strings.Contains(k, "/") || b == d.BaseBranch || contains(d.KeepBranches, b) {
				continue
			}
			at(k).branch = true
		}
	}
	if d.Workspaces != nil && d.RemoveWorkspace != nil {
		ws, err := d.Workspaces(ctx)
		if err != nil {
			r.Problems = append(r.Problems, "workspaces could not be listed: "+err.Error())
		}
		for _, k := range ws {
			at(k).workspace = true
		}
	}
	if d.MirrorKeys != nil && d.ForgetMirror != nil {
		ks, err := d.MirrorKeys(ctx)
		if err != nil {
			r.Problems = append(r.Problems, "the mirror's records could not be listed: "+err.Error())
		}
		for _, k := range ks {
			at(k).mirror = true
		}
	}

	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	type finished struct {
		key, why string
		is       issue.Issue
		gone     bool
	}
	var done []finished
	unread, found, gone := 0, 0, 0
	for _, k := range names {
		// A key is the identifier itself unless the identifier had
		// characters a key cannot hold; then it cannot be read back.
		if wk, err := issue.WorkspaceKey(k); err != nil || wk != k || hashedRE.MatchString(k) || busy(k) {
			continue
		}
		is, ok, err := d.Lookup(ctx, k)
		if errors.Is(err, ErrForeign) {
			continue
		}
		if err != nil {
			unread++
			if unread <= 3 {
				r.Problems = append(r.Problems, fmt.Sprintf("ticket %s could not be read, so nothing of it was touched: %v", k, err))
			}
			continue
		}
		switch {
		case !ok:
			gone++
			done = append(done, finished{k, "the ticket no longer exists", issue.Issue{Identifier: k}, true})
		case d.Finished(is):
			found++
			is.Identifier = k
			done = append(done, finished{k, fmt.Sprintf("the ticket is %s", is.State), is, false})
		default:
			found++ // live: left alone
		}
	}
	if unread > 3 {
		r.Problems = append(r.Problems, fmt.Sprintf("%d more tickets could not be read", unread-3))
	}
	// When the board says most tickets do not exist, it is more likely that
	// the credential lost its access than that they were all deleted.
	suspect := gone >= 3 && gone > found
	if suspect {
		r.Problems = append(r.Problems, fmt.Sprintf("the board says %d of %d tickets do not exist; "+
			"that looks like lost access, so nothing of them was touched", gone, gone+found))
	}
	for _, f := range done {
		if suspect && f.gone {
			continue
		}
		k, why, is, h := f.key, f.why, f.is, keys[f.key]
		if h.workspace {
			it := Item{Kind: "workspace", Name: k, Reason: why}
			if !dry {
				if err := d.RemoveWorkspace(ctx, k); err != nil {
					it.Error = err.Error()
				}
			}
			r.Removed = append(r.Removed, it)
		}
		branchKept := false
		if h.branch {
			name := d.BranchPrefix + k
			open, err := d.OpenChange(ctx, is)
			switch {
			case err != nil:
				branchKept = true
				r.Kept = append(r.Kept, Item{Kind: "branch", Name: name, Reason: "its pull requests could not be read: " + err.Error()})
			case open:
				branchKept = true
				r.Kept = append(r.Kept, Item{Kind: "branch", Name: name, Reason: why + ", but a pull request from the branch is still open"})
			default:
				it := Item{Kind: "branch", Name: name, Reason: why}
				if !dry {
					if err := d.DeleteBranch(ctx, is); err != nil {
						it.Error = err.Error()
						branchKept = true
					}
				}
				r.Removed = append(r.Removed, it)
			}
		}
		// The mirror's record of the last push is the lease for the next
		// one: it stays as long as the branch does.
		if h.mirror && !branchKept {
			it := Item{Kind: "mirror", Name: k, Reason: why}
			if !dry {
				if err := d.ForgetMirror(ctx, k); err != nil {
					it.Error = err.Error()
				}
			}
			r.Removed = append(r.Removed, it)
		}
	}
}

// sweepAudit applies the audit log's retention: files untouched for longer
// than the maximum age go, then the oldest until the folder fits the size.
func sweepAudit(d Deps, dry bool, busy func(string) bool, r *Report) {
	if d.AuditDir == "" {
		return
	}
	maxAge, maxBytes := d.AuditMaxAge, d.AuditMaxBytes
	if maxAge <= 0 {
		maxAge = AuditMaxAge
	}
	if maxBytes <= 0 {
		maxBytes = AuditMaxBytes
	}
	entries, err := os.ReadDir(d.AuditDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			r.Problems = append(r.Problems, "the audit folder could not be read: "+err.Error())
		}
		return
	}
	type file struct {
		name, key string
		size      int64
		mod       time.Time
	}
	var files []file
	var total int64
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, file{e.Name(), strings.TrimSuffix(e.Name(), ".jsonl"), info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	cutoff := d.Now().Add(-maxAge)
	for _, f := range files {
		var why string
		switch {
		case busy(f.key):
			continue
		case f.mod.Before(cutoff):
			why = fmt.Sprintf("older than %d days", int(maxAge.Hours()/24))
		case total > maxBytes:
			why = fmt.Sprintf("the audit log is over %s", size(maxBytes))
		default:
			continue
		}
		it := Item{Kind: "audit", Name: f.name, Reason: why, Bytes: f.size}
		if !dry {
			if err := os.Remove(filepath.Join(d.AuditDir, f.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				it.Error = err.Error()
			}
		}
		if it.Error == "" {
			total -= f.size
		}
		r.Removed = append(r.Removed, it)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// Size formats a byte count for people.
func Size(n int64) string { return size(n) }
