package unity

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestAllowed(t *testing.T) {
	for name, want := range map[string]bool{
		".local/share/unity3d/Unity/Unity_lic.ulf":                   true,
		".config/unity3d/Unity/licenses/UnityEntitlementLicense.xml": true,
		".config/unity3d/Editor.log":                                 false,
		".local/share/unity3d/Unity/../../../../etc/passwd":          false,
		"/.local/share/unity3d/Unity/x":                              false,
		".local/share/unity3d/Unity":                                 false,
		".bashrc":                                                    false,
		".local/share/unity3d/Unity/./x":                             false,
	} {
		if got := Allowed(name); got != want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

func TestSaveReplacesTheLicence(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "unity-licence")}
	if s.Has() {
		t.Fatal("a licence before any was saved")
	}
	if err := s.Save(map[string][]byte{LicenceFile: []byte("one")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(map[string][]byte{".config/unity3d/Unity/licenses/a.xml": []byte("two")}); err != nil {
		t.Fatal(err)
	}
	if got := s.Files(); !reflect.DeepEqual(got, []string{".config/unity3d/Unity/licenses/a.xml"}) {
		t.Fatalf("files %v", got)
	}
	if err := s.Save(map[string][]byte{"../escape": []byte("x")}); err == nil {
		t.Fatal("a path outside the licence places was saved")
	}
	if got := s.Files(); len(got) != 1 {
		t.Fatalf("a failed save changed the licence: %v", got)
	}
	if err := s.Save(nil); err == nil {
		t.Fatal("an empty licence was saved")
	}
	if err := s.Remove(); err != nil || s.Has() {
		t.Fatalf("remove: %v", err)
	}
}

func TestImport(t *testing.T) {
	dir := t.TempDir()
	s := Store{Dir: filepath.Join(dir, "unity-licence")}
	good := filepath.Join(dir, "Unity_lic.ulf")
	os.WriteFile(good, []byte(`<?xml version="1.0"?><root><License id="Terms"></License></root>`), 0o644)
	bad := filepath.Join(dir, "notes.txt")
	os.WriteFile(bad, []byte("hello"), 0o644)
	if err := s.Import(bad); err == nil {
		t.Fatal("a file that is no licence was imported")
	}
	if err := s.Import(good); err != nil {
		t.Fatal(err)
	}
	if got := s.Files(); !reflect.DeepEqual(got, []string{LicenceFile}) {
		t.Fatalf("files %v", got)
	}
}
