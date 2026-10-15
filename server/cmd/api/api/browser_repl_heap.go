package api

import (
	"bytes"
	"io"
	"log/slog"
	"sync"
)

// V8 prints one of these when a process hits its heap limit, then aborts
// before the process can report anything itself.
var heapLimitMarkers = [][]byte{
	[]byte("Reached heap limit"),
	[]byte("JavaScript heap out of memory"),
}

// maxHeapLimitWatcherLine bounds the partial line kept between writes.
const maxHeapLimitWatcherLine = 4096

// heapLimitWatcher forwards a child's output unchanged and logs an error the
// first time the output shows the child hit its V8 heap limit.
type heapLimitWatcher struct {
	out      io.Writer
	log      *slog.Logger
	executor string
	limitMB  string

	mu       sync.Mutex
	line     []byte
	reported bool
}

func newHeapLimitWatcher(out io.Writer, log *slog.Logger, executor, limitMB string) *heapLimitWatcher {
	return &heapLimitWatcher{out: out, log: log, executor: executor, limitMB: limitMB}
}

func (w *heapLimitWatcher) Write(p []byte) (int, error) {
	n, err := w.out.Write(p)

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.reported {
		return n, err
	}
	w.line = append(w.line, p...)
	for {
		i := bytes.IndexByte(w.line, '\n')
		if i < 0 {
			break
		}
		if w.check(w.line[:i]) {
			return n, err
		}
		w.line = w.line[i+1:]
	}
	if len(w.line) > maxHeapLimitWatcherLine {
		w.line = w.line[len(w.line)-maxHeapLimitWatcherLine:]
	}
	// A marker can arrive without a trailing newline when the process aborts.
	w.check(w.line)
	return n, err
}

// check logs and reports true when line holds a heap-limit marker. The caller
// must hold mu.
func (w *heapLimitWatcher) check(line []byte) bool {
	for _, marker := range heapLimitMarkers {
		if bytes.Contains(line, marker) {
			w.reported = true
			w.line = nil
			w.log.Error("browser REPL executor hit heap limit", "executor", w.executor, "limit_mb", w.limitMB)
			return true
		}
	}
	return false
}
