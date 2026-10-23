package api

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

const (
	processOutputChunkSize  = 32 * 1024
	processOutputHeaderSize = 13
)

var errProcessOutputLimit = errors.New("process output limit exceeded")
var errProcessOutputConsumed = errors.New("process output already consumed")
var errProcessOutputOffset = errors.New("invalid process output offset")

// ProcessOutputConfig bounds stored output, including record headers. Zero limits
// use the defaults; Dir defaults to the operating system's temporary directory.
type ProcessOutputConfig struct {
	Dir           string
	MaxBytes      int64
	TotalMaxBytes int64
}

type processOutputBudget struct {
	mu     sync.Mutex
	config ProcessOutputConfig
	used   int64
}

func newProcessOutputBudget(config ProcessOutputConfig) (*processOutputBudget, error) {
	if config.MaxBytes == 0 {
		config.MaxBytes = 64 << 20
	}
	if config.TotalMaxBytes == 0 {
		config.TotalMaxBytes = 256 << 20
	}
	if config.MaxBytes <= processOutputHeaderSize || config.TotalMaxBytes < config.MaxBytes {
		return nil, fmt.Errorf("process output limits must exceed %d bytes and total must be at least the per-process limit", processOutputHeaderSize)
	}
	return &processOutputBudget{config: config}, nil
}

func (b *processOutputBudget) reserve(n int64) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	n = min(n, b.config.TotalMaxBytes-b.used)
	b.used += n
	return n
}

func (b *processOutputBudget) release(n int64) {
	b.mu.Lock()
	b.used -= n
	b.mu.Unlock()
}

type processOutput struct {
	mu             sync.Mutex
	file           *os.File
	budget         *processOutputBudget
	size           int64
	allocated      int64
	complete       bool
	err            error
	changed        chan struct{}
	closed         chan struct{}
	closeOnce      sync.Once
	consumedOffset int64
	exitConsumed   bool
}

func (b *processOutputBudget) open() (*processOutput, error) {
	f, err := os.CreateTemp(b.config.Dir, "kernel-process-output-*")
	if err != nil {
		return nil, err
	}
	// Keep only the open descriptor so a server crash cannot orphan output files.
	if err := os.Remove(f.Name()); err != nil {
		f.Close()
		return nil, err
	}
	return &processOutput{file: f, budget: b, changed: make(chan struct{}), closed: make(chan struct{})}, nil
}

func (o *processOutput) append(stream byte, data []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return o.err
	}
	select {
	case <-o.closed:
		return os.ErrClosed
	default:
	}
	want := int64(processOutputHeaderSize + len(data))
	available := min(want, o.budget.config.MaxBytes-o.allocated)
	reserved := o.budget.reserve(available)
	if reserved <= processOutputHeaderSize {
		o.budget.release(reserved)
		o.err = errProcessOutputLimit
		return o.err
	}
	o.allocated += reserved
	n := int(reserved) - processOutputHeaderSize
	record := make([]byte, int(reserved))
	binary.BigEndian.PutUint64(record, uint64(o.size))
	binary.BigEndian.PutUint32(record[8:], uint32(n))
	record[12] = stream
	copy(record[processOutputHeaderSize:], data[:n])
	if _, err := o.file.WriteAt(record, o.size); err != nil {
		o.err = err
		return err
	}
	o.size += reserved
	close(o.changed)
	o.changed = make(chan struct{})
	if reserved < want {
		o.err = errProcessOutputLimit
	}
	return o.err
}

func (o *processOutput) finish() {
	o.mu.Lock()
	o.complete = true
	close(o.changed)
	o.mu.Unlock()
}

func (o *processOutput) close() {
	o.closeOnce.Do(func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		close(o.closed)
		if o.file != nil {
			o.file.Close()
			o.budget.release(o.allocated)
		}
	})
}

func (o *processOutput) header(offset int64) (byte, int, error) {
	var header [processOutputHeaderSize]byte
	if _, err := o.file.ReadAt(header[:], offset); err != nil {
		return 0, 0, err
	}
	n := int(binary.BigEndian.Uint32(header[8:]))
	if binary.BigEndian.Uint64(header[:]) != uint64(offset) || n <= 0 || n > processOutputChunkSize || header[12] > 1 {
		return 0, 0, errProcessOutputOffset
	}
	return header[12], n, nil
}

func (o *processOutput) validateOffset(offset int64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if offset < 0 || offset > o.size {
		return errProcessOutputOffset
	}
	if offset == o.size {
		return nil
	}
	_, n, err := o.header(offset)
	if err != nil || offset+processOutputHeaderSize+int64(n) > o.size {
		return errProcessOutputOffset
	}
	return nil
}

func (o *processOutput) next(ctx context.Context, offset int64) (stream byte, data []byte, next int64, err error) {
	for {
		o.mu.Lock()
		size, complete, changed := o.size, o.complete, o.changed
		o.mu.Unlock()
		if offset < size {
			stream, n, err := o.header(offset)
			if err != nil {
				return 0, nil, offset, err
			}
			next := offset + processOutputHeaderSize + int64(n)
			if next > size {
				return 0, nil, offset, errProcessOutputOffset
			}
			data := make([]byte, n)
			_, err = o.file.ReadAt(data, offset+processOutputHeaderSize)
			return stream, data, next, err
		}
		if complete {
			return 0, nil, offset, io.EOF
		}
		select {
		case <-ctx.Done():
			return 0, nil, offset, ctx.Err()
		case <-o.closed:
			return 0, nil, offset, os.ErrClosed
		case <-changed:
		}
	}
}

// Cursorless streams share consumption, as with the original output queue.
func (o *processOutput) nextUnread(ctx context.Context) (byte, []byte, int64, error) {
	for {
		o.mu.Lock()
		offset, consumed := o.consumedOffset, o.exitConsumed
		o.mu.Unlock()
		if consumed {
			return 0, nil, offset, errProcessOutputConsumed
		}
		stream, data, next, err := o.next(ctx, offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, nil, offset, err
		}
		o.mu.Lock()
		if o.consumedOffset != offset || o.exitConsumed {
			o.mu.Unlock()
			continue
		}
		o.consumedOffset = next
		o.exitConsumed = errors.Is(err, io.EOF)
		o.mu.Unlock()
		return stream, data, next, err
	}
}
