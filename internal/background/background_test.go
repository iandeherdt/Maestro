package background

import (
	"context"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func unit(t *testing.T, goos string) (*Unit, *[]string) {
	home := t.TempDir()
	var calls []string
	u := &Unit{GOOS: goos, Worker: "mac", Binary: "/Users/ian/.local/bin/maestro",
		SetupDir:     "/Users/ian/Library/Application Support/maestro",
		WorkflowFile: "/Users/ian/Library/Application Support/maestro/WORKFLOW.md",
		LogFile:      filepath.Join(home, "state", "service.log"), Home: home, UID: 501}
	u.Run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if strings.Contains(strings.Join(args, " "), "bootout") {
			return []byte("not loaded"), errors.New("exit status 3")
		}
		return nil, nil
	}
	return u, &calls
}

func TestLaunchdAgent(t *testing.T) {
	u, calls := unit(t, "darwin")
	u.WorkflowFile = `/Users/ian/a & b/<x>/WORKFLOW.md`
	text, err := u.Content()
	if err != nil {
		t.Fatal(err)
	}
	// The file is well-formed, whatever the paths contain.
	d := xml.NewDecoder(strings.NewReader(text))
	d.Strict = true
	var words []string
	for {
		tok, err := d.Token()
		if err != nil {
			if err.Error() != "EOF" {
				t.Fatalf("not well-formed: %v\n%s", err, text)
			}
			break
		}
		if c, ok := tok.(xml.CharData); ok && strings.TrimSpace(string(c)) != "" {
			words = append(words, string(c))
		}
	}
	got := strings.Join(words, "|")
	for _, want := range []string{"Label|com.maestro.mac",
		"/Users/ian/.local/bin/maestro|run|--dir|/Users/ian/Library/Application Support/maestro|--file|/Users/ian/a & b/<x>/WORKFLOW.md",
		"RunAtLoad", "KeepAlive", "StandardErrorPath|" + u.LogFile, "/Applications/Docker.app/Contents/Resources/bin"} {
		if !strings.Contains(got, want) {
			t.Errorf("the agent lacks %q:\n%s", want, got)
		}
	}
	if err := u.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !u.Installed() || u.Path() != filepath.Join(u.Home, "Library/LaunchAgents/com.maestro.mac.plist") {
		t.Fatalf("path %s", u.Path())
	}
	want := "launchctl bootout gui/501/com.maestro.mac,launchctl bootstrap gui/501 " + u.Path()
	if strings.Join(*calls, ",") != want {
		t.Errorf("commands = %v", *calls)
	}
	if err := u.Uninstall(context.Background()); err != nil || u.Installed() {
		t.Fatalf("uninstall: %v", err)
	}
	if err := u.Uninstall(context.Background()); err != nil {
		t.Fatalf("uninstalling twice: %v", err)
	}
}

func TestSystemdUserUnit(t *testing.T) {
	u, calls := unit(t, "linux")
	u.WorkflowFile = `/home/ian/my "files"/50%/$HOME/WORKFLOW.md`
	text, err := u.Content()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`ExecStart="/Users/ian/.local/bin/maestro" "run" "--dir" "/Users/ian/Library/Application Support/maestro" "--file" "/home/ian/my \"files\"/50%%/$$HOME/WORKFLOW.md"` + "\n",
		"Restart=always\n", "WantedBy=default.target\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("the unit lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Docker.app") {
		t.Error("a macOS path in the Linux unit")
	}
	if err := u.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*calls, ","); got != "systemctl --user daemon-reload,systemctl --user enable maestro-mac.service,systemctl --user restart maestro-mac.service" {
		t.Errorf("commands = %s", got)
	}
	if u.Path() != filepath.Join(u.Home, ".config/systemd/user/maestro-mac.service") {
		t.Errorf("path %s", u.Path())
	}
}

func TestInstallReportsAFailingCommandAndRefusesBadInput(t *testing.T) {
	u, _ := unit(t, "darwin")
	u.Run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if args[0] == "bootstrap" {
			return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
		}
		return nil, nil
	}
	if err := u.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "Input/output error") {
		t.Fatalf("%v", err)
	}
	for name, change := range map[string]func(*Unit){
		"worker":        func(u *Unit) { u.Worker = "a b" },
		"relative path": func(u *Unit) { u.Binary = "maestro" },
		"newline":       func(u *Unit) { u.WorkflowFile = "/a\nExecStartPre=/bin/x" },
		"windows":       func(u *Unit) { u.GOOS = "windows" },
	} {
		u, _ := unit(t, "linux")
		change(u)
		if _, err := u.Content(); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if err := u.Install(context.Background()); err == nil {
			t.Errorf("%s: installed", name)
		}
		if _, err := os.Stat(u.Path()); err == nil && name != "windows" {
			t.Errorf("%s: a file was written", name)
		}
	}
}

// A service installed when the command was called symphony is replaced by
// install and removed by uninstall, so two copies never run side by side.
func TestServiceUnderTheOldNameIsReplaced(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		u, calls := unit(t, goos)
		old := u.pathOf(u.oldLabel())
		write := func() {
			if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(old, []byte("old"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write()
		if !u.Installed() {
			t.Fatalf("%s: the old service is not seen as installed", goos)
		}
		if err := u.Install(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(old); err == nil {
			t.Errorf("%s: install left the old service file", goos)
		}
		if _, err := os.Stat(u.Path()); err != nil {
			t.Errorf("%s: install did not write the new file: %v", goos, err)
		}
		if !strings.Contains(strings.Join(*calls, ","), u.oldLabel()) {
			t.Errorf("%s: the old service was not stopped: %v", goos, *calls)
		}
		write()
		if err := u.Uninstall(context.Background()); err != nil || u.Installed() {
			t.Fatalf("%s: uninstall: %v", goos, err)
		}
	}
}
