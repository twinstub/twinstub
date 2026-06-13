package scaffold

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/twinstub/twinstub/internal/config"
	"github.com/twinstub/twinstub/internal/snapshot"
	"github.com/twinstub/twinstub/internal/tmpl"
)

func TestWriteProducesValidProject(t *testing.T) {
	dir := t.TempDir()
	files, err := Write(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 4 {
		t.Fatalf("scaffold wrote %d files", len(files))
	}
	cfg, warns, err := config.Load(filepath.Join(dir, "twinstub.yaml"))
	if err != nil {
		t.Fatalf("generated project does not validate: %v", err)
	}
	if len(warns) > 0 {
		t.Errorf("generated project has warnings: %v", warns)
	}
	if _, err := snapshot.Compile(cfg, tmpl.New(0, false), 1); err != nil {
		t.Fatalf("generated project does not compile: %v", err)
	}
	if len(cfg.Scenarios) == 0 || len(cfg.Endpoints) == 0 {
		t.Error("scaffold must include both endpoints and a scenario")
	}
}

func TestWriteRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "twinstub.yaml"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(dir); err == nil {
		t.Fatal("expected refusal to overwrite existing files")
	}
}
