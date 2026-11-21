package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

type rig struct {
	t      *testing.T
	p      *Proxy
	addr   string
	echo   net.Listener
	mu     sync.Mutex
	dialed []netip.AddrPort
	names  map[string][]string
	logs   strings.Builder
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// newRig starts a proxy whose public destinations all lead to a local
// echo server. The address check still runs on what the names resolve to.
func newRig(t *testing.T, allow ...string) *rig {
	r := &rig{t: t, names: map[string][]string{
		"api.anthropic.com": {"160.79.104.10"},
		"evil.example":      {"93.184.216.34"},
		"lan.example":       {"192.168.1.10"},
		"mixed.example":     {"10.0.0.1", "93.184.216.34"},
		"meta.example":      {"169.254.169.254"},
		"mapped.example":    {"::ffff:127.0.0.1"},
		"down.example":      {"93.184.216.35"},
		"sub.allowed.dev":   {"93.184.216.34"},
		"allowed.dev":       {"93.184.216.34"},
	}}
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.echo = echo
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	r.p = &Proxy{Profile: "agent", Allow: func() []string { return allow },
		Log: slog.New(slog.NewTextHandler(lockedWriter{&r.mu, &r.logs}, nil)),
		Lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
			var out []netip.Addr
			for _, s := range r.names[host] {
				out = append(out, netip.MustParseAddr(s))
			}
			if out == nil {
				return nil, errors.New("no such host")
			}
			return out, nil
		},
		Dial: func(ctx context.Context, to netip.AddrPort) (net.Conn, error) {
			r.mu.Lock()
			r.dialed = append(r.dialed, to)
			r.mu.Unlock()
			if to.Addr().String() == "93.184.216.35" {
				return nil, errors.New("refused")
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", echo.Addr().String())
		}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.addr = ln.Addr().String()
	go r.p.Serve(ln)
	t.Cleanup(func() { ln.Close(); echo.Close(); r.p.CloseConnections() })
	return r
}

// send writes raw request text and returns the status line and the open
// connection.
func (r *rig) send(raw string) (int, net.Conn, *bufio.Reader) {
	r.t.Helper()
	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, raw)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		r.t.Fatalf("%q: %v", raw, err)
	}
	return resp.StatusCode, c, br
}

func connect(target string) string {
	return "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n"
}

func TestAllowedHostIsTunnelled(t *testing.T) {
	r := newRig(t, "api.anthropic.com")
	code, c, br := r.send(connect("api.anthropic.com:443"))
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	io.WriteString(c, "hello\n")
	if line, err := br.ReadString('\n'); err != nil || line != "hello\n" {
		t.Fatalf("echo %q, %v", line, err)
	}
	if len(r.dialed) != 1 || r.dialed[0].String() != "160.79.104.10:443" {
		t.Fatalf("dialed %v", r.dialed)
	}
	c.Close()
	r.mu.Lock()
	logs := r.logs.String()
	r.mu.Unlock()
	if !strings.Contains(logs, "result=allowed") || !strings.Contains(logs, "host=api.anthropic.com") || !strings.Contains(logs, "profile=agent") {
		t.Fatalf("log:\n%s", logs)
	}
}

func TestRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		want int
	}{
		"not on the list":           {connect("evil.example:443"), 403},
		"another port":              {connect("api.anthropic.com:80"), 403},
		"ssh port":                  {connect("api.anthropic.com:22"), 403},
		"literal address":           {connect("93.184.216.34:443"), 403},
		"literal loopback":          {connect("127.0.0.1:443"), 403},
		"literal v6":                {connect("[::1]:443"), 403},
		"allowed name, LAN address": {connect("lan.example:443"), 403},
		"metadata address":          {connect("meta.example:443"), 403},
		"mapped loopback":           {connect("mapped.example:443"), 403},
		"plain GET":                 {"GET http://api.anthropic.com/ HTTP/1.1\r\nHost: api.anthropic.com\r\n\r\n", 405},
		"POST":                      {"POST / HTTP/1.1\r\nHost: api.anthropic.com\r\nContent-Length: 0\r\n\r\n", 405},
		"garbage":                   {"\x16\x03\x01 not http\r\n\r\n", 400},
		"no port":                   {connect("api.anthropic.com"), 403},
		"unknown name":              {connect("nowhere.example:443"), 502},
		"destination down":          {connect("down.example:443"), 502},
		"data sent early":           {connect("api.anthropic.com:443") + "GET / HTTP/1.1\r\n\r\n", 400},
		"suffix trick":              {connect("api.anthropic.com.evil.example:443"), 403},
		"prefix trick":              {connect("evilapi.anthropic.com:443"), 403},
	} {
		r := newRig(t, "api.anthropic.com", "lan.example", "meta.example", "mapped.example", "nowhere.example", "down.example")
		if code, _, _ := r.send(tc.raw); code != tc.want {
			t.Errorf("%s: status %d, want %d", name, code, tc.want)
		}
		r.mu.Lock()
		n := len(r.dialed)
		r.mu.Unlock()
		if tc.want == 403 && n != 0 {
			t.Errorf("%s: a connection was attempted", name)
		}
	}
}

func TestOnlyPublicAddressesAreDialled(t *testing.T) {
	r := newRig(t, "mixed.example")
	if code, _, _ := r.send(connect("mixed.example:443")); code != 200 {
		t.Fatalf("status %d", code)
	}
	if len(r.dialed) != 1 || r.dialed[0].Addr().String() != "93.184.216.34" {
		t.Fatalf("dialed %v", r.dialed)
	}
}

func TestAllowlist(t *testing.T) {
	list := []string{"API.anthropic.com", "*.allowed.dev", " ", "*."}
	for host, want := range map[string]bool{
		"api.anthropic.com": true, "API.Anthropic.Com.": true, "sub.allowed.dev": true, "a.b.allowed.dev": true,
		"allowed.dev": false, "notallowed.dev": false, "": false, "anthropic.com": false, "x.api.anthropic.com": false,
	} {
		if got := allowed(list, host); got != want {
			t.Errorf("%q: %v", host, got)
		}
	}
	// The list is read for every request.
	var list2 []string
	r := newRig(t)
	r.p.Allow = func() []string { return list2 }
	if code, _, _ := r.send(connect("allowed.dev:443")); code != 403 {
		t.Fatalf("status %d before the reload", code)
	}
	list2 = []string{"allowed.dev"}
	if code, _, _ := r.send(connect("allowed.dev:443")); code != 200 {
		t.Fatalf("status %d after the reload", code)
	}
}

func TestPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"93.184.216.34": true, "2606:2800:220:1:248:1893:25c8:1946": true,
		"10.1.2.3": false, "172.16.0.1": false, "192.168.0.1": false, "127.0.0.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "0.0.0.0": false, "0.1.2.3": false, "224.0.0.1": false, "255.255.255.255": false, "240.0.0.1": false,
		"::1": false, "::": false, "fe80::1": false, "fc00::1": false, "fd12::1": false, "ff02::1": false,
		"::ffff:10.0.0.1": false, "::ffff:93.184.216.34": true, "64:ff9b::a00:1": false,
	} {
		if got := Public(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: %v", addr, got)
		}
	}
	if Public(netip.Addr{}) {
		t.Error("the zero address is public")
	}
}

func TestSlowClientIsDropped(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the header timeout")
	}
	r := newRig(t, "api.anthropic.com")
	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "CONNECT api.anthropic.com:443 HTTP/1.1\r\n")
	c.SetReadDeadline(time.Now().Add(headerTimeout + 5*time.Second))
	buf := make([]byte, 64)
	n, _ := c.Read(buf)
	if !strings.Contains(string(buf[:n]), "400") {
		t.Fatalf("answer %q", buf[:n])
	}
}
