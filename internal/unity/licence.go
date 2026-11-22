// Package unity keeps the activated Unity licence of this machine.
//
// The licence is what Unity's own activation leaves in the home directory:
// a licence file under .local/share/unity3d and, for newer licences, the
// entitlements under .config/unity3d/Unity/licenses. The service mounts
// the directory read-only into the steps that run the repository's own
// scripts (hooks and verify), never into the agent's step. The account's
// password is never stored.
package unity

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Places are the directories, relative to a home directory, that hold a
// licence. Nothing else is kept.
var Places = []string{".local/share/unity3d/Unity", ".config/unity3d/Unity/licenses"}

// LicenceFile is where a licence file that is imported by hand goes.
const LicenceFile = ".local/share/unity3d/Unity/Unity_lic.ulf"

// maxFile bounds one file of a licence. Real ones are a few kilobytes.
const maxFile = 1 << 20

// Store is the licence directory of one installation.
type Store struct{ Dir string }

// Allowed reports whether a relative path may be part of a licence.
func Allowed(name string) bool {
	clean := path.Clean(name)
	if clean != name || strings.HasPrefix(clean, "/") || strings.Contains(clean, "..") {
		return false
	}
	for _, p := range Places {
		if strings.HasPrefix(clean, p+"/") {
			return true
		}
	}
	return false
}

// Files lists the files of the licence, relative to Dir. None means there
// is no licence.
func (s Store) Files() []string {
	var out []string
	filepath.WalkDir(s.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(s.Dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// Has reports whether there is a licence.
func (s Store) Has() bool { return len(s.Files()) > 0 }

// Save replaces the licence with files, relative path to content. It is
// written next to the old one and then swapped in, so a failure leaves the
// old licence as it was.
func (s Store) Save(files map[string][]byte) error {
	if len(files) == 0 {
		return errors.New("the activation left no licence behind")
	}
	parent := filepath.Dir(s.Dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".unity-licence-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	// The sandbox user reads the files through a read-only mount.
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	for name, raw := range files {
		if !Allowed(name) {
			return fmt.Errorf("%q is not a place a Unity licence is kept", name)
		}
		if len(raw) > maxFile {
			return fmt.Errorf("%s is larger than a licence", name)
		}
		target := filepath.Join(tmp, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, raw, 0o644); err != nil {
			return err
		}
	}
	old := s.Dir + ".old"
	os.RemoveAll(old)
	if err := os.Rename(s.Dir, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(tmp, s.Dir); err != nil {
		os.Rename(old, s.Dir)
		return err
	}
	return os.RemoveAll(old)
}

// Import makes a licence of a licence file activated elsewhere.
func (s Store) Import(file string) error {
	info, err := os.Stat(file)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxFile {
		return fmt.Errorf("%s is not a licence file", file)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if !strings.Contains(string(raw), "<License") {
		return fmt.Errorf("%s does not look like a Unity licence file (Unity_lic.ulf)", file)
	}
	return s.Save(map[string][]byte{LicenceFile: raw})
}

// Remove deletes the licence.
func (s Store) Remove() error { return os.RemoveAll(s.Dir) }
