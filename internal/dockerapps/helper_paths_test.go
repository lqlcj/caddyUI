package dockerapps

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestValidateRefDoesNotScanContainerDataSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks may require extra Windows privileges")
	}
	root := t.TempDir()
	appDir := filepath.Join(root, "demo")
	if err := os.MkdirAll(filepath.Join(appDir, "data"), 0o750); err != nil {
		t.Fatal(err)
	}
	composeFile := filepath.Join(appDir, "compose.yaml")
	if err := os.WriteFile(composeFile, []byte("services:\n  web:\n    image: nginx\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target-created-by-container", filepath.Join(appDir, "data", "current")); err != nil {
		t.Fatal(err)
	}
	app := &App{
		Version: metadataV1, Name: "demo", DisplayName: "Demo",
		ComposeRel: "compose.yaml", EnvRel: ".env",
	}
	if err := writeMetadata(appDir, app); err != nil {
		t.Fatal(err)
	}
	h := NewHelperServer(filepath.Join(root, "helper.sock"), root, "")
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	h.AppsRoot = rootAbs
	_, err = h.validateRef(HelperAppRef{
		Name: "demo", AppDir: appDir, ProjectDir: appDir, ComposeFile: composeFile,
	})
	if err != nil {
		t.Fatal(err)
	}
}
