// Package pins finds the tool versions that the sandbox image and the
// convention packs pin, and looks up whether a newer release exists.
//
// Most pins are found from their shape, so a new pack is covered without
// changes here: an image reference after FROM, COPY --from=, container: or
// image:, a literal npm install of name@version, a git clone of a tag, and
// a download of a GitHub release, and a Blender download. A version held in a constant or a variable is marked
// with a comment on the line above it, or on the same line:
//
//	// pin: npm @anthropic-ai/claude-code
//	const ClaudeVersion = "2.1.330"
package pins

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Kinds of pin, each with its own lookup.
const (
	KindImage   = "image"
	KindNPM     = "npm"
	KindGit     = "git"
	KindBlender = "blender"
)

// Place is a line of a file that holds a pin.
type Place struct {
	File string
	Line int
}

// Pin is one pinned version of one tool, with every place that names it.
type Pin struct {
	Kind    string
	Name    string // image without tag, npm package, git URL, "blender"
	Version string // tag, version, git tag
	Commit  string // git only: the commit the tag must resolve to, if checked
	Places  []Place
}

// Key identifies a pin regardless of where it is written.
func (p Pin) Key() string { return p.Kind + " " + p.Name + " " + p.Version }

var (
	reImage   = regexp.MustCompile(`(?:\bFROM\s+|--from=|^\s*(?:container|image):\s*["']?)([a-z0-9][a-z0-9._\-]*(?:/[a-z0-9._\-]+)*):([A-Za-z0-9._\-]+)`)
	reNPM     = regexp.MustCompile(`\b(?:npm|pnpm)\s+(?:install|add|i)\b[^\n"]*`)
	reNPMArg  = regexp.MustCompile(`(?:^|\s)"?(@?[a-z0-9][a-z0-9._\-]*(?:/[a-z0-9._\-]+)?)@(\d[0-9A-Za-z.\-]*)`)
	reGit     = regexp.MustCompile(`git clone\b[^\n]*?--branch[ =]"?([A-Za-z0-9._\-]+)"?[^\n]*?\s(https://[^\s"';\\]+)`)
	reRelease = regexp.MustCompile(`(https://github\.com/[A-Za-z0-9._\-]+/[A-Za-z0-9._\-]+)/releases/download/([A-Za-z0-9._\-]+)/`)
	reCommit  = regexp.MustCompile(`git rev-parse HEAD\)"?\s*=\s*"?([0-9a-f]{40})`)
	reBlender = regexp.MustCompile(`\bblender-(\d+\.\d+\.\d+)-linux`)
	reMarker  = regexp.MustCompile(`\bpin:\s*(image|npm|git|blender)\s+(\S+)`)
	reQuoted  = regexp.MustCompile(`"(v?\d+(?:\.\d+)*[0-9A-Za-z.\-]*)"`)
)

// skipDir lists folders that hold no pins of the product.
var skipDir = map[string]bool{".git": true, "node_modules": true, "testdata": true, "vendor": true, "docs": true}

// scanned reports whether pins are looked for in the file. Prose and tests
// are left out; they are searched for mentions only.
func scanned(path string) bool {
	if ownFile(path) {
		return false
	}
	base := filepath.Base(path)
	if strings.HasPrefix(base, "Dockerfile") {
		return true
	}
	switch filepath.Ext(path) {
	case ".go", ".json", ".yml", ".yaml", ".sh":
		return true
	}
	return false
}

// Scan walks the repository at root and returns its pins, sorted by key.
// Each pin also lists the other files that mention its exact version, such
// as a CI workflow that installs the same release, so that an update can
// change every place at once.
func Scan(root string) ([]Pin, error) {
	files, err := listFiles(root)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*Pin{}
	for _, rel := range files {
		if !scanned(rel) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			return nil, err
		}
		for _, p := range scanText(rel, string(data)) {
			if have, ok := byKey[p.Key()]; ok {
				have.Places = append(have.Places, p.Places...)
				if have.Commit == "" {
					have.Commit = p.Commit
				}
				continue
			}
			cp := p
			byKey[p.Key()] = &cp
		}
	}
	pins := make([]Pin, 0, len(byKey))
	for _, p := range byKey {
		pins = append(pins, *p)
	}
	if err := addMentions(root, files, pins); err != nil {
		return nil, err
	}
	sort.Slice(pins, func(i, j int) bool { return pins[i].Key() < pins[j].Key() })
	return pins, nil
}

func listFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files, err
}

// unescape turns the \n and \" of a JSON or Go string back into text, so
// that a RUN line kept in a string reads as it does in the Dockerfile.
func unescape(s string) string {
	return strings.NewReplacer(`\\`+"\n", "\n", `\n`, "\n", `\"`, `"`, `\t`, " ").Replace(s)
}

func scanText(file, text string) []Pin {
	var out []Pin
	lines := strings.Split(text, "\n")
	var marker []string // from the line above
	for i, raw := range lines {
		place := []Place{{File: file, Line: i + 1}}
		line := unescape(raw)
		if m := reMarker.FindStringSubmatch(line); m != nil {
			marker = m[1:]
			if q := reQuoted.FindStringSubmatch(line[strings.Index(line, m[0])+len(m[0]):]); q != nil {
				out = append(out, Pin{Kind: marker[0], Name: marker[1], Version: q[1], Places: place})
				marker = nil
			}
			continue
		}
		if marker != nil {
			if q := reQuoted.FindStringSubmatch(line); q != nil {
				out = append(out, Pin{Kind: marker[0], Name: marker[1], Version: q[1], Places: place})
			}
			marker = nil
		}
		for _, m := range reImage.FindAllStringSubmatch(line, -1) {
			if strings.Contains(m[0], "$") || strings.Contains(m[2], "$") {
				continue
			}
			out = append(out, Pin{Kind: KindImage, Name: NormalizeImage(m[1]), Version: m[2], Places: place})
		}
		for _, cmd := range reNPM.FindAllString(line, -1) {
			for _, m := range reNPMArg.FindAllStringSubmatch(cmd, -1) {
				out = append(out, Pin{Kind: KindNPM, Name: m[1], Version: m[2], Places: place})
			}
		}
		for _, m := range reBlender.FindAllStringSubmatch(line, -1) {
			out = append(out, Pin{Kind: KindBlender, Name: "blender", Version: m[1], Places: place})
		}
		for _, m := range reRelease.FindAllStringSubmatch(line, -1) {
			out = append(out, Pin{Kind: KindGit, Name: m[1], Version: m[2], Places: place})
		}
		for _, m := range reGit.FindAllStringSubmatch(line, -1) {
			if strings.Contains(m[1], "$") {
				continue
			}
			p := Pin{Kind: KindGit, Name: strings.TrimSuffix(m[2], ".git"), Version: m[1], Places: place}
			// The commit check usually follows within the same RUN.
			rest := unescape(strings.Join(lines[i:min(i+8, len(lines))], "\n"))
			if c := reCommit.FindStringSubmatch(rest); c != nil {
				p.Commit = c[1]
			}
			out = append(out, p)
		}
	}
	return out
}

// NormalizeImage writes an image name in full: docker.io/library/node for
// node, docker.io/unityci/editor for unityci/editor.
func NormalizeImage(name string) string {
	first, _, found := strings.Cut(name, "/")
	if !found {
		return "docker.io/library/" + name
	}
	if !strings.ContainsAny(first, ".:") && first != "localhost" {
		return "docker.io/" + name
	}
	return name
}

// tagVersion is the release inside an image tag, 6000.3.29f1 in
// ubuntu-6000.3.29f1-base-4.0.0: the longest part between dashes, when it
// is long enough to mean this release.
func tagVersion(p Pin) string {
	if p.Kind != KindImage {
		return ""
	}
	best := ""
	for _, part := range strings.Split(p.Version, "-") {
		if len(part) > len(best) && strings.ContainsAny(part, "0123456789") {
			best = part
		}
	}
	if best == p.Version || !distinctive(best) || strings.Contains(best, "-") {
		return ""
	}
	return best
}

// ownFile reports whether a file is a test or part of this check, whose
// examples are not pins.
func ownFile(rel string) bool {
	return strings.HasSuffix(rel, "_test.go") ||
		strings.HasPrefix(rel, "internal/pins/") || strings.HasPrefix(rel, "tools/pincheck/")
}

// distinctive reports whether a version string is long enough that a file
// naming it almost surely means this pin, not some other number.
func distinctive(v string) bool {
	return len(v) >= 5 && strings.ContainsAny(v, ".-")
}

func addMentions(root string, files []string, pins []Pin) error {
	type want struct {
		pin    int
		needle string
	}
	var wants []want
	for i, p := range pins {
		if distinctive(p.Version) {
			wants = append(wants, want{i, p.Version})
		}
		if v := tagVersion(p); v != "" {
			wants = append(wants, want{i, v})
		}
		if p.Commit != "" {
			wants = append(wants, want{i, p.Commit})
		}
	}
	if len(wants) == 0 {
		return nil
	}
	for _, rel := range files {
		if ownFile(rel) {
			continue
		}
		f, err := os.Open(filepath.Join(root, rel))
		if err != nil {
			return err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		n := 0
		for sc.Scan() {
			n++
			line := sc.Text()
			for _, w := range wants {
				if !containsVersion(line, w.needle) || hasPlace(pins[w.pin], rel, n) {
					continue
				}
				pins[w.pin].Places = append(pins[w.pin].Places, Place{File: rel, Line: n})
			}
		}
		f.Close()
		if err := sc.Err(); err != nil {
			// A binary file with a very long line; it is not a place to edit.
			continue
		}
	}
	return nil
}

// containsVersion finds v in line where it is not part of a longer version,
// so 5.0.1 does not match 15.0.12.
func containsVersion(line, v string) bool {
	for start := 0; ; {
		i := strings.Index(line[start:], v)
		if i < 0 {
			return false
		}
		i += start
		before, after := byte(' '), byte(' ')
		if i > 0 {
			before = line[i-1]
		}
		if j := i + len(v); j < len(line) {
			after = line[j]
		}
		if !isVersionByte(before) && !isVersionByte(after) {
			return true
		}
		start = i + 1
	}
}

func isVersionByte(b byte) bool {
	return b >= '0' && b <= '9' || b == '.'
}

func hasPlace(p Pin, file string, line int) bool {
	for _, pl := range p.Places {
		if pl.File == file && pl.Line == line {
			return true
		}
	}
	return false
}
