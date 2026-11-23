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

// SandboxSystemd is the State.Sandbox of the systemd sandbox.
const SandboxSystemd = "systemd"

var systemdVersionRE = regexp.MustCompile(`systemd (\d+)`)
var nodeVersionRE = regexp.MustCompile(`^v(\d+)\.`)

// systemdProblems is what the systemd sandbox needs instead of a container
// engine: systemd, sudo, and Node.js with npm, from which setup installs
// the pinned Claude Code for the steps.
func (m *Machine) systemdProblems(ctx context.Context) []string {
	if m.GOOS != "linux" || m.WSL.Version() != 0 {
		return []string{"The systemd sandbox runs on Linux only, not in WSL. Run setup without --sandbox to use containers"}
	}
	var out []string
	if v, err := m.Output(ctx, "systemctl", "--version"); err != nil {
		out = append(out, "systemd does not answer (systemctl --version). The systemd sandbox needs it as the machine's init system")
	} else if g := systemdVersionRE.FindStringSubmatch(v); g == nil {
		out = append(out, "The systemd version could not be read.")
	} else if n, _ := strconv.Atoi(g[1]); n < 239 {
		out = append(out, fmt.Sprintf("systemd %d is too old; 239 (RHEL 8) or newer is needed, 250 or newer is best.", n))
	}
	if !m.Exists("/sys/fs/cgroup/cgroup.controllers") {
		out = append(out, "This machine uses cgroups v1, where systemd cannot apply the sandbox's memory and CPU limits. "+
			"Switch to cgroups v2 and restart: sudo grubby --update-kernel=ALL --args=systemd.unified_cgroup_hierarchy=1 && sudo reboot")
	}
	if _, err := m.LookPath("sudo"); err != nil {
		out = append(out, "sudo is not installed. Setup uses it once to install the sandbox's root launcher: sudo dnf install sudo")
	}
	if v, err := m.Output(ctx, "/usr/bin/node", "--version"); err != nil {
		out = append(out, "Node.js is not installed in /usr/bin. The steps run Claude Code with it: sudo dnf install nodejs npm")
	} else if g := nodeVersionRE.FindStringSubmatch(strings.TrimSpace(v)); g == nil {
		out = append(out, "The Node.js version could not be read.")
	} else if n, _ := strconv.Atoi(g[1]); n < 18 {
		out = append(out, fmt.Sprintf("Node.js %d is too old; 18 or newer is needed: sudo dnf module install nodejs:20", n))
	}
	if _, err := m.LookPath("npm"); err != nil {
		out = append(out, "npm is not installed. Setup installs Claude Code for the steps with it: sudo dnf install npm")
	}
	return out
}

var gitVersionRE = regexp.MustCompile(`git version (\d+)\.(\d+)`)

// problems lists what is missing, each with what to do about it.
func (m *Machine) problems(ctx context.Context, sandbox string) []string {
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
	case sandbox == SandboxSystemd:
		out = append(out, m.systemdProblems(ctx)...)
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

func (m *Machine) Check(ctx context.Context, st *State) (Status, error) {
	sandbox := ""
	if st != nil {
		sandbox = st.Sandbox
	}
	p := m.problems(ctx, sandbox)
	if len(p) == 0 {
		if sandbox == SandboxSystemd {
			return Status{Done: true, Detail: "Git, systemd, sudo and Node.js are installed."}, nil
		}
		return Status{Done: true, Detail: "Git and a container engine (Docker or Podman) are installed."}, nil
	}
	return Status{Detail: fmt.Sprintf("%d thing(s) to fix: %s", len(p), strings.Join(short(p), "; "))}, nil
}

func (m *Machine) Run(ctx context.Context, ui *UI, st *State) error {
	sandbox := ""
	if st != nil {
		sandbox = st.Sandbox
	}
	for {
		p := m.problems(ctx, sandbox)
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

var backendLineRE = regexp.MustCompile(`(?m)^  backend: (docker|podman|systemd)$`)

// SwitchSandbox chooses containers ("" or "container") or SandboxSystemd
// for the steps. A settings file setup wrote earlier gets the new backend
// on its one `backend:` line; a file that does not have exactly one is left
// alone, and the error says what to change.
func SwitchSandbox(st *State, kind string, goos string, lookPath func(string) (string, error)) error {
	if kind == "container" {
		kind = ""
	}
	if kind != "" && kind != SandboxSystemd {
		return fmt.Errorf("--sandbox is container or %s", SandboxSystemd)
	}
	if st.Sandbox == kind {
		return nil
	}
	st.Sandbox = kind
	if st.WorkflowFile != "" {
		raw, err := os.ReadFile(st.WorkflowFile)
		if err == nil {
			if n := len(backendLineRE.FindAllIndex(raw, -1)); n != 1 {
				return fmt.Errorf("%s has %d `backend:` lines under sandbox; set `sandbox.backend` in it yourself", st.WorkflowFile, n)
			}
			backend := kind
			if backend == "" {
				if backend = Engine(goos, lookPath); backend == "" {
					backend = "docker"
				}
			}
			raw = backendLineRE.ReplaceAll(raw, []byte("  backend: "+backend))
			if err := os.WriteFile(st.WorkflowFile, raw, 0o600); err != nil {
				return err
			}
		}
	}
	return st.Save()
}
