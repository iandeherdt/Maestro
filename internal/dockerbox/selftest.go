package dockerbox

// The self-test of SPEC 11.6. The service and `maestro doctor` start a
// throwaway container exactly as a step is started, with a scratch
// workspace, and let the image's own maestro binary try what a step must
// not be able to do (`maestro probe`). A check that is required fails the
// self-test when the forbidden thing works; the service does not start
// then. An advisory check is reported and does not stop anything.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/sandbox"
)

// ProbeCheck is the answer to one check.
type ProbeCheck struct {
	// Profile is the network profile the probe ran with: none, agent or
	// hooks.
	Profile string `json:"profile"`
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	// Advisory checks are reported and never stop the service.
	Advisory bool   `json:"advisory,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// SelfTestReport is what SelfTest found.
type SelfTestReport struct {
	Checks []ProbeCheck
	// OldImage is set when the image's maestro has no probe: it was built
	// by an older Maestro. No check ran then.
	OldImage bool
}

// Failed lists the required checks that did not pass.
func (r SelfTestReport) Failed() []ProbeCheck {
	var out []ProbeCheck
	for _, c := range r.Checks {
		if !c.OK && !c.Advisory {
			out = append(out, c)
		}
	}
	return out
}

// Warnings lists the advisory checks that did not pass.
func (r SelfTestReport) Warnings() []ProbeCheck {
	var out []ProbeCheck
	for _, c := range r.Checks {
		if !c.OK && c.Advisory {
			out = append(out, c)
		}
	}
	return out
}

// canaryName is set in the environment of the container command during
// the self-test. The probe must not see it.
const canaryName = "MAESTRO_SELFTEST_CANARY"

// probeDenyHost is a host no allowlist has a reason to name.
const probeDenyHost = "example.org"

// SelfTest runs the probe in a container for each network profile, the
// way a step is run. The proxies must be up (Up). It removes what it
// created.
func (b *Box) SelfTest(ctx context.Context) (SelfTestReport, error) {
	if err := b.check(); err != nil {
		return SelfTestReport{}, err
	}
	key := "selftest-" + token()
	vol := b.Volume(key)
	if _, err := b.cli(ctx, "volume", "create", "--label", "maestro.worker="+b.Worker, vol); err != nil {
		return SelfTestReport{}, fail("%v", err)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), killTimeout)
		defer cancel()
		b.cli(c, "volume", "rm", "--force", vol)
	}()
	var rep SelfTestReport
	for _, profile := range []string{sandbox.NetNone, sandbox.NetHooks, sandbox.NetAgent} {
		checks, old, err := b.probe(ctx, key, profile)
		if err != nil {
			return rep, err
		}
		if old {
			return SelfTestReport{OldImage: true}, nil
		}
		rep.Checks = append(rep.Checks, checks...)
	}
	return rep, nil
}

func (b *Box) probe(ctx context.Context, key, profile string) ([]ProbeCheck, bool, error) {
	network := profile
	if profile == sandbox.NetNone {
		network = ""
	}
	argv := []string{"maestro", "probe", "--profile", profile, "--canary", canaryName, "--deny", probeDenyHost}
	if b.PIDs > 0 {
		argv = append(argv, "--pids", strconv.FormatInt(b.PIDs, 10))
	}
	if n, ok := memoryBytes(b.Memory); ok {
		argv = append(argv, "--memory", strconv.FormatInt(n, 10))
	}
	s := spec{step: sandbox.Step("selftest"), key: key, argv: argv, workdir: workDir,
		network: network, env: b.baseEnv(network)}
	name := b.prefix() + "-" + key + "-" + profile
	c, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.Command(b.docker(), b.runArgs(name, s)...)
	// The container command sees a variable the probe must not.
	cmd.Env = append(os.Environ(), canaryName+"="+token())
	var out, errb capped
	out.max, errb.max = maxStdout, maxStderr
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		return nil, false, fail("docker could not be started: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-c.Done():
		b.remove(name)
		<-done
		return nil, false, fail("the self-test container did not finish in time")
	}
	checks, perr := parseProbe(out.buf.Bytes(), profile)
	if perr == nil && len(checks) > 0 {
		return checks, false, nil
	}
	text := out.buf.String() + errb.buf.String()
	if strings.Contains(text, "usage:") {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fail("the self-test container failed: %s", lastLine(errb.buf.Bytes()))
	}
	return nil, false, fail("the self-test container gave no answer")
}

func parseProbe(out []byte, profile string) ([]ProbeCheck, error) {
	var checks []ProbeCheck
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var c ProbeCheck
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, err
		}
		// The probe runs in the sandbox: it says what it found, not which
		// profile it ran with.
		c.Profile = profile
		checks = append(checks, c)
	}
	return checks, sc.Err()
}

// memoryBytes turns docker's "6g" into bytes.
func memoryBytes(s string) (int64, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, false
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'k':
		mult = 1 << 10
	case 'm':
		mult = 1 << 20
	case 'g':
		mult = 1 << 30
	case 'b':
	}
	if mult != 1 || s[len(s)-1] == 'b' {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n * mult, true
}

// Probe is `maestro probe`, run by SelfTest inside the sandbox. It writes
// one JSON line per check. Its exit status is 0 whatever it finds; the
// service judges.
func Probe(args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	profile := fs.String("profile", "none", "")
	canary := fs.String("canary", "", "")
	deny := fs.String("deny", probeDenyHost, "")
	pids := fs.Int64("pids", 0, "")
	memory := fs.Int64("memory", 0, "")
	if fs.Parse(args) != nil {
		return 2
	}
	p := prober{root: "/", lookup: net.DefaultResolver.LookupHost, dial: dialTimeout}
	for _, c := range p.run(*profile, *canary, *deny, *pids, *memory) {
		raw, _ := json.Marshal(c)
		fmt.Fprintln(stdout, string(raw))
	}
	return 0
}

func dialTimeout(network, addr string) (net.Conn, error) {
	return net.DialTimeout(network, addr, 3*time.Second)
}

// prober holds what the checks reach for, so that tests can point it at
// a fake machine.
type prober struct {
	root   string
	env    []string
	lookup func(ctx context.Context, host string) ([]string, error)
	dial   func(network, addr string) (net.Conn, error)
	uid    int
	setUID bool
}

func (p prober) path(s string) string { return filepath.Join(p.root, s) }

func (p prober) environ() []string {
	if p.env != nil {
		return p.env
	}
	return os.Environ()
}

func (p prober) getuid() int {
	if p.setUID {
		return p.uid
	}
	return os.Getuid()
}

func (p prober) run(profile, canary, deny string, pids, memory int64) []ProbeCheck {
	var out []ProbeCheck
	add := func(name string, ok, advisory bool, detail string) {
		out = append(out, ProbeCheck{Name: name, OK: ok, Advisory: advisory, Detail: detail})
	}

	uid := p.getuid()
	add("runs as an ordinary user", uid != 0, false, "user id "+strconv.Itoa(uid))

	status, _ := os.ReadFile(p.path("proc/self/status"))
	capEff, capBnd, nnp := field(status, "CapEff"), field(status, "CapBnd"), field(status, "NoNewPrivs")
	add("has no capabilities", zeroHex(capEff) && zeroHex(capBnd), false, "effective "+capEff+", bounding "+capBnd)
	add("cannot gain privileges, also not through a setuid program", nnp == "1", false, "NoNewPrivs "+nnp)

	var leaked []string
	for _, kv := range p.environ() {
		name, _, _ := strings.Cut(kv, "=")
		if (canary != "" && name == canary) || credRE.MatchString(strings.ToUpper(name)) {
			leaked = append(leaked, name)
		}
	}
	add("sees nothing of the service's environment", len(leaked) == 0, false, strings.Join(leaked, ", "))

	var wrote []string
	for _, d := range []string{"", "etc", "usr/local/bin", "usr/bin"} {
		f := p.path(filepath.Join(d, ".maestro-selftest"))
		if err := os.WriteFile(f, []byte("x"), 0o600); err == nil {
			os.Remove(f)
			wrote = append(wrote, "/"+d)
		}
	}
	add("cannot write outside the workspace", len(wrote) == 0, false, strings.Join(wrote, ", "))

	f := p.path("workspace/.maestro-selftest")
	err := os.WriteFile(f, []byte("x"), 0o600)
	os.Remove(f)
	add("can write in its workspace", err == nil, false, errText(err))

	// The image has empty folders where the mirror, the hand-off folder,
	// the login and the licence go. What counts is whether something is
	// mounted there.
	var present []string
	mounted := mountPoints(p.path("proc/self/mountinfo"))
	for _, d := range []string{mirrorDir, handoffDir, claudeDir, licenceDir} {
		if mounted[d] {
			present = append(present, d)
		}
	}
	for _, d := range []string{"var/run/docker.sock", "run/docker.sock", "run/podman/podman.sock"} {
		if _, err := os.Lstat(p.path(d)); err == nil {
			present = append(present, "/"+d)
		}
	}
	add("has no folder of the machine and no container socket", len(present) == 0, false, strings.Join(present, ", "))

	var reached []string
	for _, addr := range []string{"1.1.1.1:443", "8.8.8.8:53", "169.254.169.254:80"} {
		if c, err := p.dial("tcp", addr); err == nil {
			c.Close()
			reached = append(reached, addr)
		}
	}
	if gw := gateway(p.path("proc/net/route")); gw != "" {
		for _, port := range []string{"22", "80", "443"} {
			addr := net.JoinHostPort(gw, port)
			if c, err := p.dial("tcp", addr); err == nil {
				c.Close()
				reached = append(reached, addr+" (the gateway)")
			}
		}
	}
	add("cannot connect to anything directly", len(reached) == 0, false, strings.Join(reached, ", "))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	addrs, lerr := p.lookup(ctx, deny)
	cancel()
	resolved := lerr == nil && len(addrs) > 0
	// With no network at all a name must not resolve. On a network with a
	// proxy the container engine may answer names itself; that is a way
	// to send a little data out, so it is reported.
	add("cannot look up names on the internet", !resolved, profile != sandbox.NetNone,
		map[bool]string{true: deny + " resolved", false: ""}[resolved])

	if profile != sandbox.NetNone {
		proxy := p.proxyURL()
		code, err := connectThrough(proxy, deny+":443")
		add("the proxy refuses a host that is not on the list", err == nil && code != http.StatusOK, false,
			proxyDetail(code, err))
		if profile == sandbox.NetAgent {
			code, err := connectThrough(proxy, "api.anthropic.com:443")
			add("the proxy lets the agent reach Claude", err == nil && code == http.StatusOK, true, proxyDetail(code, err))
		}
	}

	if pids > 0 {
		have := limit(p.path("sys/fs/cgroup/pids.max"), p.path("sys/fs/cgroup/pids/pids.max"))
		add("the process limit is in force", have > 0 && have <= pids, true, limitDetail(have, pids))
	}
	if memory > 0 {
		have := limit(p.path("sys/fs/cgroup/memory.max"), p.path("sys/fs/cgroup/memory/memory.limit_in_bytes"))
		add("the memory limit is in force", have > 0 && have <= memory, true, limitDetail(have, memory))
	}
	return out
}

// mountPoints reads the mount points from /proc/self/mountinfo.
func mountPoints(path string) map[string]bool {
	out := map[string]bool{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if f := strings.Fields(line); len(f) > 4 {
			out[f[4]] = true
		}
	}
	return out
}

func (p prober) proxyURL() string {
	for _, kv := range p.environ() {
		if v, ok := strings.CutPrefix(kv, "HTTPS_PROXY="); ok {
			return v
		}
	}
	return ""
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// field reads "Name:\tvalue" from /proc/self/status.
func field(status []byte, name string) string {
	for _, line := range strings.Split(string(status), "\n") {
		if v, ok := strings.CutPrefix(line, name+":"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func zeroHex(s string) bool {
	if s == "" {
		return false
	}
	return strings.Trim(s, "0") == ""
}

// gateway is the default route's gateway from /proc/net/route, or "".
func gateway(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		v, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil || v == 0 {
			continue
		}
		// Little-endian, as the kernel writes it.
		return net.IPv4(byte(v), byte(v>>8), byte(v>>16), byte(v>>24)).String()
	}
	return ""
}

// connectThrough asks the proxy for a tunnel and returns its status code.
func connectThrough(proxy, target string) (int, error) {
	u, err := url.Parse(proxy)
	if err != nil || u.Host == "" {
		return 0, errors.New("no proxy is set")
	}
	c, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	res, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
	if err != nil {
		return 0, err
	}
	res.Body.Close()
	return res.StatusCode, nil
}

func proxyDetail(code int, err error) string {
	if err != nil {
		return err.Error()
	}
	return "the proxy answered " + strconv.Itoa(code)
}

// limit reads the first cgroup limit file that exists. "max" and missing
// are 0, no limit.
func limit(paths ...string) int64 {
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil {
			return 0
		}
		// cgroups v1 writes a huge number for "no limit".
		if n >= 1<<62 {
			return 0
		}
		return n
	}
	return 0
}

func limitDetail(have, want int64) string {
	if have == 0 {
		return "no limit is set; the settings ask for " + strconv.FormatInt(want, 10)
	}
	return strconv.FormatInt(have, 10) + ", the settings ask for " + strconv.FormatInt(want, 10)
}
