package systemdbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/iandeherdt/Maestro/internal/proxy"
	"github.com/iandeherdt/Maestro/internal/sandbox"
)

// Profiles are the network profiles that have a proxy.
var Profiles = []string{sandbox.NetAgent, sandbox.NetHooks}

// Socket is where the proxy of a profile listens. The launcher's
// configuration names the same paths (proxy_sockets).
func (b *Box) Socket(profile string) string { return filepath.Join(b.SocketDir, profile+".sock") }

// proxies are the proxies this process serves.
type proxies struct {
	mu   sync.Mutex
	lns  []net.Listener
	srvs []*proxy.Proxy
}

// Up starts a proxy for each profile, in this process, on its socket.
// allowDir holds the allowlist files (proxy.WriteAllow). A profile whose
// socket already answers is served by another Maestro process of this
// installation, such as the running service while `maestro login` runs,
// and is left to it.
func (b *Box) Up(ctx context.Context, allowDir string) error {
	if b.SocketDir == "" {
		return fail("no folder for the proxy sockets is configured")
	}
	if err := os.MkdirAll(b.SocketDir, 0o700); err != nil {
		return fail("proxy sockets: %v", err)
	}
	if err := os.Chmod(b.SocketDir, 0o700); err != nil {
		return fail("proxy sockets: %v", err)
	}
	log := b.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	b.px.mu.Lock()
	defer b.px.mu.Unlock()
	for _, profile := range Profiles {
		ln, err := proxy.ListenUnix(b.Socket(profile))
		if errors.Is(err, proxy.ErrInUse) {
			continue
		}
		if err != nil {
			b.down()
			return fail("the %s proxy: %v", profile, err)
		}
		file := filepath.Join(allowDir, profile+".json")
		p := &proxy.Proxy{Profile: profile, Log: log, Allow: func() []string { return proxy.ReadAllow(file) }}
		b.px.lns, b.px.srvs = append(b.px.lns, ln), append(b.px.srvs, p)
		go p.Serve(ln)
	}
	return nil
}

// Down stops the proxies this process started.
func (b *Box) Down(context.Context) error {
	b.px.mu.Lock()
	defer b.px.mu.Unlock()
	b.down()
	return nil
}

func (b *Box) down() {
	for _, ln := range b.px.lns {
		ln.Close()
	}
	for _, p := range b.px.srvs {
		p.CloseConnections()
	}
	b.px.lns, b.px.srvs = nil, nil
}

// ProxyRunning reports whether the proxy of a profile answers.
func (b *Box) ProxyRunning(_ context.Context, profile string) bool {
	return b.SocketDir != "" && proxy.Answers(b.Socket(profile))
}

// Login runs Claude Code's sign-in in the sandbox, on this terminal. The
// launcher chooses the command; argv is the docker backend's and is not
// used.
func (b *Box) Login(ctx context.Context, _ []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if !sandbox.ValidWorker(b.Worker) {
		return fail("no valid worker name is configured")
	}
	cmd := b.command(ctx, "login")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.WaitDelay = stopTimeout
	return cmd.Run()
}

// LoggedIn asks Claude Code in the sandbox whether it is signed in.
func (b *Box) LoggedIn(ctx context.Context, _ string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res, err := b.Run(ctx, sandbox.Request{Step: sandbox.StepLoginStatus, NetworkProfile: sandbox.NetAgent}, nil)
	if err != nil {
		return false, err
	}
	yes, known := loginAnswer(res.Stdout, res.ExitStatus)
	if !known {
		return false, fail("the login could not be checked: %s", lastLine(res.Stderr))
	}
	return yes, nil
}

// loginAnswer reads `claude auth status --json`: its JSON when there is
// some, otherwise its status (1 is "not signed in").
func loginAnswer(stdout []byte, status int) (loggedIn, known bool) {
	var st struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	if json.Unmarshal(stdout, &st) == nil && st.LoggedIn != nil {
		return *st.LoggedIn, true
	}
	switch status {
	case 0:
		return true, true
	case 1:
		return false, true
	}
	return false, false
}

// SelfTest runs the probe through the launcher for each network profile.
// The proxies must be up (Up).
func (b *Box) SelfTest(ctx context.Context) (sandbox.SelfTestReport, error) {
	var rep sandbox.SelfTestReport
	for _, profile := range []string{sandbox.NetNone, sandbox.NetHooks, sandbox.NetAgent} {
		c, cancel := context.WithTimeout(ctx, 3*time.Minute)
		res, err := b.Run(c, sandbox.Request{Step: sandbox.StepSelfTest, NetworkProfile: profile}, nil)
		cancel()
		if err != nil {
			return rep, err
		}
		checks, perr := sandbox.ParseProbe(res.Stdout, profile)
		if perr != nil || len(checks) == 0 {
			text := string(res.Stdout) + string(res.Stderr)
			if strings.Contains(text, "usage:") {
				// The maestro the steps run is older than this one.
				rep.OldImage = true
				return rep, nil
			}
			return rep, fail("the self-test gave no answer: %s", lastLine(res.Stderr))
		}
		rep.Checks = append(rep.Checks, checks...)
	}
	return rep, nil
}
