package wsl

import (
	"errors"
	"reflect"
	"testing"
)

func host(files map[string]string) Host {
	return Host{
		ReadFile: func(p string) ([]byte, error) {
			if s, ok := files[p]; ok {
				return []byte(s), nil
			}
			return nil, errors.New("no such file")
		},
		Getenv: func(k string) string {
			if k == "WSL_DISTRO_NAME" {
				return files["env:"+k]
			}
			return ""
		},
	}
}

func TestVersion(t *testing.T) {
	for release, want := range map[string]int{
		"5.15.153.1-microsoft-standard-WSL2\n": 2,
		"6.6.36.6-microsoft-standard-WSL2+\n":  2,
		"4.4.0-19041-Microsoft\n":              1,
		"6.8.0-45-generic\n":                   0,
		"4.18.0-553.el8_10.x86_64\n":           0,
	} {
		if got := host(map[string]string{"/proc/sys/kernel/osrelease": release}).Version(); got != want {
			t.Errorf("%q: version %d, want %d", release, got, want)
		}
	}
	if host(nil).Detect() {
		t.Error("no osrelease must not count as WSL")
	}
}

func TestSystemdAndLinger(t *testing.T) {
	h := host(map[string]string{"/proc/1/comm": "systemd\n", "/var/lib/systemd/linger/ian": ""})
	if !h.Systemd() {
		t.Error("systemd as PID 1 not seen")
	}
	if !h.Lingering("ian") || h.Lingering("other") || h.Lingering("") || h.Lingering("../ian") {
		t.Error("linger file not read as expected")
	}
	if host(map[string]string{"/proc/1/comm": "init\n"}).Systemd() {
		t.Error("WSL's own init taken for systemd")
	}
}

const mounts = `/dev/sdc / ext4 rw,relatime,discard,errors=remount-ro,data=ordered 0 0
none /mnt/wsl tmpfs rw,relatime 0 0
C:\134 /mnt/c 9p rw,noatime,dirsync,aname=drvfs;path=C:\;uid=1000;gid=1000 0 0
D:\134 /mnt/d drvfs rw,noatime 0 0
E:\134 /mnt/my\040drive 9p rw 0 0
`

func TestOnWindowsDrive(t *testing.T) {
	h := host(map[string]string{"/proc/self/mounts": mounts})
	for p, want := range map[string]bool{
		"/home/ian/maestro":           false,
		"/mnt/c/Users/ian/maestro":    true,
		"/mnt/c":                      true,
		"/mnt/d/work":                 true,
		"/mnt/cc/work":                false,
		"/mnt/wsl/x":                  false,
		"/mnt/my drive/maestro":       true,
		"/mnt/c/../../home/ian/state": false,
	} {
		if got := h.OnWindowsDrive(p); got != want {
			t.Errorf("%s: %v, want %v", p, got, want)
		}
	}
}

func TestKeepAlive(t *testing.T) {
	got := KeepAlive("Ubuntu-24.04", "nuc")
	want := []string{"schtasks.exe", "/Create", "/F", "/SC", "ONLOGON", "/RL", "LIMITED", "/TN", "Maestro nuc",
		"/TR", "powershell.exe -NoProfile -WindowStyle Hidden -Command wsl.exe -d Ubuntu-24.04 --exec /bin/sleep infinity"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	if s := Shell(got); s != `schtasks.exe /Create /F /SC ONLOGON /RL LIMITED /TN "Maestro nuc" /TR "powershell.exe -NoProfile -WindowStyle Hidden -Command wsl.exe -d Ubuntu-24.04 --exec /bin/sleep infinity"` {
		t.Errorf("shell line %s", s)
	}
	for _, bad := range []string{"", "Ubuntu; rm", `a"b`, "my distro"} {
		if KeepAlive(bad, "nuc") != nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
