package dockerbox

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMachine lays out the files the probe reads.
func fakeMachine(t *testing.T, status, route string, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	put := func(rel, body string) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("proc/self/status", status)
	put("proc/net/route", route)
	for k, v := range files {
		put(k, v)
	}
	os.MkdirAll(filepath.Join(root, "workspace"), 0o755)
	return root
}

// fakeProxy answers CONNECT: 200 for the hosts in allow, 403 otherwise.
func fakeProxy(t *testing.T, allow ...string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil {
					return
				}
				host, _, _ := net.SplitHostPort(req.Host)
				for _, a := range allow {
					if a == host {
						c.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
						return
					}
				}
				c.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n"))
			}()
		}
	}()
	return "http://" + ln.Addr().String()
}

const goodStatus = "Name:\tmaestro\nCapEff:\t0000000000000000\nCapBnd:\t0000000000000000\nNoNewPrivs:\t1\n"

func byName(checks []ProbeCheck) map[string]ProbeCheck {
	m := map[string]ProbeCheck{}
	for _, c := range checks {
		m[c.Name] = c
	}
	return m
}

func noDial(string, string) (net.Conn, error) { return nil, errors.New("network is unreachable") }

func noLookup(context.Context, string) ([]string, error) { return nil, errors.New("no such host") }

func TestProbeOnAGoodSandbox(t *testing.T) {
	// The probe writes into "/", "/etc" and so on of the fake root; they
	// are missing here, so writing fails as it must.
	root := fakeMachine(t, goodStatus, "Iface\tDestination\tGateway\n", map[string]string{
		"sys/fs/cgroup/pids.max": "256\n", "sys/fs/cgroup/memory.max": "536870912\n"})
	os.Chmod(root, 0o555)
	t.Cleanup(func() { os.Chmod(root, 0o755) })
	proxy := fakeProxy(t, "api.anthropic.com")
	p := prober{root: root, uid: 10001, setUID: true, lookup: noLookup, dial: noDial,
		env: []string{"HOME=/home/agent", "HTTPS_PROXY=" + proxy}}
	checks := p.run("agent", canaryName, "example.com", 256, 512<<20)
	for _, c := range checks {
		// root writes into a read-only folder all the same.
		rootWrites := os.Getuid() == 0 && c.Name == "cannot write outside the workspace"
		if !c.OK && !rootWrites {
			t.Errorf("%s: %s", c.Name, c.Detail)
		}
	}
	m := byName(checks)
	for _, name := range []string{"the proxy refuses a host that is not on the list", "the proxy lets the agent reach Claude",
		"the process limit is in force", "the memory limit is in force"} {
		if _, ok := m[name]; !ok {
			t.Errorf("no check %q", name)
		}
	}
}

func TestProbeFindsEveryHole(t *testing.T) {
	root := fakeMachine(t, "CapEff:\t00000000a80425fb\nCapBnd:\t00000000a80425fb\nNoNewPrivs:\t0\n",
		"Iface\tDestination\tGateway\neth0\t00000000\t0100A8C0\n",
		map[string]string{"proc/self/mountinfo": "36 35 98:0 /srv/mirror /mirror ro,relatime - ext4 /dev/sda1 rw\n",
			"var/run/docker.sock": "", "sys/fs/cgroup/pids.max": "max\n",
			"sys/fs/cgroup/memory/memory.limit_in_bytes": "9223372036854771712\n"})
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	os.MkdirAll(filepath.Join(root, "usr/local/bin"), 0o755)
	os.MkdirAll(filepath.Join(root, "usr/bin"), 0o755)
	var dialed []string
	dial := func(_, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		a, b := net.Pipe()
		b.Close()
		return a, nil
	}
	lookup := func(context.Context, string) ([]string, error) { return []string{"93.184.215.14"}, nil }
	proxy := fakeProxy(t, "example.com")
	p := prober{root: root, uid: 0, setUID: true, lookup: lookup, dial: dial,
		env: []string{canaryName + "=x", "GITHUB_TOKEN=y", "HTTPS_PROXY=" + proxy}}
	m := byName(p.run("none", canaryName, "example.com", 256, 512<<20))
	for name, advisory := range map[string]bool{
		"runs as an ordinary user": false, "has no capabilities": false,
		"cannot gain privileges, also not through a setuid program": false,
		"sees nothing of the service's environment":                 false,
		"cannot write outside the workspace":                        false,
		"has no folder of the machine and no container socket":      false,
		"cannot connect to anything directly":                       false,
		"cannot look up names on the internet":                      false,
		"the process limit is in force":                             true,
		"the memory limit is in force":                              true,
	} {
		c, ok := m[name]
		switch {
		case !ok:
			t.Errorf("no check %q", name)
		case c.OK:
			t.Errorf("%q passed on a machine where it must fail", name)
		case c.Advisory != advisory:
			t.Errorf("%q advisory = %v", name, c.Advisory)
		}
	}
	if d := m["sees nothing of the service's environment"].Detail; !strings.Contains(d, canaryName) || !strings.Contains(d, "GITHUB_TOKEN") || strings.Contains(d, "=") {
		t.Errorf("environment detail = %q", d)
	}
	if d := m["has no folder of the machine and no container socket"].Detail; d != "/mirror, /var/run/docker.sock" {
		t.Errorf("mount detail = %q", d)
	}
	if d := m["cannot connect to anything directly"].Detail; !strings.Contains(d, "192.168.0.1:22 (the gateway)") {
		t.Errorf("gateway not tried: %q (dialed %v)", d, dialed)
	}
	if _, ok := m["the proxy refuses a host that is not on the list"]; ok {
		t.Error("a step without network has no proxy to check")
	}

	// With a proxy, a name lookup is advisory and the proxy is checked.
	m = byName(p.run("hooks", "", "example.com", 0, 0))
	if c := m["cannot look up names on the internet"]; c.OK || !c.Advisory {
		t.Errorf("lookup on hooks: %+v", c)
	}
	if c := m["the proxy refuses a host that is not on the list"]; c.OK || c.Advisory {
		t.Errorf("an open proxy: %+v", c)
	}
	if _, ok := m["the proxy lets the agent reach Claude"]; ok {
		t.Error("only the agent's proxy needs to reach Claude")
	}
	if _, ok := m["the process limit is in force"]; ok {
		t.Error("no limit asked, none checked")
	}
}

func TestParseProbeAndReport(t *testing.T) {
	out := []byte("noise\n{\"profile\":\"agent\",\"name\":\"a\",\"ok\":true}\n{\"name\":\"b\",\"ok\":false}\n{\"name\":\"c\",\"ok\":false,\"advisory\":true}\n")
	checks, err := parseProbe(out, "none")
	if err != nil || len(checks) != 3 {
		t.Fatalf("%v %v", checks, err)
	}
	for _, c := range checks {
		if c.Profile != "none" {
			t.Errorf("profile taken from the sandbox: %+v", c)
		}
	}
	rep := SelfTestReport{Checks: checks}
	if f := rep.Failed(); len(f) != 1 || f[0].Name != "b" {
		t.Errorf("failed = %v", f)
	}
	if w := rep.Warnings(); len(w) != 1 || w[0].Name != "c" {
		t.Errorf("warnings = %v", w)
	}
	if _, err := parseProbe([]byte("{bad\n"), "none"); err == nil {
		t.Error("a broken line is an error")
	}
}

func TestMemoryBytes(t *testing.T) {
	for in, want := range map[string]int64{"6g": 6 << 30, "512m": 512 << 20, "64k": 64 << 10, "1000": 1000, "100b": 100} {
		if got, ok := memoryBytes(in); !ok || got != want {
			t.Errorf("%s = %d, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "g", "-1g", "six"} {
		if _, ok := memoryBytes(in); ok {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestSelfTestInRealContainers(t *testing.T) {
	if !sharedOK {
		t.Skip("no container engine")
	}
	rep, err := shared.SelfTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OldImage {
		t.Fatal("the test image has the probe")
	}
	if f := rep.Failed(); len(f) > 0 {
		t.Fatalf("required checks failed: %+v", f)
	}
	profiles := map[string]int{}
	for _, c := range rep.Checks {
		profiles[c.Profile]++
	}
	if profiles["none"] == 0 || profiles["hooks"] == 0 || profiles["agent"] == 0 {
		t.Errorf("profiles = %v", profiles)
	}
	t.Logf("warnings: %+v", rep.Warnings())
	if out, _ := shared.cli(ctx, "volume", "ls", "--quiet", "--filter", "name=selftest"); strings.Contains(out, shared.Worker) {
		t.Errorf("a self-test volume was left: %s", out)
	}
}
