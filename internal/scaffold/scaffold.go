// Package scaffold writes the example project produced by twinstub init.
package scaffold

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

//go:embed all:templates
var templates embed.FS

// Write materializes the scaffold under dir. Existing files are never
// overwritten; that is an error to avoid clobbering user work.
func Write(dir string) ([]string, error) {
	var files []string
	err := fs.WalkDir(templates, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel("templates", path)
		dest := filepath.Join(dir, rel)
		if _, err := os.Stat(dest); err == nil {
			return fmt.Errorf("%s already exists, refusing to overwrite", dest)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		data, err := templates.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			return err
		}
		files = append(files, dest)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}
