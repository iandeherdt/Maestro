package systemdbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/proxy"
	"github.com/iandeherdt/Maestro/internal/sandbox"
)

func TestSelfTestAsksTheLauncherPerProfile(t *testing.T) {
	b, dir := fakeSudo(t, `head -c 300 `+"$(dirname $0)"+`/stdin >> `+"$(dirname $0)"+`/requests; echo >> `+"$(dirname $0)"+`/requests
echo '{"name":"runs as an ordinary user","ok":true}'`)
	rep, err := b.SelfTest(context.Background())
	if err != nil || len(rep.Checks) != 3 || len(rep.Failed()) != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	if rep.Checks[0].Profile != "none" || rep.Checks[1].Profile != "hooks" || rep.Checks[2].Profile != "agent" {
		t.Errorf("%+v", rep.Checks)
	}
	reqs, _ := os.ReadFile(filepath.Join(dir, "requests"))
	if strings.Count(string(reqs), `"step":"selftest"`) != 3 || strings.Contains(string(reqs), "argv") {
		t.Errorf("requests %s", reqs)
	}
}

func TestSelfTestOfAnOlderMaestro(t *testing.T) {
	b, _ := fakeSudo(t, `echo 'usage: maestro <command>' >&2; exit 2`)
	if rep, err := b.SelfTest(context.Background()); err != nil || !rep.OldImage {
		t.Errorf("%+v %v", rep, err)
	}
}

func TestLoggedIn(t *testing.T) {
	for body, want := range map[string]bool{
		`echo '{"loggedIn":true}'`:  true,
		`echo '{"loggedIn":false}'`: false,
		`exit 1`:                    false,
	} {
		b, _ := fakeSudo(t, body)
		if yes, err := b.LoggedIn(context.Background(), "claude"); err != nil || yes != want {
			t.Errorf("%s: %v %v", body, yes, err)
		}
	}
	b, _ := fakeSudo(t, `echo broken >&2; exit 7`)
	if _, err := b.LoggedIn(context.Background(), "claude"); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Errorf("%v", err)
	}
}

func TestLoginRunsTheLaunchersLoginOnTheTerminal(t *testing.T) {
	b, dir := fakeSudo(t, "")
	if err := b.Login(context.Background(), []string{"ignored"}, strings.NewReader(""), os.Stdout, os.Stderr); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if string(args) != "-n\n--\n/usr/local/libexec/maestro-launch\nlinux\nlogin\n" {
		t.Errorf("args %q", args)
	}
}

func TestProxiesOnSockets(t *testing.T) {
	sock := t.TempDir()
	allow := t.TempDir()
	proxy.WriteAllow(allow, "agent", []string{"api.anthropic.com"})
	b := &Box{Worker: "linux", SocketDir: filepath.Join(sock, "proxy")}
	if b.ProxyRunning(context.Background(), "agent") {
		t.Fatal("running before Up")
	}
	if err := b.Up(context.Background(), allow); err != nil {
		t.Fatal(err)
	}
	for _, p := range Profiles {
		if !b.ProxyRunning(context.Background(), p) {
			t.Errorf("%s does not answer", p)
		}
	}
	if fi, _ := os.Stat(b.SocketDir); fi.Mode().Perm() != 0o700 {
		t.Errorf("socket folder %v", fi.Mode())
	}
	// A second process, such as `maestro login` beside the service, leaves
	// them to the first.
	other := &Box{Worker: "linux", SocketDir: b.SocketDir}
	if err := other.Up(context.Background(), allow); err != nil {
		t.Fatal(err)
	}
	other.Down(context.Background())
	if !b.ProxyRunning(context.Background(), "agent") {
		t.Error("the second process stopped the first one's proxy")
	}
	b.Down(context.Background())
	if b.ProxyRunning(context.Background(), "agent") {
		t.Error("still running after Down")
	}
}

var _ = sandbox.NetAgent
