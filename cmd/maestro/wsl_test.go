package main

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/iandeherdt/Maestro/internal/setup"
	"github.com/iandeherdt/Maestro/internal/wsl"
)

func fakeWSL(t *testing.T, ran *[][]string, fail bool) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("WSL is Linux")
	}
	oldHost, oldRun := wslHost, runWindows
	t.Cleanup(func() { wslHost, runWindows = oldHost, oldRun })
	wslHost = wsl.Host{
		ReadFile: func(p string) ([]byte, error) {
			switch p {
			case "/proc/sys/kernel/osrelease":
				return []byte("5.15.153.1-microsoft-standard-WSL2\n"), nil
			case "/proc/1/comm":
				return []byte("systemd\n"), nil
			}
			return nil, errors.New("no such file")
		},
		Getenv: func(string) string { return "Ubuntu" },
	}
	runWindows = func(_ context.Context, argv []string) ([]byte, error) {
		*ran = append(*ran, argv)
		if fail {
			return []byte("ERROR: Access is denied.\r\n"), errors.New("exit status 1")
		}
		return []byte("SUCCESS"), nil
	}
}

func TestWSLAfterInstallCreatesTheTask(t *testing.T) {
	var ran [][]string
	fakeWSL(t, &ran, false)
	var out bytes.Buffer
	wslAfterInstall(context.Background(), setup.Terminal(strings.NewReader("y\n"), &out), "nuc")
	text := out.String()
	for _, w := range []string{"sudo loginctl enable-linger $USER", `/TN "Maestro nuc"`, "wsl.exe -d Ubuntu --exec /bin/sleep infinity", "The task is created"} {
		if !strings.Contains(text, w) {
			t.Errorf("output lacks %q:\n%s", w, text)
		}
	}
	if len(ran) != 1 || ran[0][0] != "schtasks.exe" || ran[0][1] != "/Create" {
		t.Errorf("ran %q", ran)
	}
}

func TestWSLAfterInstallSaysWhatToPasteWhenItFails(t *testing.T) {
	var ran [][]string
	fakeWSL(t, &ran, true)
	var out bytes.Buffer
	wslAfterInstall(context.Background(), setup.Terminal(strings.NewReader("y\n"), &out), "nuc")
	if !strings.Contains(out.String(), "Access is denied") || !strings.Contains(out.String(), "Paste the line above") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestWSLAfterInstallNotOutsideWSL(t *testing.T) {
	old := wslHost
	t.Cleanup(func() { wslHost = old })
	wslHost = wsl.Host{ReadFile: func(string) ([]byte, error) { return []byte("6.8.0-45-generic\n"), nil }}
	var out bytes.Buffer
	wslAfterInstall(context.Background(), setup.Terminal(strings.NewReader(""), &out), "nuc")
	if out.Len() != 0 {
		t.Errorf("said %q", out.String())
	}
}

func TestWSLAfterUninstallRemovesTheTask(t *testing.T) {
	var ran [][]string
	fakeWSL(t, &ran, false)
	var said []string
	wslAfterUninstall(context.Background(), func(s string) { said = append(said, s) }, "nuc")
	if len(ran) != 1 || ran[0][1] != "/Delete" || len(said) != 1 || !strings.Contains(said[0], "is removed") {
		t.Errorf("ran %q, said %q", ran, said)
	}
}
