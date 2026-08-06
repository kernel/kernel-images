package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanupSeedEnvoyStopsProcessAndRemovesBootstrap(t *testing.T) {
	bootstrapPath := filepath.Join(t.TempDir(), "bootstrap.yaml")
	require.NoError(t, os.WriteFile(bootstrapPath, []byte("seed"), 0o600))

	var events []string
	err := cleanupSeedEnvoyWith(
		bootstrapPath,
		func() { events = append(events, "stop") },
		func() bool {
			events = append(events, "stopped")
			return false
		},
	)
	require.NoError(t, err)

	_, err = os.Stat(bootstrapPath)
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Equal(t, []string{"stop", "stopped"}, events)
}
