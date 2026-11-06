// Package zstdutil provides utilities for creating and extracting tar.zst archives.
package zstdutil

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// CompressionLevel represents the zstd compression level.
type CompressionLevel string

const (
	LevelFastest CompressionLevel = "fastest"
	LevelDefault CompressionLevel = "default"
	LevelBetter  CompressionLevel = "better"
	LevelBest    CompressionLevel = "best"
)

// ToZstdLevel converts a CompressionLevel to a zstd.EncoderLevel.
func (l CompressionLevel) ToZstdLevel() zstd.EncoderLevel {
	switch l {
	case LevelFastest:
		return zstd.SpeedFastest
	case LevelBetter:
		return zstd.SpeedBetterCompression
	case LevelBest:
		return zstd.SpeedBestCompression
	default:
		return zstd.SpeedDefault
	}
}

// TarZstdDir creates a tar.zst archive from a directory and writes it to the provided writer.
// This is a streaming implementation that doesn't buffer the entire archive in memory.
func TarZstdDir(w io.Writer, sourceDir string, level CompressionLevel) error {
	// filepath.Walk does not descend into a symlinked root, so resolve it first.
	sourceDir, err := filepath.EvalSymlinks(sourceDir)
	if err != nil {
		return fmt.Errorf("resolve source dir: %w", err)
	}

	// Create zstd encoder
	zw, err := zstd.NewWriter(w,
		zstd.WithEncoderLevel(level.ToZstdLevel()),
		zstd.WithEncoderConcurrency(1), // Synchronous for predictable streaming
	)
	if err != nil {
		return fmt.Errorf("create zstd encoder: %w", err)
	}
	defer zw.Close()

	// Create tar writer on top of zstd
	tw := tar.NewWriter(zw)
	defer tw.Close()

	// Walk directory and write to tar
	err = filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return fmt.Errorf("walk error at %s: %w", path, err)
		}

		// Create relative path
		relPath, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return fmt.Errorf("rel path for %s: %w", path, err)
		}

		// Skip the root directory itself
		if relPath == "." {
			return nil
		}

		// Create tar header
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("header for %s: %w", path, err)
		}
		header.Name = relPath

		// Handle symlinks
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %s: %w", path, err)
			}
			header.Linkname = link
		}

		// Write header
		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("write header for %s: %w", path, err)
		}

		// If it's a regular file, write the content
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("open file %s: %w", path, err)
			}
			defer f.Close()

			if _, err := io.Copy(tw, f); err != nil {
				return fmt.Errorf("copy file %s: %w", path, err)
			}
		}

		return nil
	})

	if err != nil {
		return err
	}

	// Close tar writer first to flush tar footer
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar writer: %w", err)
	}

	// Close zstd writer to flush compression
	if err := zw.Close(); err != nil {
		return fmt.Errorf("close zstd writer: %w", err)
	}

	return nil
}

// UntarZstd extracts a tar.zst archive from the reader to the destination directory.
// stripComponents specifies the number of leading path components to strip.
func UntarZstd(r io.Reader, destDir string, stripComponents int) error {
	// Create zstd decoder
	zr, err := zstd.NewReader(r,
		zstd.WithDecoderConcurrency(1), // Synchronous for predictable streaming
	)
	if err != nil {
		return fmt.Errorf("create zstd decoder: %w", err)
	}
	defer zr.Close()

	// Create tar reader on top of zstd
	tr := tar.NewReader(zr)

	// Ensure destination directory exists
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("create dest dir: %w", err)
	}

	// Extract files
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar header: %w", err)
		}

		// Apply strip-components
		name := header.Name
		if stripComponents > 0 {
			parts := strings.Split(name, string(os.PathSeparator))
			if len(parts) <= stripComponents {
				continue // Skip this entry, not enough components
			}
			name = filepath.Join(parts[stripComponents:]...)
		}

		// Skip empty names (can happen after stripping)
		if name == "" || name == "." {
			continue
		}

		// Create full destination path
		destPath := filepath.Join(destDir, name)

		// Security check: prevent path traversal
		if !strings.HasPrefix(filepath.Clean(destPath), filepath.Clean(destDir)+string(os.PathSeparator)) {
			return fmt.Errorf("illegal file path: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(destPath, os.FileMode(header.Mode)); err != nil {
				return fmt.Errorf("create directory %s: %w", destPath, err)
			}

		case tar.TypeReg:
			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
				return fmt.Errorf("create parent dir for %s: %w", destPath, err)
			}

			// Create file
			f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return fmt.Errorf("create file %s: %w", destPath, err)
			}

			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("extract file %s: %w", destPath, err)
			}
			f.Close()

		case tar.TypeSymlink:
			// Security check for relative symlinks: ensure they don't escape destDir
			// Absolute symlinks are allowed (e.g., chromium creates symlinks to /tmp)
			if !filepath.IsAbs(header.Linkname) {
				symlinkDir := filepath.Dir(destPath)
				resolvedTarget := filepath.Clean(filepath.Join(symlinkDir, header.Linkname))
				if !strings.HasPrefix(resolvedTarget, filepath.Clean(destDir)+string(os.PathSeparator)) &&
					resolvedTarget != filepath.Clean(destDir) {
					return fmt.Errorf("illegal symlink target (escapes destination): %s -> %s", header.Name, header.Linkname)
				}
			}

			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
				return fmt.Errorf("create parent dir for symlink %s: %w", destPath, err)
			}

			// Remove existing file if present
			os.Remove(destPath)

			if err := os.Symlink(header.Linkname, destPath); err != nil {
				return fmt.Errorf("create symlink %s: %w", destPath, err)
			}

		case tar.TypeLink:
			// Hard link - apply strip-components to link target
			linkName := header.Linkname
			if stripComponents > 0 {
				parts := strings.Split(linkName, string(os.PathSeparator))
				if len(parts) <= stripComponents {
					continue // Skip this hard link, target has insufficient components
				}
				linkName = filepath.Join(parts[stripComponents:]...)
			}
			linkPath := filepath.Join(destDir, linkName)

			// Security check: ensure hard link target is within destDir
			if !strings.HasPrefix(filepath.Clean(linkPath), filepath.Clean(destDir)+string(os.PathSeparator)) {
				return fmt.Errorf("illegal hard link target: %s -> %s", header.Name, header.Linkname)
			}

			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
				return fmt.Errorf("create parent dir for link %s: %w", destPath, err)
			}

			// Remove existing file if present
			os.Remove(destPath)

			if err := os.Link(linkPath, destPath); err != nil {
				return fmt.Errorf("create hard link %s: %w", destPath, err)
			}

		default:
			// Skip other types (devices, FIFOs, etc.)
			continue
		}
	}

	return nil
}

// TarZstdDirToBytes creates a tar.zst archive from a directory and returns it as bytes.
// This is a convenience function for smaller directories where buffering is acceptable.
func TarZstdDirToBytes(sourceDir string, level CompressionLevel) ([]byte, error) {
	var buf bytes.Buffer
	if err := TarZstdDir(&buf, sourceDir, level); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
