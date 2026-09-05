package agenttransport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// Store must durably append an event before returning success. One runtime owns
// a store; replay and appends are serialized by the runtime.
type Store interface {
	Load() ([]Event, error)
	Append(Event) error
	Close() error
}

type MemoryStore struct{ events []Event }

func (s *MemoryStore) Load() ([]Event, error) { return append([]Event(nil), s.events...), nil }
func (s *MemoryStore) Append(e Event) error   { s.events = append(s.events, e); return nil }
func (s *MemoryStore) Close() error           { return nil }

// FileStore is an append-only journal for the experiment. A file lock excludes
// concurrent owners. A failed write poisons the handle until it is reopened.
type FileStore struct {
	file     *os.File
	failed   error
	once     sync.Once
	closeErr error
}

func OpenFileStore(path string) (*FileStore, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("journal already owned: %w", err)
	}
	s := &FileStore{file: file}
	if _, err = s.Load(); err != nil {
		s.Close()
		return nil, err
	}
	// Persist file creation before accepting commands.
	dir, err := os.OpenFile(filepath.Dir(path), os.O_RDONLY, 0)
	if err != nil {
		s.Close()
		return nil, err
	}
	err = dir.Sync()
	dir.Close()
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *FileStore) Load() ([]Event, error) {
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(s.file)
	if err != nil {
		return nil, err
	}
	end := bytes.LastIndexByte(data, '\n') + 1
	// An incomplete final record could not have been acknowledged: append writes
	// a newline before syncing. Complete but corrupt records fail closed.
	if end != len(data) {
		if err := s.file.Truncate(int64(end)); err != nil {
			return nil, err
		}
		if err := s.file.Sync(); err != nil {
			return nil, err
		}
	}
	events := make([]Event, 0)
	for _, line := range bytes.Split(data[:end], []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, fmt.Errorf("invalid journal record: %w", err)
		}
		if event.Sequence != len(events)+1 {
			return nil, errors.New("non-contiguous journal sequence")
		}
		events = append(events, event)
	}
	_, err = s.file.Seek(0, io.SeekEnd)
	return events, err
}

func (s *FileStore) Append(e Event) error {
	if s.failed != nil {
		return s.failed
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	n, err := s.file.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = s.file.Sync()
	}
	s.failed = err
	return err
}
func (s *FileStore) Close() error {
	s.once.Do(func() { s.closeErr = s.file.Close() })
	return s.closeErr
}
