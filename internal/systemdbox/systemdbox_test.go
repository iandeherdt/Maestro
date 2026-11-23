package systemdbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iandeherdt/Maestro/internal/sandbox"
)

// fakeSudo writes a script that stands for sudo and the launcher. It saves
// its arguments and standard input, prints what the test asks, and exits
// with the status the test asks.
func fakeSudo(t *testing.T, body string) (*Box, string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + dir + "/args\ncat > " + dir + "/stdin\n" + body + "\n"
	path := filepath.Join(dir, "sudo")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Box{Worker: "linux", Sudo: path, LauncherPath: "/usr/local/libexec/maestro-launch"}, dir
}

func TestRunSendsTheRequestLineThenStdin(t *testing.T) {
	b, dir := fakeSudo(t, "echo out; echo err >&2; exit 3")
	req := sandbox.Request{Step: sandbox.StepHook, WorkspaceKey: "GH-7", Argv: []string{"make", "test"},
		NetworkProfile: sandbox.NetHooks, Env: map[string]string{"MAESTRO_TURN": "1"}}
	res, err := b.Run(context.Background(), req, []byte("input"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitStatus != 3 || string(res.Stdout) != "out\n" || string(res.Stderr) != "err\n" {
		t.Errorf("%+v", res)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if string(args) != "-n\n--\n/usr/local/libexec/maestro-launch\nlinux\n" {
		t.Errorf("args %q", args)
	}
	in, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	line, rest, ok := bytes.Cut(in, []byte("\n"))
	if !ok || string(rest) != "input" {
		t.Fatalf("stdin %q", in)
	}
	var got sandbox.Request
	if err := json.Unmarshal(line, &got); err != nil || got.WorkspaceKey != "GH-7" || got.Argv[1] != "test" ||
		got.Env["MAESTRO_TURN"] != "1" || got.NetworkProfile != "hooks" {
		t.Errorf("request %s %v", line, err)
	}
}

func TestStreamPassesStdoutOn(t *testing.T) {
	b, _ := fakeSudo(t, "echo progress")
	var out bytes.Buffer
	res, err := b.Stream(context.Background(), sandbox.Request{Step: sandbox.StepAgent, WorkspaceKey: "GH-7",
		Argv: []string{"claude"}, NetworkProfile: sandbox.NetAgent}, nil, &out)
	if err != nil || res.ExitStatus != 0 || out.String() != "progress\n" || len(res.Stdout) != 0 {
		t.Errorf("%+v %v %q", res, err, out.String())
	}
}

func TestARefusalIsALaunchFailure(t *testing.T) {
	b, _ := fakeSudo(t, "echo 'maestro-launch: invalid branch name' >&2; exit 125")
	_, err := b.Run(context.Background(), sandbox.Request{Step: sandbox.StepPopulate, WorkspaceKey: "GH-7",
		Populate: &sandbox.Populate{Op: sandbox.OpProbe}}, nil)
	var se *sandbox.Error
	if !errors.As(err, &se) || !strings.Contains(err.Error(), "invalid branch name") {
		t.Errorf("%v", err)
	}
}

func TestBadRequestsNeverReachSudo(t *testing.T) {
	b, dir := fakeSudo(t, "")
	for _, req := range []sandbox.Request{
		{Step: sandbox.StepHook, WorkspaceKey: "../etc", Argv: []string{"x"}},
		{Step: sandbox.StepHook, WorkspaceKey: "GH-7", Argv: []string{"x"}, Env: map[string]string{"GITHUB_TOKEN": "x"}},
	} {
		if _, err := b.Run(context.Background(), req, nil); err == nil {
			t.Errorf("%+v accepted", req)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "args")); err == nil {
		t.Error("sudo ran")
	}
	b.Worker = "Bad Name"
	if _, err := b.Run(context.Background(), sandbox.Request{Step: sandbox.StepHook, WorkspaceKey: "GH-7", Argv: []string{"x"}}, nil); err == nil {
		t.Error("a bad worker name was accepted")
	}
}

func TestCancelAsksTheLauncherToStop(t *testing.T) {
	// The launcher stops its unit on SIGTERM; the fake records that it got it.
	b, dir := fakeSudo(t, "trap 'echo stopped > "+"$(dirname $0)"+"/term; exit 143' TERM; while :; do sleep 0.05; done")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := b.Run(ctx, sandbox.Request{Step: sandbox.StepHook, WorkspaceKey: "GH-7", Argv: []string{"x"}}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("%v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "term")); string(got) != "stopped\n" {
		t.Error("the launcher was not sent SIGTERM")
	}
}
