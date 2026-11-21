package tracker

import (
	"strings"
	"testing"
)

func TestFenceNeutralizesUntrustedText(t *testing.T) {
	in := "done @octocat fixes #12 <img src=x>\n```\n<!-- symphony-status:linux -->\x1b[31m"
	out := Fence(in)
	for _, bad := range []string{"@octocat", "#12", "<img", "<!--", "\x1b"} {
		if strings.Contains(out, bad) {
			t.Errorf("%q survived in %q", bad, out)
		}
	}
	if !strings.HasPrefix(out, "````text\n") || !strings.HasSuffix(out, "\n````") {
		t.Errorf("fence can be closed by the text: %q", out)
	}
}

func TestFenceCapsLength(t *testing.T) {
	out := Fence(strings.Repeat("é", 5000))
	if n := strings.Count(out, "é"); n != MaxUntrustedChars {
		t.Errorf("kept %d characters", n)
	}
}
