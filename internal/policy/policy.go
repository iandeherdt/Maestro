// Package policy decides whether a set of commits may be published (SPEC
// sections 15.3 and 15.4).
//
// It is a pure function of the diff. The diff itself is computed elsewhere,
// from objects imported into the mirror, never from a checkout. Deny rules
// stop publication. Flag rules are shown to the reviewer at the top of the
// pull request.
package policy

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/iandeherdt/Maestro/internal/config"
)

// Git file modes.
const (
	ModeFile    = 0o100644
	ModeExec    = 0o100755
	ModeSymlink = 0o120000
	ModeGitlink = 0o160000
)

// Change is one changed path between the base commit and the new head.
type Change struct {
	Path    string
	OldPath string // set for renames and copies
	Status  byte   // 'A' added, 'M' modified, 'D' deleted, 'R' renamed, 'C' copied, 'T' type changed
	OldMode uint32 // 0 when the path did not exist
	NewMode uint32 // 0 when the path is deleted
	Binary  bool
	NewSize int64
	// DiffBytes is the size of this path's part of the diff.
	DiffBytes int64
	Added     []string
	Removed   []string
}

// Commit is the metadata of one new commit.
type Commit struct {
	SHA            string
	AuthorName     string
	AuthorEmail    string
	CommitterName  string
	CommitterEmail string
	Message        string
}

// Input is everything the policy looks at.
type Input struct {
	Changes []Change
	Commits []Commit
	// BasePaths are the paths that exist at the base commit.
	BasePaths []string
	// BotName and BotEmail are the identity commits are expected to carry.
	BotName, BotEmail string
	// HeldSecrets are the credential values the control zone holds. They
	// are searched for by exact match and never appear in a finding.
	HeldSecrets []string
	// HistoryAdded are the lines and file names that the commits add one
	// by one, also those a later commit removes again. The change from
	// base to head does not show them, and a push sends every commit.
	HistoryAdded []string
}

// Finding is one rule result. Detail never contains file content.
type Finding struct {
	Rule   string // "D1".."D8", "F1".."F8"
	Path   string
	Detail string
}

func (f Finding) String() string {
	if f.Path == "" {
		return f.Rule + " " + f.Detail
	}
	return f.Rule + " " + f.Path + ": " + f.Detail
}

// Result is the outcome of the policy.
type Result struct {
	Denied []Finding
	Flags  []Finding
}

// OK reports whether nothing was denied.
func (r Result) OK() bool { return len(r.Denied) == 0 }

// maxFindings bounds the report, so a huge hostile diff cannot produce a
// huge comment.
const maxFindings = 100

// protected reports whether a path is on the built-in protected list (D1).
func protected(path string) bool {
	lower := strings.ToLower(path)
	segs := strings.Split(lower, "/")
	base := segs[len(segs)-1]
	for _, s := range segs {
		// .git, .github, .gitmodules, .gitattributes, .gitlab-ci.yml ...
		if strings.HasPrefix(s, ".git") && s != ".gitignore" {
			return true
		}
		// Claude Code reads these from nested directories too.
		if s == ".claude" {
			return true
		}
	}
	switch base {
	case ".mcp.json", "codeowners", "workflow.md", ".env":
		return true
	}
	if strings.HasPrefix(base, ".env.") {
		return true
	}
	switch segs[0] {
	case ".circleci", ".buildkite", ".forgejo", ".woodpecker":
		// .gitea/ is covered by .git above.
		return len(segs) > 1
	case "azure-pipelines.yml", "jenkinsfile", ".woodpecker.yml", ".woodpecker.yaml", ".drone.yml":
		return len(segs) == 1
	}
	return false
}

// badPath reports why a path is not acceptable at all, or "".
func badPath(path string) string {
	if path == "" || strings.HasPrefix(path, "/") || strings.Contains(path, "\\") {
		return "malformed path"
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "malformed path"
		}
	}
	for _, r := range path {
		// Cc: control characters. Cf: invisible formatting characters,
		// including the bidirectional overrides that disguise file names.
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || r == unicode.ReplacementChar {
			return "path contains a control or invisible character"
		}
	}
	return ""
}

// fold maps paths that would collide on a case-insensitive or
// Unicode-normalizing filesystem to the same key.
func fold(path string) string { return strings.ToLower(norm.NFC.String(path)) }

// printable makes a path safe to show in a comment or a log line.
func printable(path string) string {
	var b strings.Builder
	n := 0
	for _, r := range path {
		if n >= 200 {
			b.WriteString("…")
			break
		}
		n++
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || r == unicode.ReplacementChar {
			fmt.Fprintf(&b, "\\u%04x", r)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func validMode(m uint32) bool {
	return m == 0 || m == ModeFile || m == ModeExec || m == ModeSymlink || m == ModeGitlink
}

// Evaluate applies the deny rules and the flag rules.
func Evaluate(cfg config.Policy, in Input) Result {
	var res Result
	deny := func(rule, path, detail string) {
		if len(res.Denied) < maxFindings {
			res.Denied = append(res.Denied, Finding{rule, printable(path), detail})
		}
	}
	flag := func(rule, path, detail string) {
		if len(res.Flags) < maxFindings {
			res.Flags = append(res.Flags, Finding{rule, printable(path), detail})
		}
	}

	// reported are the kinds of credential found in the change from base
	// to head. The history is only asked about the other kinds.
	reported := map[string]bool{}

	// D5, totals.
	if int64(len(in.Changes)) > cfg.MaxChangedFiles {
		deny("D5", "", fmt.Sprintf("%d files changed, the limit is %d", len(in.Changes), cfg.MaxChangedFiles))
	}
	var total int64
	for _, c := range in.Changes {
		total += c.DiffBytes
	}
	if total > cfg.MaxDiffBytes {
		deny("D5", "", fmt.Sprintf("the diff is %d bytes, the limit is %d", total, cfg.MaxDiffBytes))
	}

	// D8, collisions. Paths present after the change, by folded form.
	deleted := map[string]bool{}
	for _, c := range in.Changes {
		if c.Status == 'D' {
			deleted[c.Path] = true
		}
		if c.Status == 'R' {
			deleted[c.OldPath] = true
		}
	}
	folded := map[string]string{}
	for _, p := range in.BasePaths {
		if !deleted[p] {
			folded[fold(p)] = p
		}
	}

	for _, c := range in.Changes {
		paths := []string{c.Path}
		if c.OldPath != "" && c.OldPath != c.Path {
			paths = append(paths, c.OldPath)
		}

		// D8
		if why := badPath(c.Path); why != "" {
			deny("D8", c.Path, why)
		}
		if c.Status != 'D' {
			if prev, ok := folded[fold(c.Path)]; ok && prev != c.Path {
				deny("D8", c.Path, "differs only by case or Unicode form from "+printable(prev))
			}
			folded[fold(c.Path)] = c.Path
		}

		// D1. A rename away from a protected path is a change to it too.
		for _, p := range paths {
			if protected(p) || matchAny(cfg.DenyPaths, p) {
				deny("D1", p, "protected path")
			}
		}

		// D2, D3, D4
		if c.NewMode == ModeGitlink || c.OldMode == ModeGitlink {
			deny("D2", c.Path, "submodule entry")
		}
		if c.NewMode == ModeSymlink && !cfg.AllowSymlinks {
			deny("D3", c.Path, "symbolic link added or changed")
		}
		if !validMode(c.NewMode) || !validMode(c.OldMode) {
			deny("D4", c.Path, "unexpected file mode")
		}

		// D5, per file. D6.
		binaryHere := c.Binary && matchAny(cfg.BinaryPaths, c.Path)
		limit := cfg.MaxFileBytes
		if binaryHere {
			limit = cfg.BinaryMaxFileBytes
		}
		if c.Status != 'D' && c.NewSize > limit {
			deny("D5", c.Path, fmt.Sprintf("file is %d bytes, the limit is %d", c.NewSize, limit))
		}
		if c.Binary && c.Status != 'D' && !cfg.AllowBinary && !binaryHere {
			deny("D6", c.Path, "binary file added or modified")
		}

		// D7
		if cfg.SecretScan {
			if c.Status != 'D' {
				for _, hit := range scanText(c.Path, in.HeldSecrets) {
					reported[hit] = true
					deny("D7", "", "file name looks like a credential ("+hit+")")
				}
			}
			for i, line := range c.Added {
				for _, hit := range scanText(line, in.HeldSecrets) {
					reported[hit] = true
					deny("D7", c.Path, fmt.Sprintf("added line %d looks like a credential (%s)", i+1, hit))
				}
			}
		}

		flagChange(cfg, c, flag)
	}

	if cfg.SecretScan {
		for _, line := range in.HistoryAdded {
			for _, hit := range scanText(line, in.HeldSecrets) {
				if !reported[hit] {
					reported[hit] = true
					deny("D7", "", "an earlier commit adds something that looks like a credential, and a later one removes it; "+
						"the earlier commit would be pushed too. Make the change again from the base commit, without it ("+hit+")")
				}
			}
		}
	}

	for _, cm := range in.Commits {
		if cfg.SecretScan {
			for _, hit := range scanText(cm.Message, in.HeldSecrets) {
				deny("D7", "", "a commit message looks like it contains a credential ("+hit+")")
			}
		}
		// F6, informational.
		if in.BotEmail != "" && (cm.AuthorEmail != in.BotEmail || cm.CommitterEmail != in.BotEmail ||
			cm.AuthorName != in.BotName || cm.CommitterName != in.BotName) {
			flag("F6", "", "commit "+shortSHA(cm.SHA)+" does not carry the service's commit identity")
		}
	}

	sort.SliceStable(res.Denied, func(a, b int) bool { return res.Denied[a].Rule < res.Denied[b].Rule })
	sort.SliceStable(res.Flags, func(a, b int) bool { return res.Flags[a].Rule < res.Flags[b].Rule })
	return res
}

var hexRE = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

func shortSHA(s string) string {
	if !hexRE.MatchString(s) {
		return "(unknown)"
	}
	return s[:7]
}

var (
	skipMarkerRE = regexp.MustCompile(`(?i)(\b(it|test|describe|context|suite)\.(skip|only|todo)\b|\bx(it|describe|test)\s*\(|\bf(it|describe)\s*\(|\.only\s*\(|\bt\.Skip(f|Now)?\(|@Ignore\b|@Disabled\b|\[Ignore\b|\[Explicit\b|@pytest\.mark\.(skip|skipif|xfail)|@unittest\.skip|\bpytest\.skip\(|#\[ignore\]|\bXCTSkip|\bpending\s*\(|\bskip\s*\()`)
	assertRE     = regexp.MustCompile(`(?i)(\bassert\w*\b|\bexpect\s*\(|\.should\b|\bshould\.|\bXCTAssert\w*|\bt\.(Error|Fatal|Fail)\w*\(|\brequire\.\w+\(|\bAssert\.\w+)`)

	qualityFiles = []string{".eslintrc*", "eslint.config.*", ".prettierrc*", "prettier.config.*", "tsconfig*.json",
		".golangci.yml", ".golangci.yaml", "setup.cfg", ".flake8", "mypy.ini", "ruff.toml", ".ruff.toml", ".pylintrc",
		".rubocop.yml", ".editorconfig", "jest.config.*", "vitest.config.*", ".nycrc*", "codecov.yml", ".codecov.yml",
		".coveragerc", "sonar-project.properties", ".swiftlint.yml", ".clang-format", ".clang-tidy", "phpstan.neon",
		".stylelintrc*", "biome.json", "tox.ini", "pytest.ini"}
	dependencyFiles = []string{"package.json", "package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml",
		"bun.lockb", "go.mod", "go.sum", "Cargo.toml", "Cargo.lock", "requirements*.txt", "Pipfile", "Pipfile.lock",
		"poetry.lock", "uv.lock", "pyproject.toml", "Gemfile", "Gemfile.lock", "composer.json", "composer.lock",
		"Podfile", "Podfile.lock", "Package.swift", "Package.resolved", "*.csproj", "packages.lock.json",
		"Packages/manifest.json", "Packages/packages-lock.json", "build.gradle", "build.gradle.kts", "pom.xml",
		"vendor/**"}
	buildFiles = []string{"Makefile", "GNUmakefile", "*.mk", "package.json", "*.sh", "*.bash", "*.ps1", "*.bat", "*.cmd",
		"Dockerfile*", "docker-compose*.yml", "docker-compose*.yaml", "**/*.xcodeproj/**", "**/*.xcworkspace/**",
		"*.xcscheme", "*.pbxproj", "ProjectSettings/**", "Taskfile.yml", "justfile", "Rakefile", "*.gradle",
		"CMakeLists.txt", "*.cmake", "fastlane/**"}
	instructionFiles = []string{"CLAUDE.md", "CLAUDE.local.md", "AGENTS.md", "GEMINI.md", ".cursorrules", ".cursor/**"}
)

// outsideRules find added code that does something outside the program
// itself. Honest code does these things too, so they are shown to the
// reviewer and deny nothing. Their purpose is that code written under a
// prompt injection (a miner, a downloader, something that sends files
// away) is not buried in an ordinary-looking diff.
var outsideRules = []struct {
	does      string
	skipLocks bool
	re        *regexp.Regexp
}{
	{"open a network connection", false, regexp.MustCompile(`(?i)(\b(https?|wss?|ftp|ssh|stratum\+tcp)://[^\s"'<>)]*[a-z0-9]|\bfetch\s*\(|\bXMLHttpRequest\b|\baxios\b|\bhttp\.(Get|Post|Head|NewRequest)\w*\(|\bnet\.(Dial|Listen)\w*\(|\b(Http|Web|Tcp|Udp)Client\b|\bnew\s+(Web)?Socket\b|\bUnityWebRequest\b|\brequests\.(get|post|put|request)\(|\burllib\b|\bsocket\.(socket|create_connection)\(|\bnet/http\b|\b(curl|wget|nc)\s+-)`)},
	{"start another program", false, regexp.MustCompile(`(?i)(\bexec\.Command\w*\(|\bos/exec\b|\bchild_process\b|\b(spawn|execFile)(Sync)?\s*\(|\bexecSync\s*\(|\bProcess\.Start\b|\bProcessStartInfo\b|\bsubprocess\.\w+|\bos\.(system|popen|exec\w*)\(|\bRuntime\.getRuntime\(|\bProcessBuilder\b|\bShellExecute\w*|\bpopen\s*\(|(^|[^.\w])system\s*\()`)},
	{"run code that is built at run time", false, regexp.MustCompile(`(?i)(\beval\s*\(|(^|[^.\w])exec\s*\(|\bnew\s+Function\s*\(|\bAssembly\.Load\w*\(|\bReflection\.Emit\b|\b__import__\s*\(|\bimportlib\.import_module\(|\bdlopen\s*\(|\bDllImport\b|\bvm\.runIn\w+\(|\bplugin\.Open\(|\bCSharpCodeProvider\b|\bcompile\s*\([^)]*exec)`)},
	{"hold a long encoded block", true, regexp.MustCompile(`([A-Za-z0-9+/]{200,}={0,2}|(\\x[0-9a-fA-F]{2}){40,}|\b[0-9a-fA-F]{256,}\b)`)},
}

// proseFiles are files in which an address is only text.
var proseFiles = []string{"*.md", "*.markdown", "*.txt", "*.rst", "*.adoc", "LICENSE*", "NOTICE*", "CHANGELOG*"}

func looksLikeTest(path string) bool {
	l := strings.ToLower(path)
	return strings.Contains(l, "test") || strings.Contains(l, "spec")
}

func count(re *regexp.Regexp, lines []string) int {
	n := 0
	for _, l := range lines {
		if re.MatchString(l) {
			n++
		}
	}
	return n
}

func flagChange(cfg config.Policy, c Change, flag func(rule, path, detail string)) {
	inTests := matchAny(cfg.TestPaths, c.Path) || (c.OldPath != "" && matchAny(cfg.TestPaths, c.OldPath))

	// F1
	if inTests {
		switch {
		case c.Status == 'D':
			flag("F1", c.Path, "test file deleted")
		case len(c.Removed) > len(c.Added):
			flag("F1", c.Path, fmt.Sprintf("test file shrank by %d lines", len(c.Removed)-len(c.Added)))
		}
	}
	if inTests || looksLikeTest(c.Path) {
		if n := count(skipMarkerRE, c.Added) - count(skipMarkerRE, c.Removed); n > 0 {
			flag("F1", c.Path, fmt.Sprintf("%d skip or only marker(s) added", n))
		}
		if removed, added := count(assertRE, c.Removed), count(assertRE, c.Added); removed > added {
			flag("F1", c.Path, fmt.Sprintf("%d assertion(s) removed without replacement", removed-added))
		}
	}

	for _, p := range uniquePaths(c) {
		if matchAny(qualityFiles, p) {
			flag("F2", p, "lint, format, type-check or coverage configuration changed")
		}
		if matchAny(dependencyFiles, p) {
			flag("F3", p, "dependency manifest or lockfile changed")
		}
		if matchAny(buildFiles, p) {
			flag("F4", p, "build or task script changed")
		}
		if matchAny(instructionFiles, p) {
			flag("F5", p, "agent instruction file changed")
		}
		if matchAny(cfg.FlagPaths, p) {
			flag("F7", p, "path listed in policy.flag_paths")
		}
	}
	// F8
	if !c.Binary && !matchAny(proseFiles, c.Path) {
		var what []string
		for _, r := range outsideRules {
			if r.skipLocks && matchAny(dependencyFiles, c.Path) {
				continue
			}
			if n := count(r.re, c.Added); n > 0 {
				what = append(what, fmt.Sprintf("%d added line(s) that %s", n, r.does))
			}
		}
		if len(what) > 0 {
			flag("F8", c.Path, "reaches outside the program: "+strings.Join(what, ", "))
		}
	}
	// A file that becomes executable is a script, whatever its name.
	if c.NewMode == ModeExec && c.OldMode != ModeExec && !matchAny(buildFiles, c.Path) {
		flag("F4", c.Path, "file made executable")
	}
}

func uniquePaths(c Change) []string {
	if c.OldPath != "" && c.OldPath != c.Path {
		return []string{c.Path, c.OldPath}
	}
	return []string{c.Path}
}

// Feedback renders denials for the agent's next attempt.
func (r Result) Feedback() []string {
	out := make([]string, len(r.Denied))
	for i, f := range r.Denied {
		out[i] = f.String()
	}
	return out
}
