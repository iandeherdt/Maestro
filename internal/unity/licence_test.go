package unity

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
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

func TestSecretsAreTheLicencesValues(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "unity-licence")}
	sig := "QmFzZTY0U2lnbmF0dXJlVmFsdWVUaGF0SXNMb25nRW5vdWdoVG9TcGxpdEludG9QaWVjZXNPZlRoaXJ0eVR3bw=="
	ulf := `<?xml version="1.0" encoding="UTF-8"?><root><License id="Terms">
<SerialMasked Value="F4-XXXX-XXXX-XXXX-XXXX-1234"/><Binding Key="1" Value="576562626572264761624c65526f7578"/>
<Short Value="abc"/></License><Signature><SignatureValue>` + sig[:40] + "\n" + sig[40:] + `</SignatureValue></Signature></root>`
	if err := s.Save(map[string][]byte{LicenceFile: []byte(ulf)}); err != nil {
		t.Fatal(err)
	}
	got := s.Secrets()
	want := []string{"F4-XXXX-XXXX-XXXX-XXXX-1234", "576562626572264761624c65526f7578", sig, sig[:32]}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("missing %q in %q", w, got)
		}
	}
	if slices.Contains(got, "abc") {
		t.Error("a short value is not distinctive")
	}
}
