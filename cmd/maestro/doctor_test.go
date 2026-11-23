package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/dockerbox"
)

func TestCgroupAdvice(t *testing.T) {
	for _, c := range []struct {
		rootless             bool
		version, controllers string
		want                 string
	}{
		{false, "v1", "", ""},
		{true, "v1", "", "systemd.unified_cgroup_hierarchy=1"},
		{true, "v2", "cpuset cpu io memory pids\n", ""},
		{true, "v2", "memory pids\n", "controllers cpu,"},
		{true, "v2", "", ""},
	} {
		got := cgroupAdvice(c.rootless, c.version, c.controllers)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%v %s %q: %q", c.rootless, c.version, c.controllers, got)
		}
	}
	if got := cgroupAdvice(true, "v2", "io"); !strings.Contains(got, "memory, pids, cpu") {
		t.Errorf("missing list: %q", got)
	}
}

func TestDoctorPrintsTheSelfTest(t *testing.T) {
	var out bytes.Buffer
	d := &doctor{out: &out}
	d.selfTest(dockerbox.SelfTestReport{Checks: []dockerbox.ProbeCheck{
		{Profile: "none", Name: "runs as an ordinary user", OK: true},
		{Profile: "agent", Name: "cannot connect to anything directly", Detail: "1.1.1.1:443"},
		{Profile: "hooks", Name: "the memory limit is in force", Advisory: true, Detail: "no limit is set"},
	}}, "")
	for _, want := range []string{
		"ok    a step without network: runs as an ordinary user",
		"FAIL  the agent: cannot connect to anything directly", "1.1.1.1:443", "does not start",
		"WARN  a hook or a check: the memory limit is in force", "runaway step",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("lacks %q:\n%s", want, out.String())
		}
	}
	if d.fails != 1 || d.warns != 1 {
		t.Errorf("fails %d warns %d", d.fails, d.warns)
	}
	out.Reset()
	d = &doctor{out: &out}
	d.selfTest(dockerbox.SelfTestReport{OldImage: true}, "Build it again with: maestro image")
	if d.fails != 1 || !strings.Contains(out.String(), "maestro image") {
		t.Errorf("old image:\n%s", out.String())
	}
}

func TestDoctorWithoutSettingsSaysSo(t *testing.T) {
	var out, errb bytes.Buffer
	code := runDoctor([]string{"--dir", t.TempDir()}, &out, &errb)
	if code != 1 || !strings.Contains(out.String(), "FAIL  the settings and the setup can be read") ||
		!strings.Contains(out.String(), "1 problem(s) stop the service") {
		t.Errorf("exit %d:\n%s%s", code, out.String(), errb.String())
	}
}

func TestGitPackagesOfAUnityManifest(t *testing.T) {
	got, err := gitPackages([]byte(`{"dependencies": {
		"com.unity.inputsystem": "1.11.2",
		"com.coplaydev.unity-mcp": "https://github.com/CoplayDev/unity-mcp.git?path=/MCPForUnity#v8.0.0",
		"com.example.ssh": "git@gitlab.com:team/pkg.git",
		"com.example.auth": "git+https://user@git.example.org:8443/pkg.git",
		"com.example.local": "file:../pkg"
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"com.coplaydev.unity-mcp": "github.com", "com.example.ssh": "gitlab.com", "com.example.auth": "git.example.org"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
	if !proxyAllows([]string{"*.github.com", "GitHub.com"}, "github.com") || proxyAllows([]string{"*.github.com"}, "github.com") ||
		!proxyAllows([]string{"*.example.org"}, "git.example.org") {
		t.Error("proxyAllows does not follow the proxy's rule")
	}
}
