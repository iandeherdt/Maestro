// Package skills keeps the skills and agent files the owner of an
// installation added for the working agents (SPEC 26).
//
// A skill is a directory with a SKILL.md, an agent file a Markdown file
// with a name and a description; both are Claude Code's own formats. They
// are instructions with the weight of the prompt, so only a person adds
// them, with a command on the machine, after seeing what they are. The
// service mounts the store read-only into the agent's container.
package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// MaxFiles and MaxBytes bound one skill.
	MaxFiles = 200
	MaxBytes = 5 << 20
	// maxFound bounds what one source may offer.
	maxFound = 50
	// maxDepth is how deep below the source's root a skill is looked for.
	maxDepth = 4
)

// Kind says what an entry is.
type Kind string

const (
	Skill Kind = "skill"
	Agent Kind = "agent"
)

// Entry is one installed skill or agent file.
type Entry struct {
	Kind        Kind   `json:"kind"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Source is what the person typed, Commit the commit it was taken at
	// (empty for a directory on this machine).
	Source string `json:"source"`
	Commit string `json:"commit,omitempty"`
	Added  string `json:"added"`
}

// Found is a skill or agent file in a fetched source, not yet installed.
type Found struct {
	Entry
	// Path is the directory of a skill or the file of an agent.
	Path string
	// Files are the files of a skill, relative to Path. Scripts are the
	// ones a skill could have the agent run.
	Files, Scripts []string
	Bytes          int64
	// Settings are the front matter fields that change what the agent
	// runs or may do, beyond the instructions: hooks, MCP servers,
	// tools, the model. The person sees them before adding.
	Settings []string
}

// powerFields are the front matter fields of Claude Code skills and agent
// files that start programs or change the agent's tools, permissions or
// model.
var powerFields = []string{"allowed-tools", "disallowed-tools", "tools", "disallowedTools", "hooks",
	"mcpServers", "permissionMode", "model", "effort", "context", "agent", "skills", "memory",
	"isolation", "background", "maxTurns", "initialPrompt"}

// MaxDescription is the longest description a skill may have in the
// Agent Skills format; Claude Code cuts the list of skills off near it.
const MaxDescription = 1024

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Store is the directory the entries are kept in.
type Store struct{ Dir string }

// SkillsDir and AgentsDir are what the agent's container gets.
func (s Store) SkillsDir() string { return filepath.Join(s.Dir, "skills") }
func (s Store) AgentsDir() string { return filepath.Join(s.Dir, "agents") }
func (s Store) index() string     { return filepath.Join(s.Dir, "installed.json") }

// Ensure creates the directories. They are readable by everyone on the
// machine, because the user inside the container is not the owner.
func (s Store) Ensure() error {
	for _, d := range []string{s.Dir, s.SkillsDir(), s.AgentsDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// List returns the installed entries, skills first, by name.
func (s Store) List() ([]Entry, error) {
	raw, err := os.ReadFile(s.index())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var es []Entry
	if err := json.Unmarshal(raw, &es); err != nil {
		return nil, fmt.Errorf("%s is damaged: %w", s.index(), err)
	}
	sort.Slice(es, func(i, j int) bool {
		if es[i].Kind != es[j].Kind {
			return es[i].Kind == Skill
		}
		return es[i].Name < es[j].Name
	})
	return es, nil
}

func (s Store) save(es []Entry) error {
	raw, _ := json.MarshalIndent(es, "", "  ")
	tmp := s.index() + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.index())
}

func (s Store) place(e Entry) string {
	if e.Kind == Agent {
		return filepath.Join(s.AgentsDir(), e.Name+".md")
	}
	return filepath.Join(s.SkillsDir(), e.Name)
}

// Install copies what was found into the store, replacing an entry of the
// same kind and name.
func (s Store) Install(f Found, now time.Time) error {
	if err := s.Ensure(); err != nil {
		return err
	}
	if !nameRE.MatchString(f.Name) {
		return fmt.Errorf("%q is not a usable name", f.Name)
	}
	dst := s.place(f.Entry)
	tmp := dst + ".new"
	os.RemoveAll(tmp)
	var err error
	if f.Kind == Agent {
		err = copyFile(f.Path, tmp)
	} else {
		if err = os.Mkdir(tmp, 0o755); err == nil {
			for _, rel := range f.Files {
				if err = os.MkdirAll(filepath.Dir(filepath.Join(tmp, rel)), 0o755); err != nil {
					break
				}
				if err = copyFile(filepath.Join(f.Path, rel), filepath.Join(tmp, rel)); err != nil {
					break
				}
			}
		}
	}
	if err != nil {
		os.RemoveAll(tmp)
		return err
	}
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	es, err := s.List()
	if err != nil {
		return err
	}
	e := f.Entry
	e.Added = now.UTC().Format(time.RFC3339)
	kept := es[:0]
	for _, old := range es {
		if old.Kind != e.Kind || old.Name != e.Name {
			kept = append(kept, old)
		}
	}
	return s.save(append(kept, e))
}

// Remove deletes every entry with the name and reports how many there
// were.
func (s Store) Remove(name string) (int, error) {
	es, err := s.List()
	if err != nil {
		return 0, err
	}
	var kept []Entry
	n := 0
	for _, e := range es {
		if e.Name != name {
			kept = append(kept, e)
			continue
		}
		if err := os.RemoveAll(s.place(e)); err != nil {
			return n, err
		}
		n++
	}
	if n == 0 {
		return 0, nil
	}
	return n, s.save(kept)
}

// copyFile copies a regular file. The copy is readable by everyone and
// executable when the original was.
func copyFile(src, dst string) error {
	st, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st.Mode()&0o111 != 0 {
		mode = 0o755
	}
	if err := os.WriteFile(dst, raw, mode); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// front reads the name and description at the top of a skill or agent
// file.
func front(path string) (name, description string, settings []string, err error) {
	st, err := os.Lstat(path)
	if err != nil {
		return "", "", nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > 1<<20 {
		return "", "", nil, fmt.Errorf("%s is not a regular file of a normal size", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", nil, err
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return "", "", nil, errors.New("no front matter")
	}
	head, _, ok := strings.Cut(text[4:], "\n---")
	if !ok {
		return "", "", nil, errors.New("the front matter is not closed")
	}
	var m struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(head), &m); err != nil {
		return "", "", nil, fmt.Errorf("the front matter is not valid: %w", err)
	}
	var all map[string]any
	_ = yaml.Unmarshal([]byte(head), &all)
	for _, k := range powerFields {
		if _, ok := all[k]; ok {
			settings = append(settings, k)
		}
	}
	return strings.TrimSpace(m.Name), strings.Join(strings.Fields(m.Description), " "), settings, nil
}

// descriptionProblem says what is wrong with a skill's description, if
// anything: the Agent Skills format allows 1024 characters and no tags.
func descriptionProblem(desc string) error {
	if desc == "" {
		return errors.New("SKILL.md has no description, so the agent would never use it")
	}
	if n := len([]rune(desc)); n > MaxDescription {
		return fmt.Errorf("the description is %d characters; at most %d, or it is cut off in the agent's list of skills", n, MaxDescription)
	}
	if strings.ContainsAny(desc, "<>") {
		return errors.New("the description holds < or >, which the Agent Skills format does not allow")
	}
	return nil
}

var scriptRE = regexp.MustCompile(`(?i)\.(sh|bash|zsh|py|js|mjs|cjs|ts|rb|pl|php|ps1|exe|bin)$`)

// skillAt describes the skill in dir.
func skillAt(dir string) (Found, error) {
	name, desc, settings, err := front(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return Found{}, fmt.Errorf("SKILL.md: %w", err)
	}
	if name == "" {
		name = strings.ToLower(filepath.Base(dir))
	}
	f := Found{Entry: Entry{Kind: Skill, Name: name, Description: desc}, Path: dir, Settings: settings}
	if !nameRE.MatchString(name) {
		return f, fmt.Errorf("the name %q has to be lower-case letters, digits and dashes", name)
	}
	if err := descriptionProblem(desc); err != nil {
		return f, err
	}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is a link or a special file; a skill may only hold ordinary files", rel)
		}
		f.Files = append(f.Files, rel)
		f.Bytes += info.Size()
		if info.Mode()&0o111 != 0 || scriptRE.MatchString(rel) {
			f.Scripts = append(f.Scripts, rel)
		}
		if len(f.Files) > MaxFiles || f.Bytes > MaxBytes {
			return fmt.Errorf("it is larger than %d files or %d MB", MaxFiles, MaxBytes>>20)
		}
		return nil
	})
	return f, err
}

// Discover finds the skills and agent files below root. A problem with
// one of them is returned next to the ones that are fine.
func Discover(root string) (found []Found, problems []string) {
	if _, err := os.Lstat(filepath.Join(root, "SKILL.md")); err == nil {
		f, err := skillAt(root)
		if err != nil {
			return nil, []string{fmt.Sprintf("%s: %v", f.Name, err)}
		}
		return []Found{f}, nil
	}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || len(found) >= maxFound {
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(root, p)
		depth := strings.Count(rel, string(filepath.Separator))
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" || depth >= maxDepth {
				return filepath.SkipDir
			}
			if _, err := os.Lstat(filepath.Join(p, "SKILL.md")); err == nil && rel != "." {
				f, err := skillAt(p)
				if err != nil {
					problems = append(problems, fmt.Sprintf("%s: %v", rel, err))
				} else {
					found = append(found, f)
				}
				return filepath.SkipDir
			}
			return nil
		}
		// An agent file lives in a directory called agents.
		if filepath.Base(filepath.Dir(p)) != "agents" || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		name, desc, settings, err := front(p)
		if err != nil || name == "" || desc == "" {
			return nil // a README or another note, not an agent file
		}
		if !nameRE.MatchString(name) {
			problems = append(problems, fmt.Sprintf("%s: the name %q has to be lower-case letters, digits and dashes", rel, name))
			return nil
		}
		info, _ := d.Info()
		found = append(found, Found{Entry: Entry{Kind: Agent, Name: name, Description: desc}, Path: p, Bytes: info.Size(), Settings: settings})
		return nil
	})
	sort.Slice(found, func(i, j int) bool {
		if found[i].Kind != found[j].Kind {
			return found[i].Kind == Skill
		}
		return found[i].Name < found[j].Name
	})
	return found, problems
}

// Source is where skills come from.
type Source struct {
	// Local is a directory on this machine; otherwise URL is a git
	// repository reached over https.
	Local, URL string
	// Ref is a branch, a tag or a commit; empty means the default branch.
	Ref string
	// Sub is a directory inside the source.
	Sub string
}

var (
	shortRE  = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9-]{0,38})/([A-Za-z0-9._-]{1,100})((?:/[A-Za-z0-9._-]+)*)$`)
	refRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,200}$`)
	commitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
	subRE    = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)
)

// Parse reads what the person typed: a directory, owner/repo with an
// optional path, or an https address of a git repository.
func Parse(arg, ref, sub, home string) (Source, error) {
	src := Source{Ref: ref, Sub: strings.Trim(sub, "/")}
	if ref != "" && (!refRE.MatchString(ref) || strings.Contains(ref, "..")) {
		return src, fmt.Errorf("%q is not a branch, tag or commit", ref)
	}
	switch {
	case arg == "":
		return src, errors.New("say where the skills are")
	case strings.HasPrefix(arg, "~/"):
		src.Local = filepath.Join(home, arg[2:])
	case strings.HasPrefix(arg, "/"), strings.HasPrefix(arg, "./"), strings.HasPrefix(arg, "../"), arg == ".":
		abs, err := filepath.Abs(arg)
		if err != nil {
			return src, err
		}
		src.Local = abs
	case strings.HasPrefix(arg, "https://"):
		if strings.ContainsAny(arg, " \t\n'\"\\") {
			return src, errors.New("that address has characters an address cannot have")
		}
		src.URL = arg
	default:
		m := shortRE.FindStringSubmatch(arg)
		if m == nil {
			return src, fmt.Errorf("%q is not a directory (start it with ./, / or ~/), not owner/repo and not an https address", arg)
		}
		src.URL = "https://github.com/" + m[1] + "/" + strings.TrimSuffix(m[2], ".git") + ".git"
		if m[3] != "" {
			if src.Sub != "" {
				return src, errors.New("give the directory once: after the repository or with --path")
			}
			src.Sub = strings.Trim(m[3], "/")
		}
	}
	if src.Sub != "" && (!subRE.MatchString(src.Sub) || strings.Contains(src.Sub, "..")) {
		return src, fmt.Errorf("%q is not a directory inside the source", src.Sub)
	}
	if src.Local != "" && src.Ref != "" {
		return src, errors.New("--ref is for a git repository, not for a directory on this machine")
	}
	return src, nil
}

// fileProtocol is "never": a repository is fetched over https only. A test
// sets it to fetch from a directory.
var fileProtocol = "never"

// Fetch makes the source available as a directory and returns the commit
// for a repository. cleanup removes what was downloaded.
func Fetch(ctx context.Context, src Source) (root, commit string, cleanup func(), err error) {
	cleanup = func() {}
	if src.Local != "" {
		root = filepath.Join(src.Local, filepath.FromSlash(src.Sub))
		st, err := os.Stat(root)
		if err != nil || !st.IsDir() {
			return "", "", cleanup, fmt.Errorf("%s is not a directory", root)
		}
		return root, "", cleanup, nil
	}
	tmp, err := os.MkdirTemp("", "maestro-skills-")
	if err != nil {
		return "", "", cleanup, err
	}
	cleanup = func() { os.RemoveAll(tmp) }
	git := func(args ...string) (string, error) {
		// Nothing from the person's own git configuration that could run
		// a program for this repository, and no question on the terminal.
		base := []string{"-C", tmp, "-c", "core.hooksPath=/dev/null", "-c", "protocol.file.allow=" + fileProtocol, "-c", "protocol.ext.allow=never"}
		cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err = git("init", "--quiet"); err != nil {
		return "", "", cleanup, err
	}
	ref := src.Ref
	if ref == "" {
		ref = "HEAD"
	}
	if _, err = git("fetch", "--quiet", "--depth", "1", "--no-tags", "--", src.URL, ref); err != nil {
		return "", "", cleanup, fmt.Errorf("%s could not be fetched at %s: %w", src.URL, ref, err)
	}
	if _, err = git("checkout", "--quiet", "--detach", "FETCH_HEAD"); err != nil {
		return "", "", cleanup, err
	}
	if commit, err = git("rev-parse", "HEAD"); err != nil || !commitRE.MatchString(commit) {
		return "", "", cleanup, fmt.Errorf("the commit could not be read: %v", err)
	}
	root = filepath.Join(tmp, filepath.FromSlash(src.Sub))
	st, err := os.Lstat(root)
	if err != nil || !st.IsDir() {
		return "", "", cleanup, fmt.Errorf("the repository has no directory %s", src.Sub)
	}
	// A link on the way must not lead out of what was downloaded.
	real, err1 := filepath.EvalSymlinks(root)
	base, err2 := filepath.EvalSymlinks(tmp)
	if err1 != nil || err2 != nil || (real != base && !strings.HasPrefix(real, base+string(filepath.Separator))) {
		return "", "", cleanup, fmt.Errorf("%s leads out of the repository", src.Sub)
	}
	return root, commit, cleanup, nil
}
