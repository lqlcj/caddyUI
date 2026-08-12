package dockerapps

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrepareAndSavePublicGitHubRepository(t *testing.T) {
	if os.Getenv("CADDYUI_INTEGRATION") == "" {
		t.Skip("set CADDYUI_INTEGRATION=1 to run network integration tests")
	}
	m := New(t.TempDir(), filepath.Join(t.TempDir(), "missing.sock"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	draft, err := m.Prepare(ctx, "https://github.com/louislam/dockge")
	if err != nil {
		t.Fatal(err)
	}
	if draft.Source.ComposePath != "compose.yaml" || draft.Name != "dockge" {
		t.Fatalf("unexpected draft: %#v", draft)
	}
	app, err := m.SaveDraft(ctx, *draft)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(app.Dir, "compose.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(app.Dir, "backend")); err != nil {
		t.Fatal("full repository was not imported:", err)
	}
}
