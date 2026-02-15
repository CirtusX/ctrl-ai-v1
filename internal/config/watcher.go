package config

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

// WatchTargets holds callbacks that fire when specific config files change.
// Used for hot-reload of rules and kill switch state without restarting
// the proxy. The running proxy sets these callbacks at startup.
type WatchTargets struct {
	// OnRulesChange fires when rules.yaml is written or created.
	// Typically triggers engine.Reload() to pick up new rules.
	OnRulesChange func()

	// OnKillSwitchChange fires when killed.yaml is written or created.
	// Typically triggers killSwitch.Reload() to update the in-memory
	// killed agent set. This is what makes `ctrlai kill` take effect
	// instantly — the CLI writes killed.yaml, the watcher fires, and
	// the proxy's kill switch state updates in memory.
	OnKillSwitchChange func()
}

// Watcher monitors the CtrlAI config directory for file changes using
// fsnotify. It watches for modifications to rules.yaml and killed.yaml,
// firing the appropriate callback when a change is detected.
//
// The watcher runs a background goroutine that processes fsnotify events.
// Call Close() to stop the watcher and release resources.
type Watcher struct {
	fsWatcher *fsnotify.Watcher
	done      chan struct{}
}

// NewWatcher creates a file watcher on the given config directory.
// It watches for changes to rules.yaml and killed.yaml.
//
// The watcher immediately starts processing events in a background
// goroutine. Events are debounced naturally by fsnotify — rapid
// successive writes typically produce a single event.
func NewWatcher(dir string, targets WatchTargets) (*Watcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("creating file watcher: %w", err)
	}

	// Watch the entire config directory. fsnotify will send events for
	// any file created, written, renamed, or removed in this directory.
	if err := fw.Add(dir); err != nil {
		fw.Close()
		return nil, fmt.Errorf("watching directory %s: %w", dir, err)
	}

	w := &Watcher{
		fsWatcher: fw,
		done:      make(chan struct{}),
	}

	// Start the event processing goroutine.
	go w.processEvents(targets)

	slog.Info("file watcher started", "dir", dir)
	return w, nil
}

// processEvents reads fsnotify events and dispatches to the appropriate
// callback. Runs in a background goroutine until Close() is called.
func (w *Watcher) processEvents(targets WatchTargets) {
	for {
		select {
		case event, ok := <-w.fsWatcher.Events:
			if !ok {
				return
			}
			// We only care about write and create events — not remove
			// or rename, which would indicate the file was deleted.
			if event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}

			// Match on filename regardless of directory path.
			name := filepath.Base(event.Name)
			switch name {
			case "rules.yaml":
				slog.Info("rules.yaml changed, triggering reload")
				if targets.OnRulesChange != nil {
					targets.OnRulesChange()
				}
			case "killed.yaml":
				slog.Info("killed.yaml changed, triggering reload")
				if targets.OnKillSwitchChange != nil {
					targets.OnKillSwitchChange()
				}
			}

		case err, ok := <-w.fsWatcher.Errors:
			if !ok {
				return
			}
			slog.Error("file watcher error", "error", err)

		case <-w.done:
			return
		}
	}
}

// Close stops the file watcher goroutine and releases the underlying
// fsnotify watcher. Safe to call multiple times.
func (w *Watcher) Close() error {
	// Signal the goroutine to stop.
	select {
	case <-w.done:
		// Already closed.
		return nil
	default:
		close(w.done)
	}
	return w.fsWatcher.Close()
}
