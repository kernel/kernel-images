package ziputil

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ZipDir creates a zip file from a directory.
func ZipDir(sourceDir string) ([]byte, error) {
	var buf bytes.Buffer
	zipWriter := zip.NewWriter(&buf)
	defer zipWriter.Close()

	err := filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return fmt.Errorf("walk error at %s: %w", path, err)
		}
		// Create a relative path
		relPath, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return fmt.Errorf("rel path for %s: %w", path, err)
		}

		// Skip the directory itself
		if relPath == "." {
			return nil
		}

		// Create zip header
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return fmt.Errorf("header for %s: %w", path, err)
		}
		header.Name = relPath

		if info.IsDir() {
			header.Name += "/"
		} else {
			header.Method = zip.Deflate
		}

		writer, err := zipWriter.CreateHeader(header)
		if err != nil {
			return fmt.Errorf("create header for %s: %w", path, err)
		}

		if info.IsDir() {
			return nil
		}

		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("read symlink %s: %w", path, err)
			}
			if _, err := writer.Write([]byte(target)); err != nil {
				return fmt.Errorf("write symlink %s: %w", path, err)
			}
			return nil
		}

		// Only include regular files. Skip sockets, devices, FIFOs, etc.
		if !info.Mode().IsRegular() {
			return nil
		}

		// Add file content
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open file %s: %w", path, err)
		}
		defer file.Close()

		_, err = io.Copy(writer, file)
		if err != nil {
			return fmt.Errorf("copy file %s: %w", path, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := zipWriter.Close(); err != nil {
		return nil, fmt.Errorf("close zip writer: %w", err)
	}
	return buf.Bytes(), nil
}

// Unzip extracts a zip file to the specified directory
func Unzip(zipFilePath, destDir string) error {
	// Open the zip file
	reader, err := zip.OpenReader(zipFilePath)
	if err != nil {
		return fmt.Errorf("failed to open zip file: %w", err)
	}
	defer reader.Close()

	// Create the destination directory if it doesn't exist
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}
	cleanDestDir, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("failed to resolve destination directory: %w", err)
	}
	cleanDestDir, err = filepath.EvalSymlinks(cleanDestDir)
	if err != nil {
		return fmt.Errorf("failed to evaluate destination directory: %w", err)
	}

	// Extract each file
	for _, file := range reader.File {
		entryPath := filepath.FromSlash(file.Name)

		// Create the full destination path
		destPath := filepath.Join(cleanDestDir, entryPath)

		// Check for directory traversal vulnerabilities
		if destPath == cleanDestDir || !isPathWithinDir(cleanDestDir, destPath) {
			return fmt.Errorf("illegal file path: %s", file.Name)
		}
		resolvedParentPath, err := resolvePathWithSymlinks(cleanDestDir, filepath.Dir(entryPath))
		if err != nil {
			return fmt.Errorf("failed to resolve destination path %s: %w", file.Name, err)
		}
		if !isPathWithinDir(cleanDestDir, resolvedParentPath) {
			return fmt.Errorf("illegal file path: %s", file.Name)
		}

		// Handle directories
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(destPath, 0755); err != nil {
				return fmt.Errorf("failed to create directory: %w", err)
			}
			continue
		}

		// Create the containing directory if it doesn't exist
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return fmt.Errorf("failed to create directory path: %w", err)
		}

		// Open the file from the zip
		fileReader, err := file.Open()
		if err != nil {
			return fmt.Errorf("failed to open file in zip: %w", err)
		}
		defer fileReader.Close()

		if file.Mode()&os.ModeSymlink != 0 {
			target, err := io.ReadAll(fileReader)
			if err != nil {
				return fmt.Errorf("failed to read symlink target: %w", err)
			}
			targetPath := string(target)
			if !filepath.IsAbs(targetPath) {
				resolvedTarget, err := resolvePathWithSymlinks(resolvedParentPath, targetPath)
				if err != nil {
					return fmt.Errorf("failed to resolve symlink target: %w", err)
				}
				if !isPathWithinDir(cleanDestDir, resolvedTarget) {
					return fmt.Errorf("illegal symlink target: %s -> %s", file.Name, targetPath)
				}
			}
			if err := os.Remove(destPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("failed to remove existing symlink path: %w", err)
			}
			if err := os.Symlink(targetPath, destPath); err != nil {
				return fmt.Errorf("failed to create symlink: %w", err)
			}
			continue
		}

		if info, err := os.Lstat(destPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(destPath); err != nil {
				return fmt.Errorf("failed to remove existing symlink: %w", err)
			}
		}

		// Create the destination file
		destFile, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, file.Mode())
		if err != nil {
			return fmt.Errorf("failed to create destination file (file mode %s): %w", file.Mode().String(), err)
		}
		defer destFile.Close()

		// Copy the contents
		if _, err := io.Copy(destFile, fileReader); err != nil {
			return fmt.Errorf("failed to extract file: %w", err)
		}
	}

	return nil
}

func isPathWithinDir(dir, path string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(os.PathSeparator))
}

func resolvePathWithSymlinks(baseDir, relativePath string) (string, error) {
	currentPath := filepath.Clean(baseDir)
	for _, part := range strings.Split(filepath.FromSlash(relativePath), string(os.PathSeparator)) {
		switch part {
		case "", ".":
			continue
		case "..":
			currentPath = filepath.Dir(currentPath)
			continue
		}

		nextPath := filepath.Join(currentPath, part)
		resolvedPath, err := filepath.EvalSymlinks(nextPath)
		if err == nil {
			currentPath = resolvedPath
			continue
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("evaluate symlinks for %s: %w", nextPath, err)
		}
		currentPath = nextPath
	}

	return filepath.Clean(currentPath), nil
}
