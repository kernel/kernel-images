package events

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// maxLogFileSize is the per-category file size cap. Writes beyond this limit
// are silently dropped to prevent unbounded disk usage on constrained instances.
const maxLogFileSize int64 = 64 << 20 // 64 MB

// trackedFile pairs an os.File with an in-memory size counter to avoid
// a stat syscall on every write.
type trackedFile struct {
	f    *os.File
	size int64
}

// fileWriter is a JSONL appender keyed by filename. It opens each file lazily
// on first write (O_APPEND|O_CREATE|O_WRONLY) and serialises all concurrent
// writes with a single mutex.
type fileWriter struct {
	mu    sync.Mutex
	files map[string]*trackedFile
	dir   string
}

// newFileWriter returns a fileWriter that writes to dir, creating it if needed.
func newFileWriter(dir string) (*fileWriter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("filewriter: create dir %s: %w", dir, err)
	}
	return &fileWriter{dir: dir, files: make(map[string]*trackedFile)}, nil
}

// Write appends data as a single JSONL line to the named file under the
// writer's directory.
func (fw *fileWriter) Write(filename string, data []byte) error {
	if filename == "" {
		return fmt.Errorf("filewriter: empty filename")
	}

	fw.mu.Lock()
	defer fw.mu.Unlock()

	tf, ok := fw.files[filename]
	if !ok {
		path := filepath.Join(fw.dir, filename)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("filewriter: open %s: %w", path, err)
		}
		// Seed size from the file in case we're appending to an existing file.
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return fmt.Errorf("filewriter: stat %s: %w", path, err)
		}
		tf = &trackedFile{f: f, size: info.Size()}
		fw.files[filename] = tf
	}

	if tf.size >= maxLogFileSize {
		return fmt.Errorf("filewriter: %s: size cap reached (%d bytes)", filename, maxLogFileSize)
	}

	line := make([]byte, len(data)+1)
	copy(line, data)
	line[len(data)] = '\n'
	n, err := tf.f.Write(line)
	tf.size += int64(n)
	if err != nil {
		return fmt.Errorf("filewriter: write: %w", err)
	}

	return nil
}

// Close closes all open log file descriptors
func (fw *fileWriter) Close() error {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	var firstErr error
	for _, tf := range fw.files {
		if err := tf.f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
