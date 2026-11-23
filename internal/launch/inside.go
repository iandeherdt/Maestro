package launch

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// Inside runs in the unit, as the sandbox user. With a proxy socket it
// serves that socket on the loopback port the step's proxy variables
// name, because the unit has no network of its own. It then runs argv and
// exits with its status. It is not privileged; it only bridges.
func Inside(args []string, stderr io.Writer) int {
	sock := ""
	for len(args) > 0 && args[0] != "--" {
		if len(args) >= 2 && args[0] == "--proxy" {
			sock, args = args[1], args[2:]
			continue
		}
		fmt.Fprintln(stderr, "maestro-launch inside: usage: inside [--proxy SOCKET] -- command...")
		return 125
	}
	if len(args) < 2 {
		fmt.Fprintln(stderr, "maestro-launch inside: no command")
		return 125
	}
	argv := args[1:]
	if sock != "" {
		ln, err := net.Listen("tcp", proxyAddr)
		if err != nil {
			fmt.Fprintln(stderr, "maestro-launch inside:", err)
			return 125
		}
		go relay(ln, sock)
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintln(stderr, "maestro-launch inside:", err)
		return 127
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Args[0] = argv[0]
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, stderr
	// The unit is stopped with SIGTERM to the whole group; pass it on in
	// case the command waits for it.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(stderr, "maestro-launch inside:", err)
		return 126
	}
	go func() {
		for s := range sig {
			cmd.Process.Signal(s)
		}
	}()
	err = cmd.Wait()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	fmt.Fprintln(stderr, "maestro-launch inside:", err)
	return 125
}

// relay forwards every connection on ln to the unix socket.
func relay(ln net.Listener, sock string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			up, err := net.Dial("unix", sock)
			if err != nil {
				return
			}
			defer up.Close()
			done := make(chan struct{}, 2)
			go func() { io.Copy(up, c); closeWrite(up); done <- struct{}{} }()
			go func() { io.Copy(c, up); closeWrite(c); done <- struct{}{} }()
			<-done
			<-done
		}()
	}
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}
