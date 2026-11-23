// Command maestro-launch is the root launcher of the systemd sandbox
// (SPEC 11.2, 11.3). The service runs it through one sudo rule:
//
//	maestro-launch <worker>     a request on standard input, see package launch
//	maestro-launch inside ...   the helper that runs in each unit, unprivileged
//
// It is installed root-owned at /usr/local/libexec/maestro-launch. Its
// configuration is /etc/maestro/launch-<worker>.json.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/iandeherdt/Maestro/internal/launch"
	"github.com/iandeherdt/Maestro/internal/sandbox"
)

// failed is the exit status of a request that was refused or could not
// run; a step's own status is passed on as it is.
const failed = 125

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) >= 1 && args[0] == "inside" {
		return launch.Inside(args[1:], stderr)
	}
	if len(args) != 1 || !sandbox.ValidWorker(args[0]) {
		fmt.Fprintln(stderr, "usage: maestro-launch <worker>   (a request on standard input)")
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "maestro-launch: must run as root, through its sudo rule")
		return failed
	}
	cfg, err := launch.Load(launch.ConfigPath(args[0]))
	if err != nil {
		fmt.Fprintln(stderr, "maestro-launch:", err)
		return failed
	}
	// The sudo rule names the service user; this is a second look.
	if s := os.Getenv("SUDO_UID"); s != "" {
		if uid, err := strconv.Atoi(s); err != nil || !isUser(cfg.ServiceUser, uid) {
			fmt.Fprintln(stderr, "maestro-launch: only the service user may run steps")
			return failed
		}
	}
	r := launch.NewReader(stdin)
	req, err := launch.ReadRequest(r)
	if err != nil {
		fmt.Fprintln(stderr, "maestro-launch:", err)
		return failed
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	l := &launch.Launcher{Cfg: cfg}
	code, err := l.Handle(ctx, req, r, stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "maestro-launch:", err)
		return failed
	}
	return code
}

func isUser(name string, uid int) bool {
	l := &launch.Launcher{}
	u, _, err := l.IDs(name)
	return err == nil && u == uid
}
