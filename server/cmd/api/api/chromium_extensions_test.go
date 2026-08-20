package api

import (
	"bytes"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func extensionUploadReader(t *testing.T, count int, name func(int) string) *multipart.Reader {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for i := 0; i < count; i++ {
		part, err := writer.CreateFormFile("extensions.zip_file", "extension.zip")
		require.NoError(t, err)
		_, err = io.WriteString(part, "zip")
		require.NoError(t, err)
		require.NoError(t, writer.WriteField("extensions.name", name(i)))
	}
	require.NoError(t, writer.Close())
	return multipart.NewReader(&body, writer.Boundary())
}

func TestReadExtensionUploadLimitsCount(t *testing.T) {
	reader := extensionUploadReader(t, maxExtensionUploadCount+1, func(i int) string {
		return "extension-" + strconv.Itoa(i)
	})
	items, cleanup, uploadErr := readExtensionUpload(reader)
	defer cleanup()

	require.Nil(t, items)
	require.NotNil(t, uploadErr)
	require.Equal(t, extensionUploadBadRequest, uploadErr.kind)
	require.Contains(t, uploadErr.message, "too many extensions")
}

func TestReadExtensionUploadRejectsDuplicateNames(t *testing.T) {
	reader := extensionUploadReader(t, 2, func(int) string { return "duplicate" })
	items, cleanup, uploadErr := readExtensionUpload(reader)
	defer cleanup()

	require.Nil(t, items)
	require.NotNil(t, uploadErr)
	require.Equal(t, extensionUploadBadRequest, uploadErr.kind)
	require.Equal(t, "duplicate extension name: duplicate", uploadErr.message)
}

func TestCopyExtensionZipLimitsBytes(t *testing.T) {
	var dst bytes.Buffer
	written, uploadErr := copyExtensionZip(&dst, strings.NewReader("12345"), 4)

	require.EqualValues(t, 5, written)
	require.NotNil(t, uploadErr)
	require.Equal(t, extensionUploadBadRequest, uploadErr.kind)
}

func TestOptionalFileSnapshotRestoresExistingAndMissingFiles(t *testing.T) {
	dir := t.TempDir()
	existingPath := filepath.Join(dir, "existing")
	require.NoError(t, os.WriteFile(existingPath, []byte("before"), 0o600))
	existing, err := captureOptionalFileSnapshot(existingPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(existingPath, []byte("after"), 0o644))
	require.NoError(t, restoreOptionalFileSnapshot(existing))
	data, err := os.ReadFile(existingPath)
	require.NoError(t, err)
	require.Equal(t, "before", string(data))
	info, err := os.Stat(existingPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	missingPath := filepath.Join(dir, "missing")
	missing, err := captureOptionalFileSnapshot(missingPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(missingPath, []byte("created"), 0o644))
	require.NoError(t, restoreOptionalFileSnapshot(missing))
	_, err = os.Stat(missingPath)
	require.True(t, os.IsNotExist(err))
}
