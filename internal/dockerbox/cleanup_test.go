package dockerbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeDocker is a docker client that answers from a script and records
// its arguments.
func fakeDocker(t *testing.T, script string) (*Box, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "docker")
	body := "#!/bin/sh\necho \"$@\" >> " + log + "\n" + script + "\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Box{Docker: bin, Worker: "nuc"}, log
}

func TestWorkspaceKeys(t *testing.T) {
	b, log := fakeDocker(t, `printf 'maestro-nuc-ws-GH-1\nmaestro-nuc-claude\nmaestro-nuc-ws-ENG-12\nmaestro-nuc-ws-\n'`)
	keys, err := b.WorkspaceKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(keys, []string{"GH-1", "ENG-12"}) {
		t.Fatalf("keys %v", keys)
	}
	args, _ := os.ReadFile(log)
	if !strings.Contains(string(args), "label=maestro.worker=nuc") {
		t.Fatalf("not filtered by worker: %s", args)
	}
}

func TestStaleImages(t *testing.T) {
	b, log := fakeDocker(t, `case "$1 $2" in
"image ls") printf 'sha256:aaa\t1.2GB\nsha256:aaa\t1.2GB\nsha256:bbb\t900MB\n' ;;
"image rm") [ "$3" = sha256:bbb ] && { echo 'conflict: image is being used by running container' >&2; exit 1; } ; exit 0 ;;
esac`)
	ctx := context.Background()
	imgs, err := b.StaleImages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 2 || imgs[0] != (StaleImage{"sha256:aaa", "1.2GB"}) {
		t.Fatalf("images %+v", imgs)
	}
	args, _ := os.ReadFile(log)
	if !strings.Contains(string(args), "dangling=true") || !strings.Contains(string(args), "label=maestro.conventions") {
		t.Fatalf("not only stale sandbox images: %s", args)
	}
	if err := b.RemoveImage(ctx, "sha256:aaa"); err != nil {
		t.Fatal(err)
	}
	if err := b.RemoveImage(ctx, "sha256:bbb"); !errors.Is(err, ErrImageInUse) {
		t.Fatalf("in use: %v", err)
	}
}
