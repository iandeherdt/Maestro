package dockerbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/sandbox"
)

// ClaudeVersion is the Claude Code release the image is built with. The
// version is pinned because a later release may change what print mode
// does with the login (SPEC 10.1). The scheduled pin check reads the
// marker below; keep it next to the constant.
//
// pin: npm @anthropic-ai/claude-code
const ClaudeVersion = "2.1.330"

const proxyConfigDir = "/etc/maestro-proxy"

// dockerfileHead builds everything except the service's own binary.
const dockerfileHead = `FROM docker.io/library/node:22-bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
ARG CLAUDE_VERSION
RUN npm install --global --no-fund --no-audit "@anthropic-ai/claude-code@${CLAUDE_VERSION}" \
 && npm cache clean --force
RUN groupadd --gid 10001 agent \
 && useradd --uid 10001 --gid 10001 --create-home agent \
 && mkdir -p /workspace /claude /handoff \
 && chown agent:agent /workspace /claude /handoff \
 && git config --system --add safe.directory /mirror \
 && git config --system --add safe.directory /workspace/repo
`

// The binary is either copied from the build context (a Linux host), or
// downloaded from the release and checked against its checksum file.
const dockerfileCopy = `COPY maestro /usr/local/bin/maestro
USER agent
`

const dockerfileDownload = `ARG MAESTRO_VERSION
ARG TARGETARCH
RUN set -eu; \
    base="https://github.com/iandeherdt/Maestro/releases/download/${MAESTRO_VERSION}"; \
    file="maestro_linux_${TARGETARCH}.tar.gz"; \
    cd /tmp; \
    curl -fsSL --proto '=https' --tlsv1.2 -o "$file" "$base/$file"; \
    curl -fsSL --proto '=https' --tlsv1.2 -o SHA256SUMS "$base/SHA256SUMS"; \
    grep " $file\$" SHA256SUMS | sha256sum -c -; \
    tar -xzf "$file" -C /usr/local/bin maestro; \
    rm -f "$file" SHA256SUMS
USER agent
`

// Build describes how the image is built.
type Build struct {
	// Dockerfile replaces the built-in one. Tests use a small image.
	Dockerfile string
	// Binary is a Linux build of this program for the image's
	// architecture, copied into the image. When empty, the release named
	// by Version is downloaded inside the build.
	Binary  string
	Version string
	Out     io.Writer
	// Extra are Dockerfile lines that add the tools of the rule packs.
	// They run as root, before the image switches to the agent's user.
	Extra []string
	// Labels are put on the image, such as which packs it was built for.
	Labels map[string]string
}

var versionRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

func extra(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// BuildImage builds the sandbox image under the configured name.
func (b *Box) BuildImage(ctx context.Context, o Build) error {
	if err := b.check(); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "maestro-image-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	text := o.Dockerfile
	args := []string{"build", "--tag", b.Image, "--build-arg", "CLAUDE_VERSION=" + ClaudeVersion}
	if b.Platform != "" {
		args = append(args, "--platform", b.Platform)
	}
	for _, k := range sortedKeys(o.Labels) {
		args = append(args, "--label", k+"="+o.Labels[k])
	}
	switch {
	case o.Binary != "":
		raw, err := os.ReadFile(o.Binary)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "maestro"), raw, 0o755); err != nil {
			return err
		}
		if text == "" {
			text = dockerfileHead + extra(o.Extra) + dockerfileCopy
		}
	case text != "":
	case versionRE.MatchString(o.Version):
		text = dockerfileHead + extra(o.Extra) + dockerfileDownload
		args = append(args, "--build-arg", "MAESTRO_VERSION="+o.Version)
	default:
		return fmt.Errorf("the image needs a released version of maestro; this build is %q", o.Version)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(text), 0o644); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, b.docker(), append(args, dir)...)
	cmd.Stdout, cmd.Stderr = o.Out, o.Out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("the image could not be built: %w", err)
	}
	return nil
}

// ImageExists reports whether the sandbox image is present.
func (b *Box) ImageExists(ctx context.Context) bool {
	ok, _ := b.ImageState(ctx)
	return ok
}

// ImageState says whether the sandbox image exists. err is Docker's own
// answer when it could not tell, such as a daemon that is still starting.
func (b *Box) ImageState(ctx context.Context) (bool, error) {
	_, err := b.cli(ctx, "image", "inspect", "--format", "{{.Id}}", b.Image)
	if err == nil {
		return true, nil
	}
	if strings.Contains(strings.ToLower(err.Error()), "no such image") {
		return false, nil
	}
	return false, err
}

// ImageLabel is a label of the sandbox image; ok is false when there is no
// image.
func (b *Box) ImageLabel(ctx context.Context, key string) (value string, ok bool) {
	out, err := b.cli(ctx, "image", "inspect", "--format", "{{index .Config.Labels \""+key+"\"}}", b.Image)
	if err != nil {
		return "", false
	}
	v := strings.TrimSpace(out)
	if v == "<no value>" {
		v = ""
	}
	return v, true
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// allowFile is what a proxy reads its allowlist from.
type allowFile struct {
	Hosts []string `json:"hosts"`
}

// WriteAllow replaces the allowlist of a profile. The proxy reads the file
// for every request, so the change applies at once.
func WriteAllow(dir, profile string, hosts []string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if hosts == nil {
		hosts = []string{}
	}
	raw, err := json.Marshal(allowFile{Hosts: hosts})
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+profile+".json.tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, profile+".json"))
}

// ReadAllow reads an allowlist. A file that cannot be read allows nothing.
func ReadAllow(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f allowFile
	if json.Unmarshal(raw, &f) != nil {
		return nil
	}
	return f.Hosts
}

// Profiles are the network profiles that have a proxy.
var Profiles = []string{sandbox.NetAgent, sandbox.NetHooks}

// Up creates what the steps need and starts the proxies: one internal
// network and one proxy per profile, and the volume for the agent's
// Claude Code state. allowDir holds the allowlist files (see WriteAllow)
// and is mounted read-only into the proxies.
//
// A proxy has two network interfaces: the internal network, where the
// sandbox reaches it, and Docker's default bridge, where it reaches the
// internet. A sandbox container has only the first.
func (b *Box) Up(ctx context.Context, allowDir string) error {
	if err := b.check(); err != nil {
		return err
	}
	if !filepath.IsAbs(allowDir) || strings.ContainsAny(allowDir, ",\n") {
		return fail("the allowlist directory must be an absolute path without commas")
	}
	if err := b.Down(ctx); err != nil {
		return err
	}
	if out, _ := b.cli(ctx, "volume", "ls", "--quiet", "--filter", "name=^"+regexp.QuoteMeta(b.ClaudeVolume())+"$"); out == "" {
		if _, err := b.cli(ctx, "volume", "create", "--label", "maestro.worker="+b.Worker, b.ClaudeVolume()); err != nil {
			return fail("%v", err)
		}
	}
	for _, profile := range Profiles {
		net, name := b.Network(profile), b.ProxyName(profile)
		if out, _ := b.cli(ctx, "network", "ls", "--quiet", "--filter", "name=^"+regexp.QuoteMeta(net)+"$"); out == "" {
			if _, err := b.cli(ctx, "network", "create", "--internal", "--label", "maestro.worker="+b.Worker, net); err != nil {
				return fail("%v", err)
			}
		}
		if _, err := b.cli(ctx, append(append([]string{"create", "--name", name}, b.engineFlags()...),
			"--label", "maestro.worker="+b.Worker, "--label", "maestro.step=proxy",
			"--user", AgentUID+":"+AgentUID, "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
			"--read-only", "--memory", "256m", "--pids-limit", "256", "--init",
			"--network", "bridge",
			"--mount", b.bind(allowDir, proxyConfigDir, true),
			"--entrypoint", "", b.Image,
			"maestro", "proxy", "--listen", ":"+ProxyPort, "--profile", profile,
			"--allow-file", proxyConfigDir+"/"+profile+".json")...); err != nil {
			return fail("%v", err)
		}
		if _, err := b.cli(ctx, "network", "connect", net, name); err != nil {
			return fail("%v", err)
		}
		if _, err := b.cli(ctx, "start", name); err != nil {
			return fail("%v", err)
		}
	}
	// A proxy that exits at once (a broken image, a bad flag) is reported
	// here and not as a puzzling network failure in the first step.
	time.Sleep(500 * time.Millisecond)
	for _, profile := range Profiles {
		if out, err := b.cli(ctx, "inspect", "--format", "{{.State.Running}}", b.ProxyName(profile)); err != nil || out != "true" {
			logs, _ := exec.CommandContext(ctx, b.docker(), "logs", "--tail", "5", b.ProxyName(profile)).CombinedOutput()
			return fail("the %s proxy did not stay up: %s", profile, lastLine(logs))
		}
	}
	return nil
}

// Down removes every container of this worker: the proxies and any step
// left over from an earlier run (SPEC 11.5). Volumes and networks stay.
func (b *Box) Down(ctx context.Context) error {
	if err := b.check(); err != nil {
		return err
	}
	ids, err := b.cli(ctx, "ps", "--all", "--quiet", "--filter", "label=maestro.worker="+b.Worker)
	if err != nil {
		return fail("%v", err)
	}
	if list := strings.Fields(ids); len(list) > 0 {
		if _, err := b.cli(ctx, append([]string{"rm", "--force"}, list...)...); err != nil {
			return fail("%v", err)
		}
	}
	return nil
}

// Purge removes everything Up and the steps created, including the
// workspaces and the agent's login. For tests and for uninstalling.
func (b *Box) Purge(ctx context.Context) error {
	if err := b.Down(ctx); err != nil {
		return err
	}
	vols, _ := b.cli(ctx, "volume", "ls", "--quiet", "--filter", "label=maestro.worker="+b.Worker)
	for _, v := range strings.Fields(vols) {
		b.cli(ctx, "volume", "rm", "--force", v)
	}
	for _, p := range Profiles {
		b.cli(ctx, "network", "rm", b.Network(p))
	}
	return nil
}

// claudeArgs is a container that has the agent's Claude Code state and the
// agent network, and no workspace. It is used to log in and to ask whether
// the login is still good.
func (b *Box) claudeArgs(name string, terminal bool, argv ...string) []string {
	args := []string{"run", "--rm", "--interactive", "--name", name,
		"--label", "maestro.worker=" + b.Worker, "--label", "maestro.step=login",
		"--user", AgentUID + ":" + AgentUID,
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--hostname", "sandbox", "--init", "--entrypoint", "",
		"--memory", "2g", "--pids-limit", "512",
		"--network", b.Network(sandbox.NetAgent),
		"--mount", "type=volume,src=" + b.ClaudeVolume() + ",dst=" + claudeDir,
		"--workdir", "/home/agent",
	}
	args = append(args, b.engineFlags()...)
	env := b.baseEnv(sandbox.NetAgent)
	env["CLAUDE_CONFIG_DIR"] = claudeDir
	if terminal {
		args = append(args, "--tty")
		env["TERM"] = "xterm-256color"
		delete(env, "CI")
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--env", k+"="+env[k])
	}
	return append(append(args, b.Image), argv...)
}

// Login runs Claude Code's sign-in in a container on the caller's terminal. The login is kept in the Claude volume. Up must have
// run, because the sign-in goes through the agent proxy.
func (b *Box) Login(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := b.check(); err != nil {
		return err
	}
	name := b.prefix() + "-login-" + token()
	cmd := exec.CommandContext(ctx, b.docker(), b.claudeArgs(name, true, argv...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	err := cmd.Run()
	b.remove(name)
	return err
}

// LoggedIn asks Claude Code whether it has a login (SPEC 10.7).
func (b *Box) LoggedIn(ctx context.Context, command string) (bool, error) {
	if err := b.check(); err != nil {
		return false, err
	}
	name := b.prefix() + "-login-" + token()
	cmd := exec.CommandContext(ctx, b.docker(), b.claudeArgs(name, false, command, "auth", "status", "--json")...)
	var outb, errb capped
	outb.max, errb.max = 64<<10, 4096
	cmd.Stdout, cmd.Stderr = &outb, &errb
	yes, known := loginAnswer(outb.buf.Bytes(), cmd.Run())
	if !known {
		return false, fail("the login could not be checked: %s", lastLine(errb.buf.Bytes()))
	}
	return yes, nil
}

// loginAnswer reads `claude auth status`. Its JSON says whether there is a
// login; without readable JSON the exit status does (0 yes, 1 no).
func loginAnswer(stdout []byte, err error) (loggedIn, known bool) {
	var status struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	if json.Unmarshal(stdout, &status) == nil && status.LoggedIn != nil {
		return *status.LoggedIn, true
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
		return true, true
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		return false, true
	}
	return false, false
}

// ProxyRunning reports whether the proxy of a profile is up.
func (b *Box) ProxyRunning(ctx context.Context, profile string) bool {
	out, err := b.cli(ctx, "inspect", "--format", "{{.State.Running}}", b.ProxyName(profile))
	return err == nil && out == "true"
}
