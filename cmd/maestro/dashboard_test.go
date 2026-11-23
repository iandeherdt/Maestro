package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDashboardURL(t *testing.T) {
	dir := t.TempDir()
	if _, err := dashboardURL(dir); err == nil {
		t.Error("no service file accepted")
	}
	os.WriteFile(filepath.Join(dir, serviceFile), []byte(`{"addr":"127.0.0.1:54321","pid":1}`), 0o600)
	if u, err := dashboardURL(dir); err != nil || u != "http://127.0.0.1:54321/" {
		t.Errorf("%q %v", u, err)
	}
	os.WriteFile(filepath.Join(dir, serviceFile), []byte(`{"addr":"10.0.0.5:80","pid":1}`), 0o600)
	if _, err := dashboardURL(dir); err == nil {
		t.Error("a non-loopback address accepted")
	}
}
