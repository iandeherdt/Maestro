package proxy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestListenUnixRefusesALiveSocket(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0o700)
	path := filepath.Join(dir, "agent.sock")
	ln, err := ListenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if !Answers(path) {
		t.Fatal("the socket does not answer")
	}
	if _, err := ListenUnix(path); !errors.Is(err, ErrInUse) {
		t.Errorf("a live socket was taken over: %v", err)
	}
}
