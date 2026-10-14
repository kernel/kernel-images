package statefile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteJSONCreatesAndReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	require.NoError(t, WriteJSON(path, map[string]bool{"on": true}))
	require.NoError(t, WriteJSON(path, map[string]bool{"on": false}))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "{\"on\":false}\n", string(data))
	_, err = os.Stat(path + ".tmp")
	require.True(t, os.IsNotExist(err), "the temporary file is renamed into place")
}
