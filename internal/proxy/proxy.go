// Package proxy is the only way out of the sandbox (SPEC 11.4): a forward
// proxy that accepts CONNECT to port 443 for host names on an allowlist
// and nothing else. It resolves names itself, refuses destinations on
// private, loopback or link-local addresses, connects to the address it
// checked, and logs every request.
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
	"time"
)

const (
	headerTimeout = 10 * time.Second
	dialTimeout   = 15 * time.Second
	maxHeader     = 16 << 10
)

// DefaultAgentHosts are the hosts Claude Code needs (SPEC 11.4).
var DefaultAgentHosts = []string{"api.anthropic.com", "claude.ai", "platform.claude.com"}

// Proxy serves one network profile. Run one per profile, each on its own
// loopback port, so the sandbox decides which one a step can reach.
type Proxy struct {
	// Profile names the allowlist in the log, such as "agent".
	Profile string
	// Allow returns the allowed host names. It is read for every request,
	// so a reloaded workflow applies at once. An entry "*.example.com"
	// allows any name below example.com, not example.com itself.
	Allow func() []string
	Log   *slog.Logger
	// Lookup and Dial are replaced in tests. The address check between
	// them is always applied.
	Lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	Dial   func(ctx context.Context, to netip.AddrPort) (net.Conn, error)

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func (p *Proxy) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	if p.Lookup != nil {
		return p.Lookup(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

func (p *Proxy) dial(ctx context.Context, to netip.AddrPort) (net.Conn, error) {
	if p.Dial != nil {
		return p.Dial(ctx, to)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", to.String())
}

// allowed reports whether host is on the list.
func allowed(list []string, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	for _, e := range list {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(e, "*."); ok {
			if rest != "" && strings.HasSuffix(host, "."+rest) {
				return true
			}
			continue
		}
		if e == host {
			return true
		}
	}
	return false
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// Public reports whether a is an address on the public internet. A name
// on the allowlist that resolves to anything else is refused, so the
// allowlist cannot be turned into a way onto the home network.
func Public(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsUnspecified() || a.IsLoopback() || a.IsPrivate() || a.IsMulticast() ||
		a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() {
		return false
	}
	if a.Is4() {
		b := a.As4()
		// Shared address space, 0.0.0.0/8, reserved and broadcast.
		if cgnat.Contains(a) || b[0] == 0 || b[0] >= 240 {
			return false
		}
		return true
	}
	// Only global unicast IPv6 (2000::/3). That leaves out unique local,
	// site local, NAT64 and other translation prefixes.
	return a.As16()[0]&0xe0 == 0x20
}

// Serve accepts connections until the listener is closed.
func (p *Proxy) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go p.handle(c)
	}
}

// CloseConnections ends every tunnel, for shutdown.
func (p *Proxy) CloseConnections() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		c.Close()
	}
}

func (p *Proxy) track(c net.Conn, on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conns == nil {
		p.conns = map[net.Conn]struct{}{}
	}
	if on {
		p.conns[c] = struct{}{}
	} else {
		delete(p.conns, c)
	}
}

func (p *Proxy) log(result, host, detail string) {
	if p.Log != nil {
		p.Log.Info("proxy request", "profile", p.Profile, "result", result, "host", host, "detail", detail)
	}
}

func refuse(c net.Conn, status int) {
	text := http.StatusText(status)
	io.WriteString(c, "HTTP/1.1 "+itoa(status)+" "+text+"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
}

func itoa(n int) string {
	return string([]byte{byte('0' + n/100), byte('0' + n/10%10), byte('0' + n%10)})
}

func (p *Proxy) handle(c net.Conn) {
	defer c.Close()
	p.track(c, true)
	defer p.track(c, false)

	c.SetReadDeadline(time.Now().Add(headerTimeout))
	br := bufio.NewReaderSize(io.LimitReader(c, maxHeader), maxHeader)
	req, err := http.ReadRequest(br)
	if err != nil {
		p.log("refused", "", "unreadable request")
		refuse(c, http.StatusBadRequest)
		return
	}
	if req.Method != http.MethodConnect {
		p.log("refused", req.Host, "method "+req.Method)
		refuse(c, http.StatusMethodNotAllowed)
		return
	}
	host, port, err := net.SplitHostPort(req.RequestURI)
	if err != nil || port != "443" {
		p.log("refused", req.RequestURI, "not a host on port 443")
		refuse(c, http.StatusForbidden)
		return
	}
	// A literal address is never on the list: only names are.
	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil || !allowed(p.Allow(), host) {
		p.log("refused", host, "not on the allowlist")
		refuse(c, http.StatusForbidden)
		return
	}
	// Bytes sent ahead of the answer would be read by the proxy, not the
	// destination. A well-behaved client waits for the 200.
	if br.Buffered() > 0 {
		p.log("refused", host, "data before the tunnel was open")
		refuse(c, http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	addrs, err := p.lookup(ctx, strings.TrimSuffix(host, "."))
	if err != nil || len(addrs) == 0 {
		p.log("failed", host, "name not resolved")
		refuse(c, http.StatusBadGateway)
		return
	}
	var up net.Conn
	checked := 0
	for _, a := range addrs {
		if !Public(a) {
			continue
		}
		checked++
		// The connection goes to the address that was checked, not to a
		// name that could resolve differently a moment later.
		if up, err = p.dial(ctx, netip.AddrPortFrom(a.Unmap(), 443)); err == nil {
			break
		}
	}
	if checked == 0 {
		p.log("refused", host, "resolves to a non-public address")
		refuse(c, http.StatusForbidden)
		return
	}
	if up == nil {
		p.log("failed", host, "connection failed")
		refuse(c, http.StatusBadGateway)
		return
	}
	defer up.Close()
	p.track(up, true)
	defer p.track(up, false)

	c.SetReadDeadline(time.Time{})
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	p.log("allowed", host, "")
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, c); closeWrite(up); done <- struct{}{} }()
	go func() { io.Copy(c, up); closeWrite(c); done <- struct{}{} }()
	<-done
	// One side finished. Give the other a moment to drain, then close.
	select {
	case <-done:
	case <-time.After(30 * time.Second):
	}
}

func closeWrite(c net.Conn) {
	if t, ok := c.(*net.TCPConn); ok {
		t.CloseWrite()
	}
}
