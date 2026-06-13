package config

import (
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch observes the root config file and the directories of include globs
// and calls onChange (debounced) on any YAML change. The callback decides
// whether the new config is valid; this function never stops the server.
func Watch(rootConfig string, includes []string, onChange func(), stop <-chan struct{}) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	abs, err := filepath.Abs(rootConfig)
	if err != nil {
		return err
	}
	dirs := map[string]bool{filepath.Dir(abs): true}
	for _, pattern := range includes {
		if !filepath.IsAbs(pattern) {
			pattern = filepath.Join(filepath.Dir(abs), pattern)
		}
		dirs[filepath.Dir(pattern)] = true
	}
	for d := range dirs {
		// Watching directories instead of files survives editors that
		// replace files on save (rename + create).
		if err := w.Add(d); err != nil {
			_ = w.Close()
			return err
		}
	}

	go func() {
		defer func() { _ = w.Close() }()
		var timer *time.Timer
		debounced := make(chan struct{}, 1)
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
					continue
				}
				ext := filepath.Ext(ev.Name)
				if ext != ".yaml" && ext != ".yml" {
					continue
				}
				if timer != nil {
					timer.Stop()
				}
				timer = time.AfterFunc(300*time.Millisecond, func() {
					select {
					case debounced <- struct{}{}:
					default:
					}
				})
			case <-debounced:
				onChange()
			case _, ok := <-w.Errors:
				if !ok {
					return
				}
			case <-stop:
				return
			}
		}
	}()
	return nil
}
