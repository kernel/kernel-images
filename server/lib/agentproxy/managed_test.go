package agentproxy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type prepareFunc func(context.Context, string, json.RawMessage) (Harness, error)

func (f prepareFunc) Prepare(ctx context.Context, dir string, data json.RawMessage) (Harness, error) {
	return f(ctx, dir, data)
}

func TestConfigurationCommitAndRecovery(t *testing.T) {
	root := t.TempDir()
	prepare := prepareFunc(func(_ context.Context, dir string, data json.RawMessage) (Harness, error) {
		if string(data) == `{"fail":true}` {
			return Harness{}, errors.New("installation failed")
		}
		return Harness{Command: "/bin/true", Cwd: dir}, nil
	})
	m, err := newConfigurationManager(root, prepare)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.preparedLaunch(); ok {
		t.Fatal("unprepared launch available")
	}
	if err = m.apply(context.Background(), "0", json.RawMessage(`{"version":1}`)); err != nil {
		t.Fatal(err)
	}
	ready := m.snapshot()
	if ready.Status != "ready" || ready.Revision != ready.EffectiveRevision {
		t.Fatal(ready)
	}
	if err = m.apply(context.Background(), "0", json.RawMessage(`{}`)); !errors.Is(err, errConfigurationConflict) {
		t.Fatal("stale write accepted")
	}
	if err = m.apply(context.Background(), ready.Revision, json.RawMessage(`{"fail":true}`)); err == nil {
		t.Fatal("failed preparation succeeded")
	}
	failed := m.snapshot()
	if failed.Status != "failed" || failed.EffectiveRevision != ready.Revision || string(failed.Effective) != `{"version":1}` {
		t.Fatal(failed)
	}
	restored, err := newConfigurationManager(root, prepare)
	if err != nil {
		t.Fatal(err)
	}
	if restored.snapshot().EffectiveRevision != ready.Revision {
		t.Fatal("restart lost last ready revision")
	}
	// A crash after the commit point but before the status write is recovered from current.
	restored.state.Status = "preparing"
	restored.state.Revision = ready.Revision
	if err = restored.persist(); err != nil {
		t.Fatal(err)
	}
	restored, err = newConfigurationManager(root, prepare)
	if err != nil {
		t.Fatal(err)
	}
	if restored.snapshot().Status != "ready" {
		t.Fatal("committed revision not recovered")
	}
}

func TestPreparationIsExclusiveAndCancellable(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan error, 1)
	m, err := newConfigurationManager(t.TempDir(), prepareFunc(func(ctx context.Context, _ string, _ json.RawMessage) (Harness, error) {
		close(started)
		<-ctx.Done()
		return Harness{}, ctx.Err()
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { finish <- m.apply(ctx, "0", json.RawMessage(`{}`)) }()
	<-started
	state := m.snapshot()
	if state.Status != "preparing" {
		t.Fatal(state)
	}
	if err = m.apply(ctx, state.Revision, json.RawMessage(`{}`)); !errors.Is(err, errConfigurationConflict) {
		t.Fatal("concurrent preparation accepted")
	}
	cancel()
	if err = <-finish; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, ok := m.preparedLaunch(); ok {
		t.Fatal("cancelled revision activated")
	}
}

func TestConfigurationFilesArePrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := atomicWrite(path, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
}
