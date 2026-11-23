package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProxyListensOnAUnixSocket(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0o700)
	path := filepath.Join(dir, "agent.sock")
	ln, err := proxyListen("unix:" + path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o666 {
		t.Errorf("socket %v %v", fi, err)
	}
	ln.Close()

	// A socket left behind is replaced; anything else is not touched.
	os.WriteFile(path, []byte("x"), 0o600)
	if _, err := proxyListen("unix:" + path); err == nil {
		t.Error("a regular file was replaced")
	}
	if _, err := proxyListen("unix:relative.sock"); err == nil {
		t.Error("a relative path was accepted")
	}
	open := t.TempDir()
	os.Chmod(open, 0o755)
	if _, err := proxyListen("unix:" + filepath.Join(open, "a.sock")); err == nil {
		t.Error("a socket in a folder other users can enter")
	}
}
