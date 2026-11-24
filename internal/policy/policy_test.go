package policy

import (
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/config"
)

func cfg() config.Policy {
	return config.Policy{MaxChangedFiles: 50, MaxDiffBytes: 10000, MaxFileBytes: 5000, SecretScan: true,
		TestPaths: []string{"tests/**", "**/*_test.go"}}
}

func mod(path string, added ...string) Change {
	return Change{Path: path, Status: 'M', OldMode: ModeFile, NewMode: ModeFile, Added: added, NewSize: 100, DiffBytes: 100}
}

func add(path string) Change {
	return Change{Path: path, Status: 'A', NewMode: ModeFile, NewSize: 100, DiffBytes: 100}
}

func rules(fs []Finding) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Rule)
	}
	return strings.Join(out, ",")
}

func eval(c config.Policy, changes ...Change) Result {
	return Evaluate(c, Input{Changes: changes})
}

func TestOrdinaryChangePasses(t *testing.T) {
	r := eval(cfg(), mod("src/app.go", "x := 1"), add("docs/notes.md"), mod(".gitignore", "dist/"))
	if !r.OK() || len(r.Flags) != 0 {
		t.Fatalf("denied %v flags %v", r.Denied, r.Flags)
	}
}

func TestMatch(t *testing.T) {
	for _, c := range []struct {
		pat, path string
		want      bool
	}{
		{"*.sh", "run.sh", true},
		{"*.sh", "a/b/run.sh", true},
		{"*.sh", "a/run.sh.txt", false},
		{"tests/**", "tests/a/b.go", true},
		{"tests/**", "tests", true},
		{"tests/**", "src/tests/a.go", false},
		{"**/*.xcodeproj/**", "App.xcodeproj/project.pbxproj", true},
		{"**/*.xcodeproj/**", "ios/App.xcodeproj/x/y", true},
		{"a/?.txt", "a/b.txt", true},
		{"a/?.txt", "a/bc.txt", false},
		{"a/*.txt", "a/b/c.txt", false},
		{"a/*.txt", "a/b.txt/c", false},
		{"Makefile", "Makefile/x", false},
		{"Makefile", "MAKEFILE", true},
		{"dockerfile*", "svc/Dockerfile.prod", true},
		{"vendor/**", "vendor/x/y.go", true},
		{"vendor/**", "a/vendor/x.go", false},
	} {
		if got := Match(c.pat, c.path); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
}

func TestProtectedPathsAreDenied(t *testing.T) {
	denied := []string{
		".github/workflows/ci.yml", ".GitHub/workflows/ci.yml", "sub/.github/x.yml", ".gitlab-ci.yml",
		".gitmodules", ".gitattributes", "a/b/.gitattributes", ".git/config", "a/.git/hooks/pre-commit",
		".circleci/config.yml", ".buildkite/pipeline.yml", "azure-pipelines.yml", "Jenkinsfile",
		".gitea/workflows/ci.yml", ".forgejo/workflows/ci.yml", ".woodpecker/test.yml", ".woodpecker.yml", ".drone.yml",
		".claude/settings.json", "pkg/.claude/settings.local.json", ".mcp.json", "pkg/.mcp.json",
		"CODEOWNERS", "docs/CODEOWNERS", "WORKFLOW.md", "workflow.md", ".env", "svc/.env", "svc/.env.production",
		"secrets/prod.yaml",
	}
	c := cfg()
	c.DenyPaths = []string{"secrets/**"}
	for _, p := range denied {
		if r := eval(c, mod(p)); !strings.Contains(rules(r.Denied), "D1") {
			t.Errorf("%s was not denied: %v", p, r.Denied)
		}
	}
	allowed := []string{".gitignore", "a/.gitignore", "src/env.go", "docs/github.md", "src/jenkinsfile/readme.md",
		"claude/notes.md", "environment.yml", "src/.envrc"}
	for _, p := range allowed {
		if r := eval(c, mod(p)); !r.OK() {
			t.Errorf("%s was denied: %v", p, r.Denied)
		}
	}
}

func TestRenameAwayFromProtectedPathIsDenied(t *testing.T) {
	r := eval(cfg(), Change{Path: "old-ci.yml", OldPath: ".github/workflows/ci.yml", Status: 'R',
		OldMode: ModeFile, NewMode: ModeFile})
	if rules(r.Denied) != "D1" {
		t.Fatalf("got %v", r.Denied)
	}
}

func TestModesLinksAndSubmodules(t *testing.T) {
	c := cfg()
	for name, tc := range map[string]struct {
		ch   Change
		want string
	}{
		"submodule added":    {Change{Path: "lib", Status: 'A', NewMode: ModeGitlink}, "D2"},
		"submodule removed":  {Change{Path: "lib", Status: 'D', OldMode: ModeGitlink}, "D2"},
		"symlink added":      {Change{Path: "link", Status: 'A', NewMode: ModeSymlink}, "D3"},
		"file to symlink":    {Change{Path: "link", Status: 'T', OldMode: ModeFile, NewMode: ModeSymlink}, "D3"},
		"symlink retargeted": {Change{Path: "link", Status: 'M', OldMode: ModeSymlink, NewMode: ModeSymlink}, "D3"},
		"setuid":             {Change{Path: "tool", Status: 'A', NewMode: 0o104755}, "D4"},
		"odd mode":           {Change{Path: "tool", Status: 'A', NewMode: 0o100664}, "D4"},
		"symlink deleted":    {Change{Path: "link", Status: 'D', OldMode: ModeSymlink}, ""},
		"plain exec":         {Change{Path: "tool", Status: 'M', OldMode: ModeExec, NewMode: ModeExec}, ""},
	} {
		if got := rules(eval(c, tc.ch).Denied); got != tc.want {
			t.Errorf("%s: denied %q, want %q", name, got, tc.want)
		}
	}
	c.AllowSymlinks = true
	if r := eval(c, Change{Path: "link", Status: 'A', NewMode: ModeSymlink}); !r.OK() {
		t.Errorf("allow_symlinks: %v", r.Denied)
	}
}

func TestSizeLimits(t *testing.T) {
	c := cfg()
	c.MaxChangedFiles = 2
	if got := rules(eval(c, add("a"), add("b"), add("c")).Denied); got != "D5" {
		t.Errorf("file count: %q", got)
	}
	if !eval(c, add("a"), add("b")).OK() {
		t.Error("exactly at the file limit must pass")
	}
	big := add("a")
	big.DiffBytes = 6000
	big2 := add("b")
	big2.DiffBytes = 4001
	if got := rules(eval(c, big, big2).Denied); got != "D5" {
		t.Errorf("total diff: %q", got)
	}
	large := add("data.txt")
	large.NewSize = 5001
	if got := rules(eval(c, large).Denied); got != "D5" {
		t.Errorf("file size: %q", got)
	}
	large.NewSize = 5000
	if !eval(c, large).OK() {
		t.Error("exactly at the size limit must pass")
	}
	gone := Change{Path: "old.bin", Status: 'D', OldMode: ModeFile, Binary: true, NewSize: 0}
	if !eval(c, gone).OK() {
		t.Error("deleting a binary file must pass")
	}
}

func TestBinary(t *testing.T) {
	c := cfg()
	bin := add("logo.png")
	bin.Binary = true
	if got := rules(eval(c, bin).Denied); got != "D6" {
		t.Errorf("got %q", got)
	}
	c.AllowBinary = true
	if !eval(c, bin).OK() {
		t.Error("allow_binary")
	}
}

func TestBinaryPaths(t *testing.T) {
	c := cfg()
	c.BinaryPaths = []string{"Assets/Art/Kit/**"}
	c.BinaryMaxFileBytes = 50000
	model := add("Assets/Art/Kit/Barrel.fbx")
	model.Binary, model.NewSize = true, 40000
	if res := eval(c, model); !res.OK() {
		t.Errorf("a model in the folder for binary files: %+v", res.Denied)
	}
	model.NewSize = 50001
	if got := rules(eval(c, model).Denied); got != "D5" {
		t.Errorf("over the size for binary files: %q", got)
	}
	elsewhere := add("Assets/Scripts/blob.bin")
	elsewhere.Binary = true
	if got := rules(eval(c, elsewhere).Denied); got != "D6" {
		t.Errorf("a binary file outside the folder: %q", got)
	}
	text := add("Assets/Art/Kit/notes.txt")
	text.NewSize = 40000
	if got := rules(eval(c, text).Denied); got != "D5" {
		t.Errorf("a large text file in the folder for binary files keeps the normal limit: %q", got)
	}
}

func TestPathCollisionsAndBadPaths(t *testing.T) {
	c := cfg()
	base := []string{"README.md", "src/café.txt", "src/old.go"}
	for name, tc := range map[string]struct {
		ch   Change
		want string
	}{
		"case twin":         {add("readme.md"), "D8"},
		"unicode twin":      {add("src/café.txt"), "D8"},
		"same path":         {mod("README.md"), ""},
		"control character": {add("a\nb.txt"), "D8"},
		"bidi override":     {add("src/\u202egpj.exe"), "D8"},
		"zero width":        {add("src/ma\u200bin.go"), "D8"},
		"dot dot":           {add("a/../b"), "D8"},
		"absolute":          {add("/etc/passwd"), "D8"},
		"backslash":         {add("a\\b"), "D8"},
		"empty segment":     {add("a//b"), "D8"},
	} {
		r := Evaluate(c, Input{Changes: []Change{tc.ch}, BasePaths: base})
		if got := rules(r.Denied); got != tc.want {
			t.Errorf("%s: denied %q, want %q", name, got, tc.want)
		}
		for _, f := range r.Denied {
			if strings.ContainsAny(f.Path, "\n\u202e\u200b") {
				t.Errorf("%s: the finding shows the raw path %q", name, f.Path)
			}
		}
	}
	// Two new files that collide with each other.
	if got := rules(eval(c, add("Docs/a.md"), add("docs/A.md")).Denied); got != "D8" {
		t.Errorf("new twins: %q", got)
	}
	// A case-only rename is not a collision: the old name is gone.
	ren := Change{Path: "readme.md", OldPath: "README.md", Status: 'R', OldMode: ModeFile, NewMode: ModeFile}
	if r := Evaluate(c, Input{Changes: []Change{ren}, BasePaths: base}); !r.OK() {
		t.Errorf("case-only rename: %v", r.Denied)
	}
	// Replacing a deleted file under another case is fine too.
	del := Change{Path: "README.md", Status: 'D', OldMode: ModeFile}
	if r := Evaluate(c, Input{Changes: []Change{del, add("Readme.md")}, BasePaths: base}); !r.OK() {
		t.Errorf("delete and re-add: %v", r.Denied)
	}
}

// Test credentials are assembled at run time so that no scanner mistakes
// this file for a leak.
func fake(prefix string, n int) string { return prefix + strings.Repeat("a1B2", n/4+1)[:n] }

func TestSecretScan(t *testing.T) {
	held := "hs-" + strings.Repeat("q", 20)
	hits := map[string]string{
		"aws_access_key":     "key = " + strings.ToUpper(fake("AKIA", 16)),
		"github_token":       "token: " + fake("ghp_", 36),
		"github_token pat":   fake("github_pat_", 60),
		"gitlab_token":       fake("glpat-", 24),
		"private_key":        "-----BEGIN " + "OPENSSH PRIVATE KEY-----",
		"jwt":                fake("eyJ", 20) + "." + fake("eyJ", 20) + "." + fake("", 20),
		"slack_token":        fake("xoxb-", 24),
		"google_api_key":     fake("AIza", 35),
		"stripe_key":         fake("sk_"+"live_", 24),
		"anthropic_key":      fake("sk-"+"ant-", 30),
		"linear_key":         fake("lin_"+"api_", 32),
		"generic_assignment": `db_password = "k9$Lm2#pQ7vX4wZ8nB5c"`,
		"held_secret":        "curl -H 'x: " + held + "'",
	}
	for name, line := range hits {
		rule := strings.Fields(name)[0]
		r := Evaluate(cfg(), Input{Changes: []Change{mod("src/a.go", "ok", line)}, HeldSecrets: []string{held}})
		if rules(r.Denied) != "D7" {
			t.Errorf("%s: not denied: %v", name, r.Denied)
			continue
		}
		f := r.Denied[0]
		if !strings.Contains(f.Detail, rule) || !strings.Contains(f.Detail, "line 2") {
			t.Errorf("%s: detail %q", name, f.Detail)
		}
		// The finding goes to the agent and onto the pull request.
		for _, word := range strings.Fields(line) {
			if len(word) >= 16 && strings.Contains(f.String(), strings.Trim(word, `"'`)) {
				t.Errorf("%s: the finding repeats the value", name)
			}
		}
	}
	clean := []string{
		`password = "changeme-changeme-changeme"`,
		`api_key = "${API_KEY_FROM_ENVIRONMENT}"`,
		`token = "aaaaaaaaaaaaaaaaaaaaaaaa"`,
		`const tokenizer = newTokenizer(input)`,
		`secret := os.Getenv("APP_SECRET")`,
		`sha: 3f786850e387550fdab836ed7e6dc881de23001b`,
		"hs-qqq",
	}
	for _, line := range clean {
		if r := Evaluate(cfg(), Input{Changes: []Change{mod("src/a.go", line)}, HeldSecrets: []string{held}}); !r.OK() {
			t.Errorf("false positive on %q: %v", line, r.Denied)
		}
	}
}

func TestSecretScanCoversNamesMessagesAndCanBeDisabled(t *testing.T) {
	tok := fake("ghp_", 36)
	if got := rules(eval(cfg(), add("keys/"+tok+".txt")).Denied); got != "D7" {
		t.Errorf("file name: %q", got)
	}
	r := Evaluate(cfg(), Input{Commits: []Commit{{SHA: strings.Repeat("a", 40), Message: "use " + tok}}})
	if rules(r.Denied) != "D7" {
		t.Errorf("commit message: %v", r.Denied)
	}
	// Removed lines are not scanned: removing a leaked value must be possible.
	ch := mod("src/a.go")
	ch.Removed = []string{"token: " + tok}
	if !eval(cfg(), ch).OK() {
		t.Error("a removed credential line was denied")
	}
	// A short held value would match everywhere and is not searched for.
	if r := Evaluate(cfg(), Input{Changes: []Change{mod("a.go", "abc")}, HeldSecrets: []string{"abc", ""}}); !r.OK() {
		t.Errorf("short held secret: %v", r.Denied)
	}
	off := cfg()
	off.SecretScan = false
	if !eval(off, mod("src/a.go", "token: "+tok)).OK() {
		t.Error("secret_scan: false still scanned")
	}
}

func TestFlags(t *testing.T) {
	c := cfg()
	c.FlagPaths = []string{"migrations/**"}
	exec := Change{Path: "tools/gen", Status: 'M', OldMode: ModeFile, NewMode: ModeExec}
	shrunk := mod("tests/a_test.py", "x")
	shrunk.Removed = []string{"a", "b", "c"}
	skip := mod("src/app.spec.ts", "it.skip('works', () => {})")
	only := mod("tests/b.js", "describe.only('x', () => {})", "y")
	only.Removed = []string{"describe('x', () => {})", "y"}
	goSkip := mod("pkg/a_test.go", `t.Skip("flaky")`, "more")
	asserts := mod("pkg/b_test.go", "x := 1", "y := 2")
	asserts.Removed = []string{"assert.Equal(t, 1, x)", "y := 3"}
	for name, tc := range map[string]struct {
		ch   Change
		want string
	}{
		"test deleted":        {Change{Path: "tests/a_test.py", Status: 'D', OldMode: ModeFile}, "F1"},
		"test shrank":         {shrunk, "F1"},
		"skip marker":         {skip, "F1"},
		"only marker":         {only, "F1"},
		"go skip":             {goSkip, "F1"},
		"assertions removed":  {asserts, "F1"},
		"test grew":           {mod("tests/a_test.py", "assert x == 1", "assert y == 2"), ""},
		"eslint":              {mod(".eslintrc.json"), "F2"},
		"nested tsconfig":     {mod("web/tsconfig.build.json"), "F2"},
		"lockfile":            {mod("web/yarn.lock"), "F3"},
		"go.mod":              {mod("go.mod"), "F3"},
		"package.json":        {mod("package.json"), "F3,F4"},
		"shell script":        {mod("scripts/deploy.sh"), "F4"},
		"dockerfile":          {mod("svc/Dockerfile.prod"), "F4"},
		"xcode project":       {mod("ios/App.xcodeproj/project.pbxproj"), "F4"},
		"unity settings":      {mod("ProjectSettings/ProjectSettings.asset"), "F4"},
		"made executable":     {exec, "F4"},
		"instructions":        {mod("CLAUDE.md"), "F5"},
		"nested instructions": {mod("pkg/AGENTS.md"), "F5"},
		"flag_paths":          {mod("migrations/001.sql"), "F7"},
		"renamed from script": {Change{Path: "docs/old.txt", OldPath: "run.sh", Status: 'R', OldMode: ModeFile, NewMode: ModeFile}, "F4"},
	} {
		r := eval(c, tc.ch)
		if !r.OK() {
			t.Errorf("%s: denied %v", name, r.Denied)
		}
		if got := rules(r.Flags); got != tc.want {
			t.Errorf("%s: flags %q, want %q (%v)", name, got, tc.want, r.Flags)
		}
	}
}

// Code that reaches outside the program is pointed out, never refused, and
// the finding names what it does without repeating the line.
func TestReachingOutsideIsFlagged(t *testing.T) {
	blob := strings.Repeat("QUJD", 60)
	for name, tc := range map[string]struct {
		path string
		line string
		want string
	}{
		"miner pool":      {"src/Worker.cs", `var pool = "stratum+tcp://pool.example.net:3333";`, "open a network connection"},
		"http client c#":  {"src/Api.cs", `using var c = new HttpClient();`, "open a network connection"},
		"fetch":           {"web/api.ts", `const r = await fetch(url)`, "open a network connection"},
		"go dial":         {"pkg/a.go", `conn, err := net.Dial("tcp", addr)`, "open a network connection"},
		"python requests": {"tool.py", `requests.post(u, data=open(p).read())`, "open a network connection"},
		"unity request":   {"Assets/Scripts/Net.cs", `var req = UnityWebRequest.Get(u);`, "open a network connection"},
		"go exec":         {"pkg/a.go", `out, _ := exec.Command("sh", "-c", s).Output()`, "start another program"},
		"node spawn":      {"web/run.js", `const p = spawn("xmrig", args)`, "start another program"},
		"c# process":      {"src/Run.cs", `Process.Start("cmd.exe", args);`, "start another program"},
		"python system":   {"tool.py", `os.system(cmd)`, "start another program"},
		"c system":        {"src/a.c", `system(cmd);`, "start another program"},
		"eval":            {"web/x.js", `eval(atob(payload))`, "run code that is built at run time"},
		"python exec":     {"tool.py", `exec(code)`, "run code that is built at run time"},
		"assembly load":   {"src/L.cs", `var a = Assembly.Load(bytes);`, "run code that is built at run time"},
		"base64 blob":     {"src/data.go", `var payload = "` + blob + `"`, "hold a long encoded block"},
		"hex escapes":     {"src/sc.c", `char sc[] = "` + strings.Repeat(`\x90`, 48) + `";`, "hold a long encoded block"},
		"plain code":      {"src/app.go", `total := price * count`, ""},
		"regexp exec":     {"web/parse.js", `const m = re.exec(line)`, ""},
		"method system":   {"src/a.cs", `var s = this.system(1);`, ""},
		"short hash":      {"src/a.go", `const sum = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"`, ""},
		"readme link":     {"README.md", `See https://example.com/docs for more.`, ""},
		"lockfile hashes": {"web/package-lock.json", `"integrity": "sha512-` + blob + `"`, ""},
		"relative import": {"web/a.ts", `import { x } from "./http"`, ""},
	} {
		r := eval(cfg(), mod(tc.path, tc.line))
		if !r.OK() {
			t.Errorf("%s: denied %v", name, r.Denied)
		}
		var got string
		for _, f := range r.Flags {
			if f.Rule == "F8" {
				got = f.Detail
			}
		}
		switch {
		case tc.want == "" && got != "":
			t.Errorf("%s: flagged: %s", name, got)
		case tc.want != "" && !strings.Contains(got, "1 added line(s) that "+tc.want):
			t.Errorf("%s: %q lacks %q", name, got, tc.want)
		case strings.Contains(got, "pool.example") || strings.Contains(got, blob[:20]):
			t.Errorf("%s: the finding repeats the line: %s", name, got)
		}
	}
	// Removed lines do not count, and several kinds are named together.
	both := mod("src/x.py", "requests.get(u)", "subprocess.run(c)", "requests.get(v)")
	both.Removed = []string{"eval(x)"}
	r := eval(cfg(), both)
	if len(r.Flags) != 1 || r.Flags[0].Detail != "reaches outside the program: 2 added line(s) that open a network connection, 1 added line(s) that start another program" {
		t.Errorf("%v", r.Flags)
	}
}

func TestCommitIdentityFlag(t *testing.T) {
	sha := strings.Repeat("ab", 20)
	good := Commit{SHA: sha, AuthorName: "bot", AuthorEmail: "bot@example.com", CommitterName: "bot", CommitterEmail: "bot@example.com"}
	other := good
	other.AuthorEmail = "someone@example.com"
	odd := other
	odd.SHA = "not a sha\nINJECTED"
	r := Evaluate(cfg(), Input{Commits: []Commit{good, other, odd}, BotName: "bot", BotEmail: "bot@example.com"})
	if !r.OK() || rules(r.Flags) != "F6,F6" {
		t.Fatalf("denied %v flags %v", r.Denied, r.Flags)
	}
	if !strings.Contains(r.Flags[0].Detail, "abababa") || strings.Contains(r.Flags[1].Detail, "INJECTED") {
		t.Errorf("details: %q, %q", r.Flags[0].Detail, r.Flags[1].Detail)
	}
}

func TestFindingsAreBoundedAndOrdered(t *testing.T) {
	var changes []Change
	for i := 0; i < 300; i++ {
		changes = append(changes, add(".github/f"+strings.Repeat("x", i)))
	}
	c := cfg()
	c.MaxChangedFiles = 1000
	c.MaxDiffBytes = 1 << 30
	r := eval(c, changes...)
	if len(r.Denied) != maxFindings {
		t.Fatalf("%d findings", len(r.Denied))
	}
	long := eval(c, add(".github/"+strings.Repeat("y", 5000)))
	if n := len([]rune(long.Denied[0].Path)); n > 201 {
		t.Errorf("path of %d characters in a finding", n)
	}
	sym := Change{Path: ".github/link", Status: 'A', NewMode: ModeSymlink, Binary: true}
	if got := rules(eval(c, sym).Denied); got != "D1,D3,D6" {
		t.Errorf("order: %q", got)
	}
	if fb := eval(c, sym).Feedback(); len(fb) != 3 || !strings.HasPrefix(fb[0], "D1 .github/link: ") {
		t.Errorf("feedback: %q", fb)
	}
}
