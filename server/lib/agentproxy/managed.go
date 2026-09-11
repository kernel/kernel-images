package agentproxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Preparer materializes an immutable revision without changing the active one.
// It must not write shared session data or inherit provider credentials.
type Preparer interface {
	Prepare(context.Context, string, json.RawMessage) (Harness, error)
}

type Configuration struct {
	Revision          string          `json:"revision"`
	Status            string          `json:"status"`
	Desired           json.RawMessage `json:"desired"`
	EffectiveRevision string          `json:"effectiveRevision"`
	Effective         json.RawMessage `json:"effective"`
	Error             string          `json:"error,omitempty"`
}

type preparedRevision struct {
	Configuration json.RawMessage `json:"configuration"`
	Launch        Harness         `json:"launch"`
}

type configurationManager struct {
	mu        sync.RWMutex
	preparing sync.Mutex
	root      string
	preparer  Preparer
	state     Configuration
	launch    *Harness
}

var errConfigurationConflict = errors.New("configuration changed or preparation already in progress")

func newConfigurationManager(root string, preparer Preparer) (*configurationManager, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("configuration directory must be absolute")
	}
	if err := os.MkdirAll(filepath.Join(root, "revisions"), 0700); err != nil {
		return nil, err
	}
	m := &configurationManager{root: root, preparer: preparer, state: Configuration{Revision: "0", Status: "unconfigured", Desired: json.RawMessage("null"), Effective: json.RawMessage("null")}}
	data, err := os.ReadFile(filepath.Join(root, "desired.json"))
	if err == nil {
		if err = json.Unmarshal(data, &m.state); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	data, err = os.ReadFile(filepath.Join(root, "current", "prepared.json"))
	if err == nil {
		var revision preparedRevision
		if err = json.Unmarshal(data, &revision); err != nil {
			return nil, err
		}
		target, err := os.Readlink(filepath.Join(root, "current"))
		if err != nil {
			return nil, err
		}
		m.state.EffectiveRevision = filepath.Base(target)
		m.state.Effective = revision.Configuration
		m.launch = &revision.Launch
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if m.state.Revision == m.state.EffectiveRevision {
		m.state.Status = "ready"
		m.state.Error = ""
	} else if m.state.Status == "preparing" {
		m.state.Status = "failed"
		m.state.Error = "preparation interrupted; last ready revision retained"
	}
	return m, nil
}

func (m *configurationManager) snapshot() Configuration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := m.state
	s.Desired = append(json.RawMessage(nil), s.Desired...)
	s.Effective = append(json.RawMessage(nil), s.Effective...)
	return s
}

func (m *configurationManager) preparedLaunch() (Harness, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.launch == nil {
		return Harness{}, false
	}
	return *m.launch, true
}

func (m *configurationManager) apply(ctx context.Context, expected string, desired json.RawMessage) error {
	if !m.preparing.TryLock() {
		return errConfigurationConflict
	}
	defer m.preparing.Unlock()
	m.mu.Lock()
	if expected != m.state.Revision {
		m.mu.Unlock()
		return errConfigurationConflict
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		m.mu.Unlock()
		return err
	}
	revision := hex.EncodeToString(token)
	previous := m.state
	m.state.Revision = revision
	m.state.Desired = append(json.RawMessage(nil), desired...)
	m.state.Status = "preparing"
	m.state.Error = ""
	if err := m.persist(); err != nil {
		m.state = previous
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()
	dir := filepath.Join(m.root, "revisions", revision)
	launch, err := m.prepare(ctx, dir, desired)
	if err != nil {
		_ = os.RemoveAll(dir)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.state.Status = "failed"
		m.state.Error = "configuration preparation failed; last ready revision retained"
		_ = m.persist()
		return err
	}
	// The symlink is the commit point for both launch metadata and shared settings.
	temporary := filepath.Join(m.root, "next-"+revision)
	if err = os.Symlink(filepath.Join("revisions", revision), temporary); err == nil {
		err = os.Rename(temporary, filepath.Join(m.root, "current"))
	}
	if err != nil {
		os.Remove(temporary)
		m.state.Status = "failed"
		m.state.Error = "configuration activation failed"
		_ = m.persist()
		return err
	}
	m.launch = &launch
	m.state.Status = "ready"
	m.state.Error = ""
	m.state.EffectiveRevision = revision
	m.state.Effective = append(json.RawMessage(nil), desired...)
	// current is already committed. A status-write failure must not report a
	// rejected update; restart reconstructs ready state from the same pointer.
	_ = m.persist()
	return nil
}

func (m *configurationManager) prepare(ctx context.Context, dir string, desired json.RawMessage) (Harness, error) {
	if err := os.Mkdir(dir, 0700); err != nil {
		return Harness{}, err
	}
	launch, err := m.preparer.Prepare(ctx, dir, desired)
	if err != nil {
		return Harness{}, err
	}
	if err = ctx.Err(); err != nil {
		return Harness{}, err
	}
	check := Config{ACPRemote: "/validated-by-owner", MaxConnections: 1, Harnesses: map[string]Harness{"pi": launch}}
	if err = check.validate(); err != nil {
		return Harness{}, fmt.Errorf("invalid prepared launch: %w", err)
	}
	data, err := json.Marshal(preparedRevision{Configuration: desired, Launch: launch})
	if err != nil {
		return Harness{}, err
	}
	if err = atomicWrite(filepath.Join(dir, "prepared.json"), data); err != nil {
		return Harness{}, err
	}
	return launch, nil
}

// Caller holds mu; desired state is separate from the atomic active-revision pointer.
func (m *configurationManager) persist() error {
	data, err := json.Marshal(m.state)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(m.root, "desired.json"), data)
}

func atomicWrite(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".config-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}
