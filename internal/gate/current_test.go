package gate

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/iandeherdt/Maestro/internal/config"
	"github.com/iandeherdt/Maestro/internal/issue"
	"github.com/iandeherdt/Maestro/internal/workspace"
)

func TestCurrentPicturesAreGivenToTheAgentAndShownWithTheDesign(t *testing.T) {
	l := newLocal(t)
	pic := picture(40, 30)
	g := l.gates(config.Screenshots{Command: `
printf %s '` + base64.StdEncoding.EncodeToString(pic) + `' | base64 -d > "$SCREENSHOT_DIR/1-Title.png"
printf %s '` + base64.StdEncoding.EncodeToString(pic) + `' | base64 -d > "$SCREENSHOT_DIR/2-Level.png"
`})
	pub := &designPub{}
	g.Publisher = pub
	ws := workspace.Workspace{Key: "GH-1-design"}

	// Something the agent left there earlier is gone.
	dir := filepath.Join(filepath.Dir(l.repo), CurrentDir)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "old.png"), []byte("x"), 0o644)

	cur := g.Current(ctx, ws)
	if cur.Note != "" || len(cur.Shots) != 2 || len(cur.Files) != 2 {
		t.Fatalf("%+v", cur)
	}
	if cur.Files[0].Caption != "1-Title" || cur.Files[0].Path != "../"+CurrentDir+"/1.png" || cur.Files[1].Path != "../"+CurrentDir+"/2.png" {
		t.Fatalf("files %+v", cur.Files)
	}
	for _, name := range []string{"1.png", "2.png"} {
		got, err := os.ReadFile(filepath.Join(l.repo, "..", CurrentDir, name))
		if err != nil || !bytes.Equal(got, cur.Shots[0].PNG) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "old.png")); !os.IsNotExist(err) {
		t.Fatalf("an old file is still there: %v", err)
	}

	l.write(map[string]string{"design/GH-1/README.md": "Only words."})
	if _, _, err := g.Design(ctx, issue.Issue{Identifier: "GH-1"}, ws, "design/GH-1"); err != nil {
		t.Fatal(err)
	}
	if len(pub.current) != 2 || pub.current[1].Caption != "2-Level" {
		t.Fatalf("the design got %d current pictures", len(pub.current))
	}
}

func TestNoCurrentPicturesWithoutTheSetting(t *testing.T) {
	l := newLocal(t)
	g := l.gates(config.Screenshots{})
	if cur := g.Current(ctx, workspace.Workspace{Key: "GH-1-design"}); len(cur.Files) != 0 || cur.Note != "" {
		t.Fatalf("%+v", cur)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(l.repo), CurrentDir)); !os.IsNotExist(err) {
		t.Fatalf("the folder was made: %v", err)
	}
}

func TestCurrentPicturesThatFailCarryTheNote(t *testing.T) {
	l := newLocal(t)
	g := l.gates(config.Screenshots{Command: "exit 3"})
	cur := g.Current(ctx, workspace.Workspace{Key: "GH-1-design"})
	if len(cur.Files) != 0 || cur.Note == "" {
		t.Fatalf("%+v", cur)
	}
}
