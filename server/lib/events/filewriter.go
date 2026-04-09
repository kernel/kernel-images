package events

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// fileWriter is a per-category JSONL appender. It opens each log file lazily on
// first write (O_APPEND|O_CREATE|O_WRONLY) and serialises all concurrent writes
// with a single mutex
type fileWriter struct {
	mu    sync.Mutex
	files map[EventCategory]*os.File
	dir   string
}

// newFileWriter returns a fileWriter that writes to dir, creating it if needed.
func newFileWriter(dir string) (*fileWriter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("filewriter: create dir %s: %w", dir, err)
	}
	return &fileWriter{dir: dir, files: make(map[EventCategory]*os.File)}, nil
}

// Write appends data as a single JSONL line to the per-category log file.
func (fw *fileWriter) Write(env Envelope, data []byte) error {
	cat := env.Event.Category
	if cat == "" {
		return fmt.Errorf("filewriter: event %q has empty category", env.Event.Type)
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

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("filewriter: write: %w", err)
	}
	if _, err := f.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("filewriter: write newline: %w", err)
	}

	return nil
}

// Close closes all open log file descriptors
func (fw *fileWriter) Close() error {
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
