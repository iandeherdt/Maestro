package pins

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, root, rel, text string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func find(pins []Pin, kind, name string) *Pin {
	for i := range pins {
		if pins[i].Kind == kind && pins[i].Name == name {
			return &pins[i]
		}
	}
	return nil
}

func TestScanFindsPinsByShape(t *testing.T) {
	root := t.TempDir()
	write(t, root, "internal/box/setup.go", "package box\n\n// pin: npm @anthropic-ai/claude-code\nconst V = \"2.1.330\"\n\nconst head = `FROM docker.io/library/node:22-bookworm-slim\nRUN npm install -g \"@anthropic-ai/claude-code@${V}\"\n`\n")
	write(t, root, "packs/unity/pack.json", `{"image": ["COPY --from=docker.io/unityci/editor:ubuntu-6000.3.29f1-base-4.0.0 /opt/unity /opt/unity", "RUN npm install --global pnpm@10"],
"image_note": "the Unity Editor 6000.3.29f1"}`)
	write(t, root, "packs/unity/ci.yml", "jobs:\n  a:\n    container: unityci/editor:ubuntu-6000.3.29f1-base-4.0.0\n")
	write(t, root, "packs/klad/pack.json", `{"image": ["RUN set -eu; \\\n git clone --quiet --depth 1 --branch v0.4.0 https://github.com/klad-app/klad /tmp/klad; \\\n cd /tmp/klad; \\\n test \"$(git rev-parse HEAD)\" = 971a89b671ebbed0f806e2969f5da1a4c6735abb; \\\n make"]}`)
	write(t, root, "packs/klad/ci.yml", "env:\n  KLAD_VERSION: v0.4.0\n")
	write(t, root, "packs/blender/pack.json", `{"image": ["RUN file=\"blender-5.0.1-linux-$arch.tar.xz\""]}`)
	write(t, root, "packs/godot/pack.json", `{"image": ["RUN curl -o g.zip https://github.com/godotengine/godot/releases/download/4.5.1-stable/Godot_v4.5.1-stable_linux.x86_64.zip"]}`)
	write(t, root, "README.md", "Blender 5.0.1, and 15.0.12 is something else.\n")
	write(t, root, "internal/box/setup_test.go", "const x = `FROM alpine:3.20`\n")
	write(t, root, "docs/SPEC.md", "FROM debian:12\n")

	pins, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if find(pins, KindImage, "docker.io/library/alpine") != nil || find(pins, KindImage, "docker.io/library/debian") != nil {
		t.Error("tests and docs are not scanned for pins")
	}
	if p := find(pins, KindNPM, "@anthropic-ai/claude-code"); p == nil || p.Version != "2.1.330" {
		t.Errorf("marked constant: %+v", p)
	}
	if p := find(pins, KindNPM, "pnpm"); p == nil || p.Version != "10" {
		t.Errorf("npm literal: %+v", p)
	}
	if p := find(pins, KindImage, "docker.io/library/node"); p == nil || p.Version != "22-bookworm-slim" {
		t.Errorf("FROM: %+v", p)
	}
	unity := find(pins, KindImage, "docker.io/unityci/editor")
	if unity == nil || len(unity.Places) != 3 {
		t.Fatalf("unity in the pack, the workflow and the note: %+v", unity)
	}
	klad := find(pins, KindGit, "https://github.com/klad-app/klad")
	if klad == nil || klad.Version != "v0.4.0" || klad.Commit != "971a89b671ebbed0f806e2969f5da1a4c6735abb" {
		t.Fatalf("git clone: %+v", klad)
	}
	if !hasPlace(*klad, "packs/klad/ci.yml", 2) {
		t.Errorf("the workflow's KLAD_VERSION is listed: %+v", klad.Places)
	}
	blender := find(pins, KindBlender, "blender")
	if blender == nil || !hasPlace(*blender, "README.md", 1) || len(blender.Places) != 2 {
		t.Errorf("blender and its mention: %+v", blender)
	}
	if p := find(pins, KindGit, "https://github.com/godotengine/godot"); p == nil || p.Version != "4.5.1-stable" {
		t.Errorf("release download: %+v", p)
	}
}

func TestScanRepository(t *testing.T) {
	pins, err := Scan("../..")
	if err != nil {
		t.Fatal(err)
	}
	// The pins the sandbox image is built from must stay visible to the
	// scheduled check; a refactor that hides one fails here.
	if find(pins, KindNPM, "@anthropic-ai/claude-code") == nil {
		t.Error("ClaudeVersion lost its pin marker")
	}
	if find(pins, KindImage, "docker.io/library/node") == nil {
		t.Error("the Node base image is not found")
	}
}

func TestNewest(t *testing.T) {
	cases := []struct {
		cur  string
		tags []string
		want string
	}{
		{"22-bookworm-slim", []string{"22.11-bookworm-slim", "24-bookworm-slim", "24-trixie-slim", "20-bookworm-slim"}, "24-bookworm-slim"},
		{"10.0", []string{"10.0", "11.0-preview", "9.0", "11.0"}, "11.0"},
		{"ubuntu-6000.3.29f1-base-4.0.0", []string{"ubuntu-6000.3.30f1-base-4.0.0", "ubuntu-6000.5.0a3-base-4.0.0", "windows-6000.9.1f1-base-4.0.0"}, "ubuntu-6000.3.30f1-base-4.0.0"},
		{"v0.4.0", []string{"v0.3.0", "v0.4.0"}, ""},
		{"4.5.1-stable", []string{"4.6-stable", "4.5.2-stable", "4.6.0-rc1"}, "4.5.2-stable"},
	}
	for _, c := range cases {
		if got := newest(c.cur, c.tags, nil); got != c.want {
			t.Errorf("newest(%s) = %q, want %q", c.cur, got, c.want)
		}
	}
	if got := truncate("9.39.1", "9"); got != "9" {
		t.Errorf("truncate = %q", got)
	}
}
