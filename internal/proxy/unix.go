package proxy

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// ListenUnix listens on a unix socket. The systemd sandbox binds it into
// each unit with network (SPEC 11.3). Its folder must be closed to other
// users: the socket itself is open to all, because the sandbox user
// connects to it inside the unit.
func ListenUnix(path string) (net.Listener, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%s: the socket path must be absolute", path)
	}
	fi, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s: the folder of the socket must be closed to other users (chmod 700)", filepath.Dir(path))
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if Answers(path) {
			return nil, fmt.Errorf("%s: %w", path, ErrInUse)
		}
		// Left by an earlier run; nothing else is removed.
		os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// Answers reports whether something accepts connections on the socket.
func Answers(path string) bool {
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// ErrInUse is returned by ListenUnix when another proxy serves the socket.
var ErrInUse = errors.New("a proxy already listens on the socket")
