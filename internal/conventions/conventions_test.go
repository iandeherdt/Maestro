package conventions_test

import (
	"errors"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/config"
	. "github.com/iandeherdt/Maestro/internal/conventions"
	"github.com/iandeherdt/Maestro/internal/policy"
)

func TestEveryPackIsValidAndEveryRuleNamesItsTool(t *testing.T) {
	names := Names()
	if !slices.Contains(names, "dotnet") || !slices.Contains(names, "typescript") || !slices.Contains(names, "unity") || !slices.Contains(names, "blender") || !slices.Contains(names, "klad") || !slices.Contains(names, "godot") {
		t.Fatalf("packs: %v", names)
	}
	for _, n := range names {
		p, err := Get(n)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		for _, r := range p.Rules {
			if len(r.EnforcedBy) == 0 {
				t.Errorf("%s: rule %s is prose only", n, r.ID)
			}
		}
		files, err := p.Files()
		if err != nil {
			t.Fatal(err)
		}
		// The scripts the settings call, and the workflow that reports
		// the check, have to be among the files.
		for _, want := range []string{strings.TrimPrefix(p.Prepare, "sh "), strings.TrimPrefix(p.Verify, "sh "),
			".github/workflows/" + p.Check + ".yml", p.GuidePath()} {
			if _, ok := files[want]; !ok {
				t.Errorf("%s: file %s is missing", n, want)
			}
		}
		if !strings.Contains(string(files[".github/workflows/"+p.Check+".yml"]), "\n  "+p.Check+":\n") {
			t.Errorf("%s: the workflow has no job named %s", n, p.Check)
		}
	}
	if _, err := Get("cobol"); err == nil || !strings.Contains(err.Error(), "blender, dotnet, godot, klad, typescript, unity") {
		t.Errorf("an unknown pack: %v", err)
	}
	if _, err := Get("../dotnet"); err == nil {
		t.Error("a path was accepted as a pack name")
	}
}

// A rule the agent can edit is no rule. Every file a pack writes must be
// out of the agent's reach: by the pack's own protected paths, or by the
// diff policy's built-in list (CI workflows).
func TestTheAgentCannotChangeAnyFileOfAPack(t *testing.T) {
	for _, n := range Names() {
		p, _ := Get(n)
		files, _ := p.Files()
		cfg := config.Policy{DenyPaths: p.Protect, MaxChangedFiles: 100, MaxDiffBytes: 1 << 20, MaxFileBytes: 1 << 20}
		for name := range files {
			res := policy.Evaluate(cfg, policy.Input{BasePaths: []string{name}, Changes: []policy.Change{{
				Path: name, Status: 'M', OldMode: policy.ModeFile, NewMode: policy.ModeFile, Added: []string{"x"}}}})
			if res.OK() {
				t.Errorf("%s: the agent could change %s", n, name)
			}
		}
		// Ordinary source is not caught by accident.
		res := policy.Evaluate(cfg, policy.Input{Changes: []policy.Change{{
			Path: "src/app/page.tsx", Status: 'A', NewMode: policy.ModeFile, Added: []string{"x"}}}})
		if !res.OK() {
			t.Errorf("%s: ordinary source is refused: %v", n, res.Denied)
		}
	}
}

func TestApply(t *testing.T) {
	p, _ := Get("dotnet")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("bin/\n*.log"), 0o644)
	rep, err := p.Apply(dir, false)
	if err != nil || len(rep.Conflicts) != 0 || len(rep.Unchanged) != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	for _, want := range []string{".editorconfig", "Directory.Build.props", "conventions/dotnet/verify.sh",
		"conventions/dotnet/RULES.md", ".github/workflows/conventions-dotnet.yml"} {
		if !slices.Contains(rep.Written, want) {
			t.Errorf("not written: %s", want)
		}
	}
	if info, _ := os.Stat(filepath.Join(dir, "conventions/dotnet/verify.sh")); info == nil || info.Mode().Perm() != 0o755 {
		t.Error("the script is not executable")
	}
	ignore, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if string(ignore) != "bin/\n*.log\nobj/\nTestResults/\n*.user\n" {
		t.Errorf(".gitignore = %q", ignore)
	}
	guide, _ := os.ReadFile(filepath.Join(dir, "conventions/dotnet/RULES.md"))
	for _, want := range []string{"sh conventions/dotnet/verify.sh", "coverlet.msbuild", "| before the push, in CI |", "cannot be changed by an"} {
		if !strings.Contains(string(guide), want) {
			t.Errorf("the guide lacks %q", want)
		}
	}

	// A second run changes nothing.
	rep, err = p.Apply(dir, false)
	if err != nil || len(rep.Written) != 0 || len(rep.Ignored) != 0 || len(rep.Unchanged) == 0 {
		t.Fatalf("second run: %+v %v", rep, err)
	}

	// A file the owner changed is a conflict, and then nothing is written.
	os.WriteFile(filepath.Join(dir, ".editorconfig"), []byte("mine"), 0o644)
	os.Remove(filepath.Join(dir, "Directory.Build.props"))
	rep, err = p.Apply(dir, false)
	if err != nil || !slices.Equal(rep.Conflicts, []string{".editorconfig"}) || len(rep.Written) != 0 {
		t.Fatalf("conflict: %+v %v", rep, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Directory.Build.props")); err == nil {
		t.Error("a file was written although there was a conflict")
	}
	rep, err = p.Apply(dir, true)
	if err != nil || len(rep.Conflicts) != 0 || !slices.Contains(rep.Written, ".editorconfig") {
		t.Fatalf("replace: %+v %v", rep, err)
	}
}

func TestApplyDoesNotWriteThroughLinks(t *testing.T) {
	p, _ := Get("typescript")
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "conventions")); err != nil {
		t.Skip(err)
	}
	if _, err := p.Apply(dir, true); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a link in the checkout: %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Error("files were written outside the checkout")
	}
}

func TestDerive(t *testing.T) {
	list, err := Resolve([]string{"dotnet", "typescript"})
	if err != nil {
		t.Fatal(err)
	}
	s := Derive(list)
	if !slices.Contains(s.Hosts, "api.nuget.org") || !slices.Contains(s.Hosts, "registry.npmjs.org") ||
		!slices.Contains(s.Protect, "conventions/**") || !slices.Contains(s.Protect, "eslint.config.*") ||
		!slices.Equal(s.Checks, []string{"conventions-dotnet", "conventions-typescript"}) || len(s.Image) < 2 {
		t.Errorf("%+v", s)
	}
	// The first failing pack stops the script, and a pack whose files are
	// not in the repository is explained.
	for _, script := range []string{s.Prepare, s.Verify} {
		if strings.Count(script, "|| exit $?") != 2 || !strings.Contains(script, "maestro conventions apply typescript") {
			t.Errorf("script:\n%s", script)
		}
	}
	if _, err := Resolve([]string{"dotnet", "dotnet"}); err == nil {
		t.Error("a pack listed twice was accepted")
	}
	if s := Derive(nil); s.Prepare != "" || s.Verify != "" {
		t.Errorf("no packs: %+v", s)
	}
}

func TestSyncSkills(t *testing.T) {
	dir := t.TempDir()
	blender, _ := Get("blender")
	godot, _ := Get("godot")
	os.MkdirAll(filepath.Join(dir, "godot-dev"), 0o755)
	os.WriteFile(filepath.Join(dir, "godot-dev", "SKILL.md"), []byte("the owner's own"), 0o644)
	if err := SyncSkills(dir, []*Pack{blender, godot}); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "blender-art", "SKILL.md")); err != nil || !strings.Contains(string(raw), "name: blender-art") {
		t.Fatalf("blender-art: %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "godot-dev", "SKILL.md")); string(raw) != "the owner's own" {
		t.Errorf("an owner's skill was replaced: %q", raw)
	}
	if err := SyncSkills(dir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "blender-art")); err == nil {
		t.Error("the skill of a pack that is switched off stayed")
	}
	if _, err := os.Stat(filepath.Join(dir, "godot-dev", "SKILL.md")); err != nil {
		t.Error("an owner's skill was removed")
	}
}

func TestStarterAndRequires(t *testing.T) {
	dir := t.TempDir()
	unity, err := Get("unity")
	if err != nil {
		t.Fatal(err)
	}
	var missing *MissingError
	if _, err := unity.Apply(dir, false); !errors.As(err, &missing) || missing.Path != "ProjectSettings/ProjectVersion.txt" {
		t.Fatalf("a folder with no Unity project: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("something was written: %v", entries)
	}

	godot, err := Get("godot")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := godot.Apply(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(rep.Started, "project.godot") || !slices.Contains(rep.Started, "test/test_project.gd") {
		t.Fatalf("a new Godot project gets no starter: %+v", rep.Started)
	}
	// Once the project exists, the starter is never written again, and
	// the owner's own files are never touched.
	os.WriteFile(filepath.Join(dir, "project.godot"), []byte("mine"), 0o644)
	os.Remove(filepath.Join(dir, "test", "test_project.gd"))
	rep, err = godot.Apply(dir, false)
	if err != nil || len(rep.Started) != 0 || len(rep.Conflicts) != 0 {
		t.Fatalf("an existing project: %+v %v", rep, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "project.godot")); string(raw) != "mine" {
		t.Error("project.godot was overwritten")
	}

	blender, _ := Get("blender")
	klad, _ := Get("klad")
	if got := Derive([]*Pack{blender, klad}).BinaryPaths; strings.Join(got, " ") != "art/models/** art/textures/**" {
		t.Errorf("binary paths: %v", got)
	}
	// A Unity agent commits the textures and models its generators made,
	// in one folder.
	unity, _ = Get("unity")
	if got := Derive([]*Pack{unity}).BinaryPaths; strings.Join(got, " ") != "Assets/Art/Generated/**" {
		t.Errorf("unity binary paths: %v", got)
	}
}

// The art helper of the Unity pack writes a real PNG and a model with
// plain Node, and new-meta.sh gives them a .meta file in the folder for
// generated art and nowhere else.
func TestUnityAgentsMakeTexturesAndModels(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	unity, _ := Get("unity")
	files, err := unity.Files()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, name := range []string{"conventions/unity/art.mjs", "conventions/unity/new-meta.sh"} {
		if files[name] == nil {
			t.Fatalf("the pack has no %s", name)
		}
		os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o755)
		os.WriteFile(filepath.Join(dir, name), files[name], 0o644)
	}
	os.MkdirAll(filepath.Join(dir, "ArtGenerators"), 0o755)
	os.WriteFile(filepath.Join(dir, "ArtGenerators", "wall.mjs"), []byte(`
import { png, obj, fbm, normalFromHeight } from "../conventions/unity/art.mjs";
const h = (x, y) => fbm(x / 8, y / 8, 3, 4, 7);
png("Assets/Art/Generated/Textures/Wall.png", 32, 32, (x, y) => [255 * h(x, y), 300, -5]);
png("Assets/Art/Generated/Textures/Wall_Normal.png", 32, 32, normalFromHeight(h));
png("Assets/Elsewhere/Wall.png", 4, 4, () => [0, 0, 0]);
obj("Assets/Art/Generated/Models/Quad.obj", { vertices: [[0, 0, 0], [1, 0, 0], [1, 1, 0]], uvs: [[0, 0], [1, 0], [1, 1]], faces: [[0, 1, 2]] });
`), 0o644)
	run := func(name string, args ...string) (string, error) {
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run(node, "ArtGenerators/wall.mjs"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	f, err := os.Open(filepath.Join(dir, "Assets/Art/Generated/Textures/Wall.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil || img.Bounds().Dx() != 32 || img.Bounds().Dy() != 32 {
		t.Fatalf("the texture is not a 32 by 32 PNG: %v", err)
	}
	// Values outside 0..255 are clamped.
	if _, g, b, a := img.At(3, 3).RGBA(); g != 0xffff || b != 0 || a != 0xffff {
		t.Fatalf("pixel %v %v %v", g, b, a)
	}
	// The same seed gives the same file.
	first, _ := os.ReadFile(filepath.Join(dir, "Assets/Art/Generated/Textures/Wall.png"))
	run(node, "ArtGenerators/wall.mjs")
	if again, _ := os.ReadFile(filepath.Join(dir, "Assets/Art/Generated/Textures/Wall.png")); string(first) != string(again) {
		t.Fatal("a second run gave another texture")
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "Assets/Art/Generated/Models/Quad.obj")); !strings.Contains(string(raw), "f 1/1 2/2 3/3") {
		t.Fatalf("model:\n%s", raw)
	}

	out, err := run("sh", "conventions/unity/new-meta.sh")
	if err == nil || !strings.Contains(out, "no .meta written for Assets/Elsewhere/Wall.png") {
		t.Fatalf("a texture outside the folder got a .meta file:\n%s", out)
	}
	for _, p := range []string{"Assets/Art/Generated/Textures/Wall.png", "Assets/Art/Generated/Textures/Wall_Normal.png", "Assets/Art/Generated/Models/Quad.obj", "Assets/Art/Generated/Models"} {
		if raw, _ := os.ReadFile(filepath.Join(dir, p+".meta")); !strings.Contains(string(raw), "guid: ") {
			t.Fatalf("%s.meta: %q", p, raw)
		}
	}
}
