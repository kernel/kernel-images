package ziputil

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestZipDirPreservesSymlinks(t *testing.T) {
	sourceDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "target.txt"), []byte("target contents"), 0644))
	require.NoError(t, os.Symlink("target.txt", filepath.Join(sourceDir, "link.txt")))

	zipContent, err := ZipDir(sourceDir)
	require.NoError(t, err)

	zipFile, err := os.CreateTemp(t.TempDir(), "archive-*.zip")
	require.NoError(t, err)
	_, err = zipFile.Write(zipContent)
	require.NoError(t, err)
	require.NoError(t, zipFile.Close())

	destDir := t.TempDir()
	require.NoError(t, Unzip(zipFile.Name(), destDir))

	linkPath := filepath.Join(destDir, "link.txt")
	info, err := os.Lstat(linkPath)
	require.NoError(t, err)
	assert.True(t, info.Mode()&os.ModeSymlink != 0)
	target, err := os.Readlink(linkPath)
	require.NoError(t, err)
	assert.Equal(t, "target.txt", target)
}

func TestUnzipRejectsEscapingSymlink(t *testing.T) {
	zipPath := createSymlinkZip(t, "../outside.txt")

	err := Unzip(zipPath, t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "illegal symlink target")
}

func TestUnzipRejectsSymlinkChainEscape(t *testing.T) {
	zipPath := createSymlinkChainEscapeZip(t)
	destDir := filepath.Join(t.TempDir(), "extract")

	err := Unzip(zipPath, destDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "illegal symlink target")
}

func TestUnzipPreservesAbsoluteSymlink(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target.txt")
	zipPath := createSymlinkZip(t, target)
	destDir := t.TempDir()

	require.NoError(t, Unzip(zipPath, destDir))
	actualTarget, err := os.Readlink(filepath.Join(destDir, "link.txt"))
	require.NoError(t, err)
	assert.Equal(t, target, actualTarget)
}

func TestUnzipRejectsRootEntry(t *testing.T) {
	zipPath := createNamedSymlinkZip(t, ".", "target.txt")
	destDir := t.TempDir()

	err := Unzip(zipPath, destDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "illegal file path")
}

func TestUnzipRejectsEntryUnderExistingSymlink(t *testing.T) {
	zipPath := createFileZip(t, "link/file.txt")
	destDir := t.TempDir()
	outsideDir := t.TempDir()
	require.NoError(t, os.Symlink(outsideDir, filepath.Join(destDir, "link")))

	err := Unzip(zipPath, destDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "illegal file path")
	require.NoFileExists(t, filepath.Join(outsideDir, "file.txt"))
}

func TestUnzipOverwritesFileWithSymlink(t *testing.T) {
	zipPath := createSymlinkZip(t, "target.txt")
	destDir := t.TempDir()
	linkPath := filepath.Join(destDir, "link.txt")
	require.NoError(t, os.WriteFile(linkPath, []byte("old contents"), 0644))

	require.NoError(t, Unzip(zipPath, destDir))

	info, err := os.Lstat(linkPath)
	require.NoError(t, err)
	assert.True(t, info.Mode()&os.ModeSymlink != 0)
	target, err := os.Readlink(linkPath)
	require.NoError(t, err)
	assert.Equal(t, "target.txt", target)
}

func createSymlinkZip(t *testing.T, target string) string {
	t.Helper()
	return createNamedSymlinkZip(t, "link.txt", target)
}

func createNamedSymlinkZip(t *testing.T, name, target string) string {
	t.Helper()

	zipPath := filepath.Join(t.TempDir(), "symlink.zip")
	zipFile, err := os.Create(zipPath)
	require.NoError(t, err)

	zipWriter := zip.NewWriter(zipFile)
	header := &zip.FileHeader{Name: name, Method: zip.Store}
	header.SetMode(os.ModeSymlink | 0777)
	writer, err := zipWriter.CreateHeader(header)
	require.NoError(t, err)
	_, err = writer.Write([]byte(target))
	require.NoError(t, err)
	require.NoError(t, zipWriter.Close())
	require.NoError(t, zipFile.Close())

	return zipPath
}

func createFileZip(t *testing.T, name string) string {
	t.Helper()

	zipPath := filepath.Join(t.TempDir(), "file.zip")
	zipFile, err := os.Create(zipPath)
	require.NoError(t, err)
	zipWriter := zip.NewWriter(zipFile)
	writer, err := zipWriter.Create(name)
	require.NoError(t, err)
	_, err = writer.Write([]byte("contents"))
	require.NoError(t, err)
	require.NoError(t, zipWriter.Close())
	require.NoError(t, zipFile.Close())
	return zipPath
}

func createSymlinkChainEscapeZip(t *testing.T) string {
	t.Helper()

	zipPath := filepath.Join(t.TempDir(), "chain-escape.zip")
	zipFile, err := os.Create(zipPath)
	require.NoError(t, err)

	zipWriter := zip.NewWriter(zipFile)
	linkHeader := &zip.FileHeader{Name: "link", Method: zip.Store}
	linkHeader.SetMode(os.ModeSymlink | 0777)
	linkWriter, err := zipWriter.CreateHeader(linkHeader)
	require.NoError(t, err)
	_, err = linkWriter.Write([]byte("."))
	require.NoError(t, err)

	escapeHeader := &zip.FileHeader{Name: "escape", Method: zip.Store}
	escapeHeader.SetMode(os.ModeSymlink | 0777)
	escapeWriter, err := zipWriter.CreateHeader(escapeHeader)
	require.NoError(t, err)
	_, err = escapeWriter.Write([]byte("link/.."))
	require.NoError(t, err)

	require.NoError(t, zipWriter.Close())
	require.NoError(t, zipFile.Close())
	return zipPath
}

func TestUnzipFile(t *testing.T) {
	// Create a temporary directory for test files
	sourceDir, err := os.MkdirTemp("", "zip-source-*")
	require.NoError(t, err)
	defer os.RemoveAll(sourceDir)

	// Create test files
	testFiles := map[string]string{
		"app.py":            "print('Hello, World!')",
		"requirements.txt":  "requests==2.26.0",
		"utils/helpers.py":  "def greet(): return 'Hello'",
		"utils/__init__.py": "",
		"data/sample.json":  "{\"key\": \"value\"}",
	}

	for path, content := range testFiles {
		fullPath := filepath.Join(sourceDir, path)
		err := os.MkdirAll(filepath.Dir(fullPath), 0755)
		require.NoError(t, err)
		err = os.WriteFile(fullPath, []byte(content), 0644)
		require.NoError(t, err)
	}

	// Create a zip file from the source directory
	zipFilePath := filepath.Join(sourceDir, "test.zip")
	zipFile, err := os.Create(zipFilePath)
	require.NoError(t, err)
	defer zipFile.Close()

	// Write zip content
	zipContent, err := ZipDir(sourceDir)
	require.NoError(t, err)
	zipFile.Close() // Close to flush content

	// Create a new zipFile from the content for testing
	tempZipFile, err := os.CreateTemp("", "test-*.zip")
	require.NoError(t, err)
	defer os.Remove(tempZipFile.Name())
	defer tempZipFile.Close()

	_, err = tempZipFile.Write(zipContent)
	require.NoError(t, err)
	tempZipFile.Close()

	// Create destination directory for unzipping
	destDir, err := os.MkdirTemp("", "zip-dest-*")
	require.NoError(t, err)
	defer os.RemoveAll(destDir)

	// Test the UnzipFile function
	err = Unzip(tempZipFile.Name(), destDir)
	require.NoError(t, err)

	// Verify that all files exist in the extracted directory
	for path, expectedContent := range testFiles {
		extractedPath := filepath.Join(destDir, path)
		require.FileExists(t, extractedPath)

		content, err := os.ReadFile(extractedPath)
		require.NoError(t, err)
		assert.Equal(t, expectedContent, string(content))
	}

	// Test handling of malicious zip entries (directory traversal)
	maliciousZipPath := filepath.Join(sourceDir, "malicious.zip")
	createMaliciousZip(t, maliciousZipPath)

	// Create destination directory for malicious unzipping
	maliciousDestDir, err := os.MkdirTemp("", "zip-malicious-*")
	require.NoError(t, err)
	defer os.RemoveAll(maliciousDestDir)

	// This should fail with an illegal file path error
	err = Unzip(maliciousZipPath, maliciousDestDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "illegal file path")
}

// createMaliciousZip creates a ZIP file with a path traversal attempt
func createMaliciousZip(t *testing.T, zipPath string) {
	zipFile, err := os.Create(zipPath)
	require.NoError(t, err)
	defer zipFile.Close()

	zipWriter := zip.NewWriter(zipFile)
	defer zipWriter.Close()

	// Create a file with path traversal attempt
	traversalPath := "../../../../../etc/passwd"
	fileHeader := &zip.FileHeader{
		Name:   traversalPath,
		Method: zip.Deflate,
	}

	writer, err := zipWriter.CreateHeader(fileHeader)
	require.NoError(t, err)

	_, err = writer.Write([]byte("malicious content"))
	require.NoError(t, err)

	err = zipWriter.Close()
	require.NoError(t, err)
}
