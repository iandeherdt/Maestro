package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tarball(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	tw.Write(body)
	tw.Close()
	zw.Close()
	return b.Bytes()
}

func releaseServer(t *testing.T, archive []byte, sum string) *httptest.Server {
	t.Helper()
	if sum == "" {
		s := sha256.Sum256(archive)
		sum = hex.EncodeToString(s[:])
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/v0.5.19", http.StatusFound)
	})
	mux.HandleFunc("/releases/download/v0.5.19/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("abc  maestro_linux_amd64.tar.gz\n" + sum + "  maestro_darwin_arm64.tar.gz\n"))
	})
	mux.HandleFunc("/releases/download/v0.5.19/maestro_darwin_arm64.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestUpdateDownloadsTheCheckedBinary(t *testing.T) {
	srv := releaseServer(t, tarball(t, "maestro", []byte("new binary")), "")
	ctx := context.Background()
	tag, err := latestTag(ctx, srv.Client(), srv.URL+"/releases")
	if err != nil || tag != "v0.5.19" {
		t.Fatalf("latest = %q, %v", tag, err)
	}
	bin, err := download(ctx, srv.Client(), srv.URL+"/releases/download/"+tag, "darwin", "arm64")
	if err != nil || string(bin) != "new binary" {
		t.Fatalf("download = %q, %v", bin, err)
	}
	path := filepath.Join(t.TempDir(), "maestro")
	os.WriteFile(path, []byte("old"), 0o755)
	if err := replaceBinary(path, bin); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new binary" {
		t.Errorf("the binary holds %q", got)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", st.Mode())
	}
}

func TestUpdateRefusesABadChecksum(t *testing.T) {
	srv := releaseServer(t, tarball(t, "maestro", []byte("tampered")), strings.Repeat("0", 64))
	_, err := download(context.Background(), srv.Client(), srv.URL+"/releases/download/v0.5.19", "darwin", "arm64")
	if err == nil || !strings.Contains(err.Error(), "checksum does not match") {
		t.Fatalf("err = %v", err)
	}
}

func TestUpdateWantsTheBinaryInTheArchive(t *testing.T) {
	srv := releaseServer(t, tarball(t, "../maestro", []byte("x")), "")
	_, err := download(context.Background(), srv.Client(), srv.URL+"/releases/download/v0.5.19", "darwin", "arm64")
	if err == nil || !strings.Contains(err.Error(), "no maestro binary") {
		t.Fatalf("err = %v", err)
	}
}

func TestUpdateRejectsAStrangeLatestTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/0.5.18", http.StatusFound)
	}))
	defer srv.Close()
	if _, err := latestTag(context.Background(), srv.Client(), srv.URL+"/releases"); err == nil {
		t.Fatal("a tag without v was taken")
	}
}

func TestUpdateBringsTheLauncherOnLinux(t *testing.T) {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for name, body := range map[string]string{"maestro": "new binary", "maestro-launch": "new launcher"} {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	zw.Close()
	srv := releaseServer(t, b.Bytes(), "")
	files, err := downloadFiles(context.Background(), srv.Client(), srv.URL+"/releases/download/v0.5.19", "darwin", "arm64")
	if err != nil || string(files["maestro"]) != "new binary" || string(files["maestro-launch"]) != "new launcher" {
		t.Fatalf("%v %v", files, err)
	}
	dir := t.TempDir()
	if err := replaceBinary(filepath.Join(dir, "maestro-launch"), files["maestro-launch"]); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "maestro-launch")); string(got) != "new launcher" {
		t.Errorf("%q", got)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}
