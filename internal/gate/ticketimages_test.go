package gate

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/ticketimage"
	"github.com/iandeherdt/Maestro/internal/workspace"
)

func TestTicketImagesAreWrittenNextToTheRepository(t *testing.T) {
	l := newLocal(t)
	g := l.gates(config.Screenshots{})
	dir := filepath.Join(filepath.Dir(l.repo), TicketImagesDir)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "3.png"), []byte("from an earlier attempt"), 0o644)

	a, b := picture(5, 5), picture(7, 3)
	paths, ok := g.TicketImages(ctx, workspace.Workspace{Key: "GH-1"}, []ticketimage.Image{{PNG: a}, {PNG: b}})
	if !ok || len(paths) != 2 || paths[0] != "../"+TicketImagesDir+"/1.png" || paths[1] != "../"+TicketImagesDir+"/2.png" {
		t.Fatalf("%v %v", paths, ok)
	}
	for i, want := range [][]byte{a, b} {
		got, err := os.ReadFile(filepath.Join(l.repo, paths[i]))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: %v", paths[i], err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "3.png")); !os.IsNotExist(err) {
		t.Fatalf("an old picture is still there: %v", err)
	}
}
