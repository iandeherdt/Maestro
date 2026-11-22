package dockerbox

import (
	"archive/tar"
	"bytes"
	"context"
	"strings"
	"testing"
)

func tarOf(t *testing.T, entries map[string]string, links ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for name, body := range entries {
		w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		w.Write([]byte(body))
	}
	for _, l := range links {
		w.WriteHeader(&tar.Header{Name: l, Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink})
	}
	w.Close()
	return buf.Bytes()
}

func TestReadLicenceKeepsOnlyLicenceFiles(t *testing.T) {
	raw := tarOf(t, map[string]string{
		".local/share/unity3d/Unity/Unity_lic.ulf":                     "ulf",
		"./.config/unity3d/Unity/licenses/UnityEntitlementLicense.xml": "xml",
		".config/unity3d/Editor.log":                                   "log",
		".bashrc":                                                      "rc",
	}, ".local/share/unity3d/Unity/link.ulf")
	files, err := readLicence(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || string(files[".local/share/unity3d/Unity/Unity_lic.ulf"]) != "ulf" ||
		string(files[".config/unity3d/Unity/licenses/UnityEntitlementLicense.xml"]) != "xml" {
		t.Fatalf("files %v", files)
	}
	if _, err := readLicence(tarOf(t, map[string]string{".bashrc": "rc"})); err == nil {
		t.Fatal("a stream without a licence was accepted")
	}
}

func TestActivationRefusesAccountsThatAreNotOneLine(t *testing.T) {
	b := &Box{Worker: "w", Image: "i", MirrorDir: "/m", HandoffDir: "/h"}
	for _, a := range []Account{
		{Email: "a@example.com"},
		{Email: "a@example.com", Password: "p\nwhoami"},
		{Email: "", Password: "p"},
	} {
		if _, err := b.ActivateUnity(context.Background(), a, &bytes.Buffer{}); err == nil {
			t.Errorf("%v: accepted", a)
		}
	}
	if s := (Account{Email: "a@example.com", Password: "secret"}).String(); strings.Contains(s, "secret") {
		t.Errorf("the password is printed: %s", s)
	}
}

func TestSignInScriptAsksForNothingAndWritesOnlyTheTar(t *testing.T) {
	// Standard output carries the licence, so every line for the person
	// goes to standard error, and the tools read no input.
	for _, line := range strings.Split(signInScript, "\n") {
		if strings.HasPrefix(line, "unity ") && !strings.Contains(line, "</dev/null >&2") {
			t.Errorf("%q may read input or write to the licence stream", line)
		}
		if strings.HasPrefix(line, "echo ") && !strings.HasSuffix(line, ">&2") {
			t.Errorf("%q writes to the licence stream", line)
		}
	}
	if !strings.Contains(signInScript, "{{places}}") {
		t.Error("the script does not take the licence places")
	}
}
