// Package wsl recognises Windows Subsystem for Linux 2, which is how
// Maestro runs on Windows: as its Linux build, inside a WSL distribution.
//
// Three things differ from a Linux machine of its own, and this package
// finds them so that setup and the service can say what to do:
//
//   - systemd may be off, and then a user service cannot be installed;
//   - Windows stops the distribution shortly after its last terminal
//     closes and does not start it at boot, so something on the Windows
//     side has to start it and keep it running;
//   - folders on a Windows drive (/mnt/c and the like) are slow and do not
//     keep Unix file permissions, which Maestro's key and state rely on.
package wsl

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Host reads what it needs from the running system. The zero value reads
// the real files and environment; tests replace them.
type Host struct {
	ReadFile func(string) ([]byte, error)
	Getenv   func(string) string
}

func (h Host) readOK(p string) ([]byte, bool) {
	f := h.ReadFile
	if f == nil {
		f = os.ReadFile
	}
	b, err := f(p)
	return b, err == nil
}

func (h Host) read(p string) []byte { b, _ := h.readOK(p); return b }

func (h Host) env(k string) string {
	if h.Getenv == nil {
		return os.Getenv(k)
	}
	return h.Getenv(k)
}

// Detect reports whether this is a WSL 2 distribution. WSL 1 has no real
// Linux kernel and cannot run containers; it is reported as WSL too, and
// Version tells the two apart.
func (h Host) Detect() bool {
	r := strings.ToLower(string(h.read("/proc/sys/kernel/osrelease")))
	return strings.Contains(r, "microsoft") || strings.Contains(r, "wsl")
}

// Version is 2 for WSL 2, 1 for WSL 1 and 0 when this is not WSL.
func (h Host) Version() int {
	if !h.Detect() {
		return 0
	}
	r := strings.ToLower(string(h.read("/proc/sys/kernel/osrelease")))
	// WSL 2 kernels end in "-microsoft-standard-WSL2"; WSL 1 reports the
	// Windows build, such as "4.4.0-19041-Microsoft".
	if strings.Contains(r, "wsl2") || strings.Contains(r, "microsoft-standard") {
		return 2
	}
	return 1
}

// Distro is the name of this distribution as Windows knows it ("Ubuntu").
func (h Host) Distro() string { return h.env("WSL_DISTRO_NAME") }

// Systemd reports whether systemd is the first process.
func (h Host) Systemd() bool {
	return strings.TrimSpace(string(h.read("/proc/1/comm"))) == "systemd"
}

// Lingering reports whether systemd starts the user's services without a
// login (loginctl enable-linger).
func (h Host) Lingering(user string) bool {
	if user == "" {
		return false
	}
	if strings.ContainsAny(user, "/\x00") || user == "." || user == ".." {
		return false
	}
	// The file is empty: being able to read it is the answer.
	_, ok := h.readOK("/var/lib/systemd/linger/" + user)
	return ok
}

// OnWindowsDrive reports whether an absolute path is on a drive that
// Windows shares into the distribution (a 9p or drvfs mount such as
// /mnt/c) rather than on the distribution's own disk.
func (h Host) OnWindowsDrive(path string) bool {
	path = filepath.Clean(path)
	best, kind := "", ""
	sc := bufio.NewScanner(bytes.NewReader(h.read("/proc/self/mounts")))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 {
			continue
		}
		mp := unescape(f[1])
		if (path == mp || strings.HasPrefix(path, strings.TrimSuffix(mp, "/")+"/")) && len(mp) > len(best) {
			best, kind = mp, f[2]
		}
	}
	return kind == "9p" || kind == "drvfs" || kind == "v9fs"
}

// unescape undoes the octal escapes /proc/self/mounts uses for spaces and
// the like.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

var distroRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// TaskName is the name of the Windows scheduled task that keeps the
// distribution running for one worker.
func TaskName(worker string) string { return "Maestro " + worker }

// KeepAlive is the Windows command that creates the scheduled task: at
// each Windows login it starts the distribution, without a window, and
// keeps it running so that systemd and the service stay up. It is meant
// for a Windows terminal (PowerShell or Command Prompt), and also works
// from inside WSL as schtasks.exe. It returns nil when the distribution's
// name cannot be put in a command safely.
func KeepAlive(distro, worker string) []string {
	if !distroRE.MatchString(distro) {
		return nil
	}
	run := "powershell.exe -NoProfile -WindowStyle Hidden -Command wsl.exe -d " + distro +
		" --exec /bin/sleep infinity"
	return []string{"schtasks.exe", "/Create", "/F", "/SC", "ONLOGON", "/RL", "LIMITED",
		"/TN", TaskName(worker), "/TR", run}
}

// RemoveKeepAlive is the command that deletes that task again.
func RemoveKeepAlive(worker string) []string {
	return []string{"schtasks.exe", "/Delete", "/F", "/TN", TaskName(worker)}
}

// Shell turns a command into one line to paste into a Windows terminal.
func Shell(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if strings.ContainsAny(a, " \t") {
			a = `"` + a + `"`
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}
