package setup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/wsl"
)

// Machine checks that the programs the service needs are present. It
// installs nothing: for each missing piece it says what to do.
type Machine struct {
	// LookPath and Output default to the real ones. Tests replace them.
	LookPath func(string) (string, error)
	Output   func(ctx context.Context, name string, args ...string) (string, error)
	GOOS     string
	Exists   func(path string) bool
	// WSL reads what tells a WSL distribution apart. Tests replace it.
	WSL wsl.Host
	// Dir is the setup folder, which must not be on a Windows drive in WSL.
	Dir string
}

func (m *Machine) defaults() {
	if m.LookPath == nil {
		m.LookPath = exec.LookPath
	}
	if m.Output == nil {
		m.Output = func(ctx context.Context, name string, args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, name, args...).Output()
			return string(out), err
		}
	}
	if m.GOOS == "" {
		m.GOOS = runtime.GOOS
	}
	if m.Exists == nil {
		m.Exists = func(p string) bool { _, err := os.Stat(p); return err == nil }
	}
}

func (*Machine) Title() string { return "Check this machine" }

var gitVersionRE = regexp.MustCompile(`git version (\d+)\.(\d+)`)

// problems lists what is missing, each with what to do about it.
func (m *Machine) problems(ctx context.Context) []string {
	m.defaults()
	var out []string
	if _, err := m.LookPath("git"); err != nil {
		out = append(out, "Git is not installed. Install it with your package manager, for example: sudo dnf install git")
	} else if v, err := m.Output(ctx, "git", "--version"); err != nil {
		out = append(out, "Git is installed but does not run: "+err.Error())
	} else if g := gitVersionRE.FindStringSubmatch(v); g == nil {
		out = append(out, "The Git version could not be read.")
	} else {
		major, _ := strconv.Atoi(g[1])
		minor, _ := strconv.Atoi(g[2])
		// 2.31 is what RHEL 8 ships and has everything the mirror uses.
		if major < 2 || (major == 2 && minor < 31) {
			out = append(out, fmt.Sprintf("Git %d.%d is too old, 2.31 or newer is needed.", major, minor))
		}
	}
	switch engine := Engine(m.GOOS, m.LookPath); {
	case engine == "" && m.GOOS == "darwin":
		out = append(out, "Docker is not installed. The agent runs in a Docker container. Install Docker Desktop from https://www.docker.com/products/docker-desktop/ and start it")
	case engine == "" && m.GOOS == "linux" && m.WSL.Version() == 2:
		out = append(out, "No container engine is reachable in this WSL distribution. Start Docker Desktop on Windows and turn on Settings, Resources, WSL integration for this distribution, or install Podman or Docker Engine inside it")
	case engine == "":
		out = append(out, "Neither Podman nor Docker is installed. The agent runs in a container. On RHEL and Fedora: sudo dnf install podman. Elsewhere, install Docker Engine as described at https://docs.docker.com/engine/install/")
	default:
		if _, err := m.Output(ctx, engine, "--version"); err != nil {
			out = append(out, engine+" is installed but does not run: "+err.Error())
		}
	}
	switch m.GOOS {
	case "linux":
		out = append(out, WSLProblems(m.WSL, m.Dir)...)
	case "darwin":
	default:
		out = append(out, "This operating system is not supported. The service runs on Linux and macOS, and on Windows inside WSL 2.")
	}
	return out
}

// WSLProblems lists what has to change before Maestro can run in this WSL
// distribution; nothing when this is not WSL. dirs are folders Maestro
// keeps files in.
func WSLProblems(h wsl.Host, dirs ...string) []string {
	switch h.Version() {
	case 0:
		return nil
	case 1:
		name := h.Distro()
		if name == "" {
			name = "<distribution>"
		}
		return []string{"This is WSL 1, which cannot run containers. In a Windows terminal run: wsl --set-version " + name + " 2"}
	}
	var out []string
	if !h.Systemd() {
		out = append(out, "systemd is off in this WSL distribution, and the service needs it. Add these two lines to /etc/wsl.conf "+
			"(sudo nano /etc/wsl.conf): [boot] and systemd=true. Then run wsl --shutdown in a Windows terminal and open the distribution again")
	}
	for _, d := range dirs {
		if d != "" && h.OnWindowsDrive(d) {
			out = append(out, d+" is on a Windows drive. There Maestro is slow and cannot keep its key private. "+
				"Use a folder on the distribution's own disk instead, such as one in your Linux home (cd ~)")
			break
		}
	}
	return out
}

func (m *Machine) Check(ctx context.Context, _ *State) (Status, error) {
	p := m.problems(ctx)
	if len(p) == 0 {
		return Status{Done: true, Detail: "Git and a container engine (Docker or Podman) are installed."}, nil
	}
	return Status{Detail: fmt.Sprintf("%d thing(s) to fix: %s", len(p), strings.Join(short(p), "; "))}, nil
}

func (m *Machine) Run(ctx context.Context, ui *UI, _ *State) error {
	for {
		p := m.problems(ctx)
		if len(p) == 0 {
			return nil
		}
		ui.Say("  Please fix the following, in another terminal:")
		for _, line := range p {
			ui.Say("    - %s", line)
		}
		ok, err := ui.Confirm("  Check again?")
		if err != nil {
			return err
		}
		if !ok {
			return ErrAborted
		}
	}
}

// short keeps the first sentence of each problem, for the one-line status.
func short(problems []string) []string {
	out := make([]string, len(problems))
	for i, p := range problems {
		if at := strings.Index(p, ". "); at > 0 {
			p = p[:at]
		}
		out[i] = strings.TrimSuffix(p, ".")
	}
	return out
}

// Engine picks the container command for this machine: Docker on a Mac;
// on Linux Podman when it is there, because it needs no root daemon, and
// Docker otherwise. It returns "" when there is none.
func Engine(goos string, lookPath func(string) (string, error)) string {
	order := []string{"docker"}
	if goos == "linux" {
		order = []string{"podman", "docker"}
	}
	for _, e := range order {
		if _, err := lookPath(e); err == nil {
			return e
		}
	}
	return ""
}
