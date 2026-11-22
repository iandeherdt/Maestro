package dockerbox

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/config"
)

func TestEveryToolTheSettingsAcceptIsDefined(t *testing.T) {
	var names []string
	for n, tool := range Tools {
		names = append(names, n)
		if tool.Note == "" || len(tool.Lines) == 0 {
			t.Errorf("%s: no note or no lines", n)
		}
		for _, l := range tool.Lines {
			if strings.HasPrefix(l, "USER") || strings.HasPrefix(l, "FROM") {
				t.Errorf("%s: a tool may not change the user or the base image: %q", n, l)
			}
		}
	}
	sort.Strings(names)
	want := slices.Clone(config.SandboxTools)
	sort.Strings(want)
	if !slices.Equal(names, want) {
		t.Fatalf("dockerbox.Tools %v, config.SandboxTools %v", names, want)
	}
}

func TestBlenderIsCheckedAgainstItsChecksum(t *testing.T) {
	text := strings.Join(Tools["blender"].Lines, "\n")
	for _, must := range []string{"sha256sum -c", "download.blender.org", "blender-" + BlenderVersion + "-linux-", "BLENDER=/opt/blender/blender"} {
		if !strings.Contains(text, must) {
			t.Errorf("missing %q", must)
		}
	}
}
