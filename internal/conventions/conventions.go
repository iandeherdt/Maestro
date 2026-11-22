// Package conventions holds the rule packs: sets of conventions for one
// kind of project in which every rule names the tool that enforces it.
//
// A pack is enforced in three places the agent cannot reach. The service
// runs the pack's verify script itself before it pushes anything. The
// repository's CI runs the same script, plus the checks that need the
// network. And the diff policy refuses any change to the rule files, so
// the cheapest way to pass a rule, weakening it, is closed.
//
// The packs are part of the binary. `maestro conventions apply` writes a
// pack's files into a checkout for the owner to commit; the settings file
// then names the pack, and the configuration derives the rest from it.
package conventions

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

//go:embed all:packs
var packs embed.FS

// Rule is one convention and what guarantees it.
type Rule struct {
	ID string `json:"id"`
	// Rule says what holds, in a sentence a person can check.
	Rule string `json:"rule"`
	// EnforcedBy names the tools. A rule without one is not a rule.
	EnforcedBy []string `json:"enforced_by"`
	// Where it is checked: "verify" (by the service before the push),
	// "ci" (on the pull request) or "service" (by the service's own gates).
	Where []string `json:"where"`
}

// Pack is the set of conventions for one kind of project.
type Pack struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Applies string `json:"applies"`
	Rules   []Rule `json:"rules"`
	// Protect are the paths the agent may not change, in the pattern
	// language of the diff policy.
	Protect []string `json:"protect"`
	// Hosts are what Prepare needs to reach.
	Hosts []string `json:"hosts"`
	// Prepare installs dependencies. It runs with the hook network.
	Prepare string `json:"prepare"`
	// Verify runs every check that needs no network.
	Verify string `json:"verify"`
	// Check is the name of the CI check the pack's workflow reports.
	Check string `json:"check"`
	// Gitignore are lines the checks need in .gitignore, because the
	// verify step fails when it leaves files behind.
	Gitignore []string `json:"gitignore"`
	// Image are Dockerfile lines that add the pack's tools to the sandbox
	// image, and ImageNote says in words what they add.
	Image     []string `json:"image"`
	ImageNote string   `json:"image_note"`
	// AfterApply are steps only the owner can do, shown after apply.
	AfterApply []string `json:"after_apply"`
	// BinaryPaths are the folders the pack's tools write binary files
	// into by default; they join policy.binary_paths.
	BinaryPaths []string `json:"binary_paths"`
	// Requires is a file the project must have before the pack can be
	// applied, with what to do when it is missing.
	Requires *Requirement `json:"requires"`
	// StarterUnless names a file whose absence marks a new project: then
	// Apply also writes the pack's starter files (packs/NAME/starter/), so
	// that a new project passes its checks from the first commit. A
	// starter file that exists is never touched.
	StarterUnless string `json:"starter_unless"`
}

// Requirement is a file the project must have, and how to make it.
type Requirement struct {
	Path string `json:"path"`
	Hint string `json:"hint"`
}

// MissingError says that the project lacks what the pack requires.
type MissingError struct {
	Pack string
	Requirement
}

func (e *MissingError) Error() string {
	return fmt.Sprintf("the %s pack needs %s, which this checkout does not have. %s", e.Pack, e.Path, e.Hint)
}

var (
	nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	hostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,200}[a-z0-9])?$`)
	lineRE = regexp.MustCompile(`^[^\x00-\x1f\x7f]+$`)
)

// Names lists the packs in the binary.
func Names() []string {
	entries, err := fs.ReadDir(packs, "packs")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// Get returns a pack by name.
func Get(name string) (*Pack, error) {
	if !nameRE.MatchString(name) {
		return nil, unknown(name)
	}
	raw, err := packs.ReadFile(path.Join("packs", name, "pack.json"))
	if err != nil {
		return nil, unknown(name)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	p := &Pack{}
	if err := dec.Decode(p); err != nil {
		return nil, fmt.Errorf("conventions pack %s: %w", name, err)
	}
	if err := p.valid(name); err != nil {
		return nil, fmt.Errorf("conventions pack %s: %w", name, err)
	}
	return p, nil
}

func unknown(name string) error {
	return fmt.Errorf("there is no conventions pack named %q. The packs are: %s", name, strings.Join(Names(), ", "))
}

// valid checks what the rest of the service relies on. A pack is part of
// the binary, so a failure here is a mistake in the pack, found by the
// tests.
func (p *Pack) valid(name string) error {
	switch {
	case p.Name != name:
		return errors.New("the name does not match its directory")
	case p.Title == "" || p.Applies == "" || len(p.Rules) == 0:
		return errors.New("title, applies and rules are required")
	case !lineRE.MatchString(p.Prepare) || !lineRE.MatchString(p.Verify):
		return errors.New("prepare and verify must each be one command line")
	case len(p.Protect) == 0:
		return errors.New("a pack must protect its own rule files")
	}
	seen := map[string]bool{}
	for _, r := range p.Rules {
		if !nameRE.MatchString(r.ID) || seen[r.ID] {
			return fmt.Errorf("rule id %q is malformed or used twice", r.ID)
		}
		seen[r.ID] = true
		if strings.TrimSpace(r.Rule) == "" || len(r.EnforcedBy) == 0 || len(r.Where) == 0 {
			return fmt.Errorf("rule %s needs a text, at least one tool that enforces it, and where", r.ID)
		}
		for _, w := range r.Where {
			if w != "verify" && w != "ci" && w != "service" {
				return fmt.Errorf("rule %s: unknown place %q", r.ID, w)
			}
		}
	}
	for _, h := range p.Hosts {
		if !hostRE.MatchString(h) {
			return fmt.Errorf("host %q is malformed", h)
		}
	}
	for _, g := range append(append([]string{}, p.Protect...), p.BinaryPaths...) {
		if g == "" || strings.HasPrefix(g, "/") || strings.Contains(g, "..") {
			return fmt.Errorf("path %q is malformed", g)
		}
	}
	if p.Requires != nil && (!safeRel(p.Requires.Path) || p.Requires.Hint == "") {
		return errors.New("requires needs a relative path and a hint")
	}
	if p.StarterUnless != "" && !safeRel(p.StarterUnless) {
		return errors.New("starter_unless must be a relative path")
	}
	return nil
}

func safeRel(p string) bool {
	return p != "" && !strings.HasPrefix(p, "/") && !strings.Contains(p, "..")
}

// Resolve returns the packs for a list of names, in the given order.
func Resolve(names []string) ([]*Pack, error) {
	var out []*Pack
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			return nil, fmt.Errorf("conventions pack %q is listed twice", n)
		}
		seen[n] = true
		p, err := Get(n)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// Guide is the page written into the repository as conventions/<pack>/RULES.md.
// It is for people and for the agent, which reads it like any other file.
func (p *Pack) Guide() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Conventions: %s\n\n", p.Title)
	b.WriteString("Every rule below is checked by a tool, not by reading. The checks run in two places:\n" +
		"the Maestro service runs `" + p.Verify + "` itself before it pushes a change, and CI runs the\n" +
		"same script on the pull request (check `" + p.Check + "`).\n\n" +
		"To see what the checks say about your work, run the same command:\n\n" +
		"```sh\n" + p.Verify + "\n```\n\n" +
		"The rule files, the scripts in this directory and the CI workflows cannot be changed by an\n" +
		"agent. A change to them is refused before anything is pushed. When a rule is wrong for this\n" +
		"project, say so in the pull request or on the ticket; the owner changes rules by hand.\n" +
		"Do not work around a rule with a suppression comment or attribute.\n\n")
	fmt.Fprintf(&b, "Applies to: %s\n\n", p.Applies)
	b.WriteString("| Rule | Enforced by | Checked |\n|---|---|---|\n")
	where := map[string]string{"verify": "before the push", "ci": "in CI", "service": "by the service"}
	for _, r := range p.Rules {
		var places []string
		for _, w := range r.Where {
			places = append(places, where[w])
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", cell(r.Rule), cell(strings.Join(r.EnforcedBy, "; ")), strings.Join(places, ", "))
	}
	return b.String()
}

func cell(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

// GuidePath is where Guide is written.
func (p *Pack) GuidePath() string { return "conventions/" + p.Name + "/RULES.md" }

// Files returns the files of the pack: repository path to content.
func (p *Pack) Files() (map[string][]byte, error) {
	root := path.Join("packs", p.Name, "files")
	out := map[string][]byte{p.GuidePath(): []byte(p.Guide())}
	err := fs.WalkDir(packs, root, func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := packs.ReadFile(name)
		if err != nil {
			return err
		}
		out[strings.TrimPrefix(name, root+"/")] = raw
		return nil
	})
	return out, err
}

// Starter returns the starter files of the pack: repository path to
// content. Most packs have none.
func (p *Pack) Starter() (map[string][]byte, error) {
	out := map[string][]byte{}
	root := path.Join("packs", p.Name, "starter")
	if _, err := fs.Stat(packs, root); err != nil {
		return out, nil
	}
	err := fs.WalkDir(packs, root, func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := packs.ReadFile(name)
		if err != nil {
			return err
		}
		out[strings.TrimPrefix(name, root+"/")] = raw
		return nil
	})
	return out, err
}

// Report says what Apply did.
type Report struct {
	Written []string
	// Started are the starter files written for a new project.
	Started   []string
	Unchanged []string
	// Conflicts exist with other content and were left alone.
	Conflicts []string
	// Ignored are the lines added to .gitignore.
	Ignored []string
}

// Apply writes the pack into a checkout. A file that exists with other
// content is not touched unless replace is set; then nothing at all is
// written while any conflict remains unresolved by replace. Symbolic links
// are never followed: the checkout is the owner's, but a repository can
// carry a link that points outside it.
func (p *Pack) Apply(dir string, replace bool) (Report, error) {
	var rep Report
	if r := p.Requires; r != nil {
		if err := noLinks(dir, r.Path); err != nil {
			return rep, err
		}
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(r.Path))); err != nil {
			return rep, &MissingError{Pack: p.Name, Requirement: *r}
		}
	}
	files, err := p.Files()
	if err != nil {
		return rep, err
	}
	starter := map[string]bool{}
	if p.StarterUnless != "" {
		if err := noLinks(dir, p.StarterUnless); err != nil {
			return rep, err
		}
		if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(p.StarterUnless))); errors.Is(err, fs.ErrNotExist) {
			extra, err := p.Starter()
			if err != nil {
				return rep, err
			}
			for n, raw := range extra {
				if err := noLinks(dir, n); err != nil {
					return rep, err
				}
				if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(n))); errors.Is(err, fs.ErrNotExist) {
					if _, dup := files[n]; !dup {
						files[n], starter[n] = raw, true
					}
				}
			}
		}
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)

	todo := map[string]bool{}
	for _, n := range names {
		target := filepath.Join(dir, filepath.FromSlash(n))
		if err := noLinks(dir, n); err != nil {
			return rep, err
		}
		old, err := os.ReadFile(target)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			todo[n] = true
		case err != nil:
			return rep, err
		case bytes.Equal(old, files[n]):
			rep.Unchanged = append(rep.Unchanged, n)
		case replace:
			todo[n] = true
		default:
			rep.Conflicts = append(rep.Conflicts, n)
		}
	}
	if len(rep.Conflicts) > 0 {
		return rep, nil
	}
	for _, n := range names {
		if !todo[n] {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return rep, err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(n, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(target, files[n], mode); err != nil {
			return rep, err
		}
		if starter[n] {
			rep.Started = append(rep.Started, n)
		} else {
			rep.Written = append(rep.Written, n)
		}
	}
	rep.Ignored, err = ignore(dir, p.Gitignore)
	return rep, err
}

// noLinks refuses a path any existing part of which is a symbolic link.
func noLinks(dir, name string) error {
	cur := dir
	for _, seg := range strings.Split(name, "/") {
		cur = filepath.Join(cur, seg)
		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link; the conventions are not written through links", cur)
		}
	}
	return nil
}

// ignore appends the lines .gitignore lacks.
func ignore(dir string, lines []string) ([]string, error) {
	if len(lines) == 0 {
		return nil, nil
	}
	if err := noLinks(dir, ".gitignore"); err != nil {
		return nil, err
	}
	target := filepath.Join(dir, ".gitignore")
	old, err := os.ReadFile(target)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	have := map[string]bool{}
	for _, l := range strings.Split(string(old), "\n") {
		have[strings.TrimSpace(l)] = true
	}
	var add []string
	for _, l := range lines {
		if !have[l] && !have["/"+l] {
			add = append(add, l)
		}
	}
	if len(add) == 0 {
		return nil, nil
	}
	text := string(old)
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	text += strings.Join(add, "\n") + "\n"
	return add, os.WriteFile(target, []byte(text), 0o644)
}

// Settings is what a list of packs adds to the configuration.
type Settings struct {
	Protect []string
	Hosts   []string
	// Prepare and Verify are shell scripts that stop at the first failing
	// pack. Empty when no pack is given.
	Prepare string
	Verify  string
	// Image are the Dockerfile lines of all packs.
	Image []string
	// Checks are the CI checks the packs' workflows report.
	Checks []string
	// BinaryPaths are the packs' folders for binary files.
	BinaryPaths []string
}

// Derive combines packs into settings.
func Derive(list []*Pack) Settings {
	var s Settings
	var prep, ver []string
	for _, p := range list {
		s.Protect = append(s.Protect, p.Protect...)
		s.Hosts = append(s.Hosts, p.Hosts...)
		s.Image = append(s.Image, p.Image...)
		s.Checks = append(s.Checks, p.Check)
		s.BinaryPaths = append(s.BinaryPaths, p.BinaryPaths...)
		// A pack that is switched on while its files are not in the
		// repository must fail with words, not with "No such file".
		guard := fmt.Sprintf("[ -f %s ] || { echo 'The conventions pack %s is switched on in the settings file, "+
			"but its files are not in this repository. Run `maestro conventions apply %s` in a checkout, "+
			"commit and push.' >&2; exit 1; }", p.GuidePath(), p.Name, p.Name)
		prep = append(prep, guard, p.Prepare+" || exit $?")
		ver = append(ver, guard, p.Verify+" || exit $?")
	}
	s.Prepare, s.Verify = strings.Join(prep, "\n"), strings.Join(ver, "\n")
	return s
}

// Skills returns the agent skills of the pack: skill name to its files,
// path within the skill to content. They are the directories under
// .claude/skills/ in the pack's files. The pack writes them into the
// repository for people's own Claude Code; the service gives the agents
// the copies from the binary, because the agents do not read the
// repository's .claude settings.
func (p *Pack) Skills() (map[string]map[string][]byte, error) {
	files, err := p.Files()
	if err != nil {
		return nil, err
	}
	out := map[string]map[string][]byte{}
	for name, raw := range files {
		rest, ok := strings.CutPrefix(name, ".claude/skills/")
		if !ok {
			continue
		}
		skill, file, ok := strings.Cut(rest, "/")
		if !ok {
			continue
		}
		if out[skill] == nil {
			out[skill] = map[string][]byte{}
		}
		out[skill][file] = raw
	}
	return out, nil
}

// PackSkillMark is the file that marks a skill directory as written by
// SyncSkills, so it is replaced or removed with the packs.
const PackSkillMark = ".maestro-pack"

// SyncSkills makes dir (the agents' skills directory) hold the skills of
// the given packs. Skills it wrote before for packs that are no longer
// switched on are removed. A skill the owner installed under the same
// name is left alone.
func SyncSkills(dir string, list []*Pack) error {
	want := map[string]map[string][]byte{}
	for _, p := range list {
		skills, err := p.Skills()
		if err != nil {
			return err
		}
		for name, files := range skills {
			want[name] = files
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		mark := filepath.Join(dir, e.Name(), PackSkillMark)
		if _, ok := want[e.Name()]; !ok && e.IsDir() && fileExists(mark) {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	for name, files := range want {
		target := filepath.Join(dir, name)
		if info, err := os.Lstat(target); err == nil && (!info.IsDir() || !fileExists(filepath.Join(target, PackSkillMark))) {
			continue // the owner's own
		}
		if err := os.RemoveAll(target); err != nil {
			return err
		}
		for file, raw := range files {
			path := filepath.Join(target, filepath.FromSlash(file))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				return err
			}
		}
		if err := os.WriteFile(filepath.Join(target, PackSkillMark), []byte("written by maestro for a conventions pack\n"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}
