package claude

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/config"
)

const (
	idA = "11111111-2222-4333-8444-555555555555"
	idB = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
)

func TestInteractiveArgsKeepTheAgentsRestrictions(t *testing.T) {
	c := config.Claude{Command: "claude", PermissionMode: "bypassPermissions", DisallowedTools: []string{"WebFetch", "Bash(curl:*)"}, Model: "opus"}
	argv, err := InteractiveArgs(c, idA, true)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(argv, " ")
	for _, want := range []string{"--setting-sources user", "--strict-mcp-config", `--mcp-config {"mcpServers":{}}`,
		"--disallowedTools WebFetch,Bash(curl:*)", "--model opus", "--permission-mode bypassPermissions", "--resume " + idA} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
	if slices.Contains(argv, "-p") || slices.Contains(argv, "--output-format") {
		t.Errorf("print mode in an interactive session: %q", got)
	}
	argv, _ = InteractiveArgs(c, idA, false)
	if argv[len(argv)-2] != "--session-id" {
		t.Errorf("a new session: %q", argv)
	}
	for _, bad := range []string{"", "--help", "x; rm -rf /", idA + " "} {
		if _, err := InteractiveArgs(c, bad, true); err == nil {
			t.Errorf("session id %q was accepted", bad)
		}
	}
}

func TestLastSessionReadsWholeLinesOnly(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"` + idA + `"}`,
		`{"type":"assistant","message":{"content":"session_id \"` + idB + `\""}}`,
		`not json "session_id":"` + idB + `"`,
		`{"type":"result","session_id":"not-a-uuid"}`,
		``,
	}, "\n")
	if got := LastSession(strings.NewReader(stream)); got != idA {
		t.Errorf("LastSession = %q", got)
	}
	if got := LastSession(strings.NewReader("")); got != "" {
		t.Errorf("empty stream gave %q", got)
	}
}

func TestConversations(t *testing.T) {
	dir := t.TempDir()
	c := Conversations{Dir: filepath.Join(dir, "talk")}
	audit := filepath.Join(dir, "GH-3.jsonl")
	os.WriteFile(audit, []byte(`{"type":"system","session_id":"`+idA+`"}`+"\n"), 0o600)
	if got := c.Last("GH-3", audit); got != idA {
		t.Errorf("without a conversation Last = %q, want the agent's session", got)
	}
	if err := c.Save("GH-3", idB); err != nil {
		t.Fatal(err)
	}
	if got := c.Pending("GH-3"); got != idB {
		t.Errorf("Pending = %q", got)
	}
	if got := c.Last("GH-3", audit); got != idB {
		t.Errorf("with a conversation Last = %q", got)
	}
	if fi, err := os.Stat(filepath.Join(dir, "talk", "GH-3.talk")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the file: %v %v", fi, err)
	}
	c.Done("GH-3")
	if got := c.Pending("GH-3"); got != "" {
		t.Errorf("after Done Pending = %q", got)
	}
	for _, key := range []string{"", "../x", "a/b"} {
		if c.Save(key, idA) == nil {
			t.Errorf("key %q was accepted", key)
		}
	}
	if c.Save("GH-4", "nope") == nil {
		t.Error("a session id that is not a UUID was saved")
	}
}
