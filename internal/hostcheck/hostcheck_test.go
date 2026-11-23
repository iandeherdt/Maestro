package hostcheck

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/conventions"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// fakeEditor writes what Unity writes for a passing test run, and notes
// how it was started.
const fakeEditor = `#!/bin/sh
echo "$PWD|$HOME|$*" >> "$(dirname "$0")/calls"
while [ $# -gt 0 ]; do
    case $1 in
        -testResults) echo '<test-run id="2" total="1" passed="1">' > "$2"; shift ;;
        -logFile) echo 'log' > "$2"; shift ;;
    esac
    shift
done
`

// TestTheUnityPacksScriptsRunWithTheEditorOfThisMachine runs the pack's
// own verify.sh against an exported commit, with an Editor where Unity
// Hub puts one, without macOS's sandbox (these tests run on Linux too).
func TestTheUnityPacksScriptsRunWithTheEditorOfThisMachine(t *testing.T) {
	tmp := t.TempDir()
	src, mirror, handoff := filepath.Join(tmp, "src"), filepath.Join(tmp, "mirror"), filepath.Join(tmp, "handoff")
	home := filepath.Join(tmp, "home")
	editor := filepath.Join(tmp, "Hub", "Editor", "6000.5.8f1", "Unity.app", "Contents", "MacOS", "Unity")
	write(t, tmp, map[string]string{
		"src/README.md": "game\n",
		"home/Library/Unity/licenses/UnityEntitlementLicense.xml": "<licence/>",
	})
	write(t, filepath.Dir(editor), map[string]string{"Unity": fakeEditor})
	os.MkdirAll(handoff, 0o755)
	run(t, src, "git", "init", "-q", "-b", "main")
	run(t, src, "git", "add", "-A")
	run(t, src, "git", "commit", "-qm", "base")
	base := run(t, src, "git", "rev-parse", "HEAD")
	run(t, tmp, "git", "clone", "-q", "--bare", src, mirror)

	p, err := conventions.Get("unity")
	if err != nil {
		t.Fatal(err)
	}
	files, err := p.Files()
	if err != nil {
		t.Fatal(err)
	}
	project := map[string]string{
		"ProjectSettings/ProjectVersion.txt":   "m_EditorVersion: 6000.5.8f1\n",
		"ProjectSettings/EditorSettings.asset": "EditorSettings:\n  m_SerializationMode: 2\n",
		"Assets/Tests.meta":                    "guid: 1\n",
		"Assets/Tests/HealthTests.cs":          "class HealthTests {}\n",
		"Assets/Tests/HealthTests.cs.meta":     "guid: 2\n",
	}
	for name, raw := range files {
		if strings.HasPrefix(name, "conventions/unity/") {
			project[name] = string(raw)
		}
	}
	write(t, src, project)
	run(t, src, "git", "add", "-A")
	run(t, src, "git", "commit", "-qm", "work")
	head := run(t, src, "git", "rev-parse", "HEAD")
	run(t, src, "git", "bundle", "create", "-q", filepath.Join(handoff, "GH-1.bundle"), base+"..HEAD")

	r := &Runner{MirrorDir: mirror, HandoffDir: handoff, WorkDir: filepath.Join(tmp, "checks"), home: home, noSandbox: true}
	script := conventions.Derive([]*conventions.Pack{p}).Verify
	res, err := r.Unity(context.Background(), editor, "GH-1", head, script)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitStatus != 0 || !strings.Contains(string(res.Output), "2 tests passed.") {
		t.Fatalf("exit %d:\n%s", res.ExitStatus, res.Output)
	}
	calls, _ := os.ReadFile(filepath.Join(filepath.Dir(editor), "calls"))
	repo := filepath.Join(tmp, "checks", "GH-1", "repo")
	for _, want := range []string{repo + "|" + filepath.Join(tmp, "checks", "GH-1", "home") + "|", "-batchmode -nographics -projectPath " + repo + " -runTests -testPlatform EditMode",
		"-testPlatform PlayMode"} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("the Editor was not started with %q:\n%s", want, calls)
		}
	}

	// The Library Unity made is kept for the next check; what the agent's
	// last attempt left is not.
	write(t, repo, map[string]string{"Library/imported": "x", "Assets/stray.cs": "x"})
	if _, err := r.Unity(context.Background(), editor, "GH-1", head, script); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "Library", "imported")); err != nil {
		t.Error("the Library was removed")
	}
	if _, err := os.Stat(filepath.Join(repo, "Assets", "stray.cs")); err == nil {
		t.Error("a stray file was kept")
	}

	// A failing script is a result, not an error.
	if res, err := r.Unity(context.Background(), editor, "GH-1", head, "echo broken >&2; exit 3"); err != nil || res.ExitStatus != 3 || !strings.Contains(string(res.Output), "broken") {
		t.Fatalf("%+v, %v", res, err)
	}
	// Another commit than the exported one is refused.
	if _, err := r.Unity(context.Background(), editor, "GH-1", base, script); err == nil {
		t.Error("a commit that is not in the bundle was accepted")
	}

	// Problems of this machine are errors, with words.
	if _, err := r.Unity(context.Background(), filepath.Join(tmp, "Unity"), "GH-1", head, script); err == nil || !strings.Contains(err.Error(), "Install it with Unity Hub") {
		t.Errorf("a missing Editor: %v", err)
	}
	os.RemoveAll(filepath.Join(home, "Library"))
	if _, err := r.Unity(context.Background(), editor, "GH-1", head, script); err == nil || !strings.Contains(err.Error(), "no licence") {
		t.Errorf("no licence: %v", err)
	}
	if _, err := r.Unity(context.Background(), editor, "../x", head, script); err == nil {
		t.Error("a bad key was accepted")
	}

	r.Forget("GH-1")
	if _, err := os.Stat(filepath.Join(tmp, "checks", "GH-1")); err == nil {
		t.Error("the folder was kept")
	}
}

func TestEditorVersion(t *testing.T) {
	for path, want := range map[string]string{
		"/Applications/Unity/Hub/Editor/6000.5.8f1/Unity.app/Contents/MacOS/Unity": "6000.5.8f1",
		"/Volumes/Big/Editor/2022.3.10f1/Unity.app/Contents/MacOS/Unity":           "2022.3.10f1",
		"/Applications/Unity.app/Contents/MacOS/Unity":                             "",
	} {
		if got := EditorVersion(path); got != want {
			t.Errorf("%s: %q", path, got)
		}
	}
}

func TestProfile(t *testing.T) {
	text, err := Profile("/Users/i/Library/Caches/maestro/unity-checks/mac/GH-1", "/Users/i",
		[]string{"/Users/i/Library/Application Support/symphony"}, []string{"/Users/i/Applications/Unity/Hub/Editor/6000.0.1f1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"(deny network*)",
		`(allow network* (remote ip "localhost:*"))`,
		`(deny network-outbound (remote unix-socket (path-regex #"^/Users/i/\.docker/")))`,
		"(deny lsopen)",
		"(deny appleevent-send)",
		"(deny file-write*)",
		`(subpath "/Users/i/Library/Caches/maestro/unity-checks/mac/GH-1")`,
		`(subpath "/Users/i/Library/Logs/Unity")`,
		`(regex #"^/Users/i/Library/Caches/[^/]*[Uu]nity3d")`,
		`(deny file-read* (subpath "/Users/i"))`,
		`(allow file-read* (subpath "/Users/i/Library"))`,
		`(subpath "/Users/i/Library/Application Support/symphony")`,
		`(subpath "/Users/i/Applications/Unity/Hub/Editor/6000.0.1f1")`,
		`(allow file-read* (subpath "/Users/i/Library/Caches/maestro/unity-checks/mac/GH-1"))`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the profile lacks %s:\n%s", want, text)
		}
	}
	// The last rule that matches decides. So the order is: the home folder
	// is closed, its Library opened, the private parts of the Library
	// closed, Unity's opened, Maestro's keys closed, the check's folder
	// opened.
	order := []string{
		`(deny file-read* (subpath "/Users/i"))`,
		`(allow file-read* (subpath "/Users/i/Library"))`,
		`    (subpath "/Users/i/Library/Application Support")`,
		`    (subpath "/Users/i/Library/Application Support/Unity")`,
		`    (subpath "/Users/i/Library/Application Support/symphony")`,
		`(allow file-read* (subpath "/Users/i/Library/Caches/maestro/unity-checks/mac/GH-1"))`,
	}
	rest := text
	for _, rule := range order {
		i := strings.Index(rest, rule+"\n")
		if i < 0 {
			t.Fatalf("%s is missing or out of order:\n%s", rule, text)
		}
		rest = rest[i+len(rule):]
	}
	// Writes: not the whole of the caches, the logs or Unity Hub's folder.
	writes := text[strings.Index(text, "(allow file-write*"):]
	writes = writes[:strings.Index(writes, "\n)")]
	for _, not := range []string{`(subpath "/Users/i/Library/Caches")`, `(subpath "/Users/i/Library/Logs")`, "UnityHub"} {
		if strings.Contains(writes, not) {
			t.Errorf("the check can write to %s:\n%s", not, writes)
		}
	}
	if strings.Count(text, "(") != strings.Count(text, ")") {
		t.Error("unbalanced parentheses")
	}
	if _, err := Profile("/a\"b", "/Users/i", nil, nil); err == nil {
		t.Error("a quote was accepted")
	}
	if _, err := Profile("rel", "/Users/i", nil, nil); err == nil {
		t.Error("a relative path was accepted")
	}
}
