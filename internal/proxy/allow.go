package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
)

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
