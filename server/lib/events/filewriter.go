package events

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// FileWriter is a per-category JSONL appender. It opens each log file lazily on
// first write (O_APPEND|O_CREATE|O_WRONLY) and serialises all concurrent writes
// with a single mutex.
type FileWriter struct {
	mu    sync.Mutex
	files map[EventCategory]*os.File
	dir   string
}

// NewFileWriter returns a FileWriter that writes to dir.
// No files are opened until the first Write call.
func NewFileWriter(dir string) *FileWriter {
	return &FileWriter{dir: dir, files: make(map[EventCategory]*os.File)}
}

// Write serialises ev to JSON and appends it as a single JSONL line to the
// per-category log file. The mutex guarantees whole-line atomicity across
// concurrent callers.
func (fw *FileWriter) Write(ev BrowserEvent) error {
	cat := ev.Category
	if cat == "" {
		return fmt.Errorf("filewriter: event %q has empty category", ev.Type)
	}

	fw.mu.Lock()
	defer fw.mu.Unlock()

	f, ok := fw.files[cat]
	if !ok {
		path := filepath.Join(fw.dir, string(cat)+".log")
		var err error
		f, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("filewriter: open %s: %w", path, err)
		}
		fw.files[cat] = f
	}

	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("filewriter: marshal: %w", err)
	}

	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("filewriter: write: %w", err)
	}

	return nil
}

// Close closes all open log file descriptors. The first encountered error is
// returned; subsequent files are still closed.
func (fw *FileWriter) Close() error {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	var firstErr error
	for _, f := range fw.files {
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
