package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// openURL opens a page in the browser. Replaced by tests.
var openURL = func(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, url).Run()
}

// runDashboard is `maestro dashboard`: it opens the running service's page,
// which listens on a loopback port of its own choosing.
func runDashboard(args []string, stdout, stderr io.Writer) int {
	dir, explicit, ok := parse("dashboard", args, stderr)
	if !ok {
		return 2
	}
	in, err := open(dir, explicit, nil)
	if err != nil {
		return report(stderr, err)
	}
	url, err := dashboardURL(in.cfg.Budget.StateDir)
	if err != nil {
		return report(stderr, err)
	}
	fmt.Fprintln(stdout, "The status page of worker "+in.cfg.Worker.Name+": "+url)
	fmt.Fprintln(stdout, "It refreshes by itself. It is only reachable on this machine; from another computer,")
	fmt.Fprintln(stdout, "first run there: ssh -N -L 8080:"+url[len("http://"):len(url)-1]+" <user>@<this-machine>  and open http://localhost:8080")
	if err := openURL(url); err != nil {
		fmt.Fprintln(stdout, "Open the address above in a browser.")
	}
	return 0
}

// dashboardURL reads where the running service listens. Only a loopback
// address is accepted.
func dashboardURL(stateDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(stateDir, serviceFile))
	if err != nil {
		return "", explain(notRunning)
	}
	var info serviceInfo
	if json.Unmarshal(raw, &info) != nil {
		return "", explain(notRunning)
	}
	host, port, err := net.SplitHostPort(info.Addr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() || port == "" {
		return "", explain(notRunning)
	}
	return "http://" + net.JoinHostPort(host, port) + "/", nil
}
