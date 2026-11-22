package workflow

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePath(t *testing.T) {
	if got := ResolvePath("/etc/maestro/WORKFLOW.md"); got != "/etc/maestro/WORKFLOW.md" {
		t.Errorf("explicit path not used: %q", got)
	}
	if got := ResolvePath(""); got != "WORKFLOW.md" {
		t.Errorf("default = %q, want WORKFLOW.md", got)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.md"))
	if ClassOf(err) != ClassMissingFile {
		t.Fatalf("class = %q, want %q (err %v)", ClassOf(err), ClassMissingFile, err)
	}
}

func TestLoadSetsPath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "WORKFLOW.md")
	if err := os.WriteFile(p, []byte("---\na: 1\n---\nhello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	def, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if def.Path != p || def.Config["a"] != 1 || def.PromptTemplate != "hello" {
		t.Errorf("unexpected definition: %+v", def)
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantClass  string
		wantPrompt string
		wantKeys   int
	}{
		{name: "front matter and body", in: "---\ntracker:\n  kind: github\n---\n\n  Do the work.  \n",
			wantPrompt: "Do the work.", wantKeys: 1},
		{name: "no front matter", in: "Just a prompt\nwith two lines\n", wantPrompt: "Just a prompt\nwith two lines"},
		{name: "empty front matter", in: "---\n---\nbody", wantPrompt: "body"},
		{name: "comment-only front matter", in: "---\n# nothing\n---\nbody", wantPrompt: "body"},
		{name: "empty body", in: "---\na: 1\n---\n", wantKeys: 1},
		{name: "crlf line endings", in: "---\r\na: 1\r\n---\r\nbody\r\n", wantPrompt: "body", wantKeys: 1},
		{name: "dashes later are body", in: "intro\n---\nnot: yaml\n---\n", wantPrompt: "intro\n---\nnot: yaml\n---"},
		{name: "unclosed front matter", in: "---\na: 1\nbody", wantClass: ClassParseError},
		{name: "invalid yaml", in: "---\na: [1, 2\n---\nbody", wantClass: ClassParseError},
		{name: "list front matter", in: "---\n- a\n- b\n---\nbody", wantClass: ClassFrontMatterNotMap},
		{name: "scalar front matter", in: "---\njust text\n---\nbody", wantClass: ClassFrontMatterNotMap},
		{name: "anchor rejected", in: "---\na: &x 1\n---\n", wantClass: ClassParseError},
		{name: "alias rejected", in: "---\na: &x {k: v}\nb: *x\n---\n", wantClass: ClassParseError},
		{name: "merge key rejected", in: "---\nbase: {k: v}\nb:\n  <<: {k: v}\n---\n", wantClass: ClassParseError},
		{name: "custom tag rejected", in: "---\na: !!python/object:os.system x\n---\n", wantClass: ClassParseError},
		{name: "local tag rejected", in: "---\na: !secret x\n---\n", wantClass: ClassParseError},
		{name: "binary tag rejected", in: "---\na: !!binary aGk=\n---\n", wantClass: ClassParseError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			def, err := Parse([]byte(tc.in))
			if got := ClassOf(err); got != tc.wantClass {
				t.Fatalf("class = %q, want %q (err %v)", got, tc.wantClass, err)
			}
			if err != nil {
				return
			}
			if def.PromptTemplate != tc.wantPrompt {
				t.Errorf("prompt = %q, want %q", def.PromptTemplate, tc.wantPrompt)
			}
			if len(def.Config) != tc.wantKeys {
				t.Errorf("config has %d keys, want %d: %v", len(def.Config), tc.wantKeys, def.Config)
			}
		})
	}
}
