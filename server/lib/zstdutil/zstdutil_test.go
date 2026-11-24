package zstdutil

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTarZstdDirFollowsSymlinkedRoot(t *testing.T) {
	realDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(realDir, "file.txt"), []byte("contents"), 0644))
	linkDir := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(realDir, linkDir))

	archive, err := TarZstdDirToBytes(linkDir, LevelFastest)
	require.NoError(t, err)

	destDir := t.TempDir()
	require.NoError(t, UntarZstd(bytes.NewReader(archive), destDir, 0))
	contents, err := os.ReadFile(filepath.Join(destDir, "file.txt"))
	require.NoError(t, err)
	assert.Equal(t, "contents", string(contents))
}
