package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"sync"

	"os/user"

	"github.com/fsnotify/fsnotify"
	"github.com/nrednav/cuid2"
	"github.com/onkernel/kernel-images/server/lib/logger"
	oapi "github.com/onkernel/kernel-images/server/lib/oapi"
	"github.com/onkernel/kernel-images/server/lib/ziputil"
	"github.com/onkernel/kernel-images/server/lib/zstdutil"
)

// fsWatch represents an in-memory directory watch.
type fsWatch struct {
	path      string
	recursive bool
	events    chan oapi.FileSystemEvent
	watcher   *fsnotify.Watcher
	closeOnce sync.Once
}

// addRecursive walks the directory and registers all subdirectories when recursive=true.
func addRecursive(w *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return w.Add(path)
		}
		return nil
	})
}

// Close safely shuts down the underlying fsnotify.Watcher exactly once.
func (fw *fsWatch) Close() {
	fw.closeOnce.Do(func() {
		_ = fw.watcher.Close()
	})
}

// ReadFile returns the contents of a file specified by the path param.
func (s *ApiService) ReadFile(ctx context.Context, req oapi.ReadFileRequestObject) (oapi.ReadFileResponseObject, error) {
	log := logger.FromContext(ctx)
	path := req.Params.Path
	if path == "" {
		return oapi.ReadFile400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "path cannot be empty"}}, nil
	}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return oapi.ReadFile404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "file not found"}}, nil
		}
		log.Error("failed to open file", "err", err, "path", path)
		return oapi.ReadFile500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "unable to open file"}}, nil
	}

	stat, err := f.Stat()
	if err != nil {
		f.Close()
		log.Error("failed to stat file", "err", err, "path", path)
		return oapi.ReadFile500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "unable to stat file"}}, nil
	}

	return oapi.ReadFile200ApplicationoctetStreamResponse{
		Body:          f,
		ContentLength: stat.Size(),
	}, nil
}

// WriteFile creates or overwrites a file with the supplied data stream.
func (s *ApiService) WriteFile(ctx context.Context, req oapi.WriteFileRequestObject) (oapi.WriteFileResponseObject, error) {
	log := logger.FromContext(ctx)
	path := req.Params.Path
	if path == "" {
		return oapi.WriteFile400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "path cannot be empty"}}, nil
	}
	if req.Body == nil {
		return oapi.WriteFile400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "empty request body"}}, nil
	}

	// create parent directories if necessary
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Error("failed to create directories", "err", err, "path", path)
		return oapi.WriteFile500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "unable to create directories"}}, nil
	}

	// determine desired file mode (default 0o644)
	perm := os.FileMode(0o644)
	if req.Params.Mode != nil {
		if v, err := strconv.ParseUint(*req.Params.Mode, 8, 32); err == nil {
			perm = os.FileMode(v)
		} else {
			log.Error("invalid mode", "mode", *req.Params.Mode)
			return oapi.WriteFile400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid mode"}}, nil
		}
	}

	// open the file with the specified permissions
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		log.Error("failed to create file", "err", err, "path", path)
		return oapi.WriteFile500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "unable to create file"}}, nil
	}
	defer f.Close()

	if _, err := io.Copy(f, req.Body); err != nil {
		log.Error("failed to write file", "err", err, "path", path)
		return oapi.WriteFile500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to write data"}}, nil
	}

	return oapi.WriteFile201Response{}, nil
}

// CreateDirectory creates a new directory (recursively) with an optional mode.
func (s *ApiService) CreateDirectory(ctx context.Context, req oapi.CreateDirectoryRequestObject) (oapi.CreateDirectoryResponseObject, error) {
	log := logger.FromContext(ctx)
	if req.Body == nil {
		return oapi.CreateDirectory400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body required"}}, nil
	}
	path := req.Body.Path
	if path == "" {
		return oapi.CreateDirectory400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "path cannot be empty"}}, nil
	}
	// default to 0o755
	perm := os.FileMode(0o755)
	if req.Body.Mode != nil {
		if v, err := strconv.ParseUint(*req.Body.Mode, 8, 32); err == nil {
			perm = os.FileMode(v)
		} else {
			log.Error("invalid mode", "mode", *req.Body.Mode)
			return oapi.CreateDirectory400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid mode"}}, nil
		}
	}
	if err := os.MkdirAll(path, perm); err != nil {
		log.Error("failed to create directory", "err", err, "path", path)
		return oapi.CreateDirectory500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to create directory"}}, nil
	}
	return oapi.CreateDirectory201Response{}, nil
}

// DeleteFile removes a single file.
func (s *ApiService) DeleteFile(ctx context.Context, req oapi.DeleteFileRequestObject) (oapi.DeleteFileResponseObject, error) {
	log := logger.FromContext(ctx)
	if req.Body == nil {
		return oapi.DeleteFile400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body required"}}, nil
	}
	path := req.Body.Path
	if path == "" {
		return oapi.DeleteFile400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "path cannot be empty"}}, nil
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return oapi.DeleteFile404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "file not found"}}, nil
		}
		log.Error("failed to delete file", "err", err, "path", path)
		return oapi.DeleteFile500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to delete file"}}, nil
	}
	return oapi.DeleteFile200Response{}, nil
}

// DeleteDirectory removes a directory and its contents.
func (s *ApiService) DeleteDirectory(ctx context.Context, req oapi.DeleteDirectoryRequestObject) (oapi.DeleteDirectoryResponseObject, error) {
	log := logger.FromContext(ctx)
	if req.Body == nil {
		return oapi.DeleteDirectory400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body required"}}, nil
	}
	path := req.Body.Path
	if path == "" {
		return oapi.DeleteDirectory400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "path cannot be empty"}}, nil
	}
	if err := os.RemoveAll(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return oapi.DeleteDirectory404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "directory not found"}}, nil
		}
		log.Error("failed to delete directory", "err", err, "path", path)
		return oapi.DeleteDirectory500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to delete directory"}}, nil
	}
	return oapi.DeleteDirectory200Response{}, nil
}

// ListFiles returns FileInfo entries for the contents of a directory.
func (s *ApiService) ListFiles(ctx context.Context, req oapi.ListFilesRequestObject) (oapi.ListFilesResponseObject, error) {
	log := logger.FromContext(ctx)
	path := req.Params.Path
	if path == "" {
		return oapi.ListFiles400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "path cannot be empty"}}, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return oapi.ListFiles404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "directory not found"}}, nil
		}
		log.Error("failed to read directory", "err", err, "path", path)
		return oapi.ListFiles500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to read directory"}}, nil
	}
	var list oapi.ListFiles
	for _, entry := range entries {
		// Retrieve FileInfo for each entry. If this fails (e.g. broken symlink, permission
		// error) we surface the failure to the client instead of silently ignoring it so
		// that consumers do not unknowingly operate on incomplete or unreliable metadata.
		info, err := entry.Info()
		if err != nil {
			log.Error("failed to stat directory entry", "err", err, "dir", path, "entry", entry.Name())
			return oapi.ListFiles500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to stat directory entry"}}, nil
		}

		// By specification SizeBytes should be 0 for directories.
		size := 0
		if !info.IsDir() {
			size = int(info.Size())
		}

		fi := oapi.FileInfo{
			Name:      entry.Name(),
			Path:      filepath.Join(path, entry.Name()),
			IsDir:     entry.IsDir(),
			SizeBytes: size,
			ModTime:   info.ModTime(),
			Mode:      info.Mode().String(),
		}

		list = append(list, fi)
	}
	return oapi.ListFiles200JSONResponse(list), nil
}

// FileInfo returns metadata about a file or directory.
func (s *ApiService) FileInfo(ctx context.Context, req oapi.FileInfoRequestObject) (oapi.FileInfoResponseObject, error) {
	log := logger.FromContext(ctx)
	path := req.Params.Path
	if path == "" {
		return oapi.FileInfo400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "path cannot be empty"}}, nil
	}
	stat, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return oapi.FileInfo404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "path not found"}}, nil
		}
		log.Error("failed to stat path", "err", err, "path", path)
		return oapi.FileInfo500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to stat path"}}, nil
	}
	// By specification SizeBytes should be 0 for directories.
	// Match behaviour of ListFiles for consistency.
	size := 0
	if !stat.IsDir() {
		size = int(stat.Size())
	}
	fi := oapi.FileInfo{
		Name:      filepath.Base(path),
		Path:      path,
		IsDir:     stat.IsDir(),
		SizeBytes: size,
		ModTime:   stat.ModTime(),
		Mode:      stat.Mode().String(),
	}
	return oapi.FileInfo200JSONResponse(fi), nil
}

// MovePath renames or moves a file/directory.
func (s *ApiService) MovePath(ctx context.Context, req oapi.MovePathRequestObject) (oapi.MovePathResponseObject, error) {
	log := logger.FromContext(ctx)
	if req.Body == nil {
		return oapi.MovePath400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body required"}}, nil
	}
	src := req.Body.SrcPath
	dst := req.Body.DestPath
	if src == "" || dst == "" {
		return oapi.MovePath400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "src_path and dest_path required"}}, nil
	}
	if err := os.Rename(src, dst); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return oapi.MovePath404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "source not found"}}, nil
		}
		log.Error("failed to move path", "err", err, "src", src, "dst", dst)
		return oapi.MovePath500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to move path"}}, nil
	}
	return oapi.MovePath200Response{}, nil
}

// SetFilePermissions changes mode (and optionally owner/group) of a path.
func (s *ApiService) SetFilePermissions(ctx context.Context, req oapi.SetFilePermissionsRequestObject) (oapi.SetFilePermissionsResponseObject, error) {
	log := logger.FromContext(ctx)
	if req.Body == nil {
		return oapi.SetFilePermissions400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body required"}}, nil
	}
	path := req.Body.Path
	if path == "" {
		return oapi.SetFilePermissions400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "path cannot be empty"}}, nil
	}
	// parse mode
	modeVal, err := strconv.ParseUint(req.Body.Mode, 8, 32)
	if err != nil {
		return oapi.SetFilePermissions400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid mode"}}, nil
	}
	if err := os.Chmod(path, os.FileMode(modeVal)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return oapi.SetFilePermissions404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "path not found"}}, nil
		}
		log.Error("failed to chmod", "err", err, "path", path)
		return oapi.SetFilePermissions500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to chmod"}}, nil
	}
	// chown if owner/group provided (best effort)
	if req.Body.Owner != nil || req.Body.Group != nil {
		uid := -1
		gid := -1
		// Handle owner (uid)
		if req.Body.Owner != nil {
			ownerStr := *req.Body.Owner
			// 1. Try parsing as a numeric UID directly
			if id, err := strconv.Atoi(ownerStr); err == nil && id >= 0 {
				uid = id
			} else {
				// 2. Fall back to name lookup
				if u, err := user.Lookup(ownerStr); err == nil {
					if id, err := strconv.Atoi(u.Uid); err == nil && id >= 0 {
						uid = id
					}
				}
			}
		}

		// Handle group (gid)
		if req.Body.Group != nil {
			groupStr := *req.Body.Group
			// 1. Try parsing as a numeric GID directly
			if id, err := strconv.Atoi(groupStr); err == nil && id >= 0 {
				gid = id
			} else {
				// 2. Fall back to name lookup
				if g, err := user.LookupGroup(groupStr); err == nil {
					if id, err := strconv.Atoi(g.Gid); err == nil && id >= 0 {
						gid = id
					}
				}
			}
		}
		// only attempt if at least one resolved
		if uid != -1 || gid != -1 {
			_ = os.Chown(path, uid, gid) // ignore error (likely EPERM) to keep API simpler
		}
	}
	return oapi.SetFilePermissions200Response{}, nil
}

// StartFsWatch is not implemented in this basic filesystem handler. It returns a 400 error to the client.
func (s *ApiService) StartFsWatch(ctx context.Context, req oapi.StartFsWatchRequestObject) (oapi.StartFsWatchResponseObject, error) {
	log := logger.FromContext(ctx)
	if req.Body == nil {
		return oapi.StartFsWatch400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body required"}}, nil
	}

	path := req.Body.Path
	if path == "" {
		return oapi.StartFsWatch400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "path cannot be empty"}}, nil
	}
	// Ensure path exists
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return oapi.StartFsWatch404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "path not found"}}, nil
		}
		log.Error("failed to stat path", "err", err, "path", path)
		return oapi.StartFsWatch500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "unable to stat path"}}, nil
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Error("failed to create fsnotify watcher", "err", err)
		return oapi.StartFsWatch500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "internal error"}}, nil
	}

	recursive := req.Body.Recursive != nil && *req.Body.Recursive
	if recursive {
		if err := addRecursive(watcher, path); err != nil {
			log.Error("failed to add directories recursively", "err", err)
			watcher.Close()
			return oapi.StartFsWatch500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "internal error"}}, nil
		}
	} else {
		if err := watcher.Add(path); err != nil {
			log.Error("failed to watch path", "err", err, "path", path)
			watcher.Close()
			return oapi.StartFsWatch500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "internal error"}}, nil
		}
	}

	watchID := cuid2.Generate()
	w := &fsWatch{
		path:      path,
		recursive: recursive,
		events:    make(chan oapi.FileSystemEvent, 100),
		watcher:   watcher,
	}

	// Register the watch before starting the forwarding goroutine to avoid a
	// race where the goroutine might exit before it is added to the map.
	s.watchMu.Lock()
	s.watches[watchID] = w
	s.watchMu.Unlock()

	// Start background goroutine to forward events. We intentionally decouple
	// its lifetime from the HTTP request context so that the watch continues
	// to run until it is explicitly stopped via StopFsWatch or until watcher
	// channels are closed.
	go func(s *ApiService, id string) {
		// Ensure resources are cleaned up no matter how the goroutine exits.
		defer func() {
			// Best-effort close (idempotent).
			w.Close()

			// Remove stale entry to avoid map/chan leak if the watch stops on
			// its own (e.g. underlying fs error, watcher overflow, etc.). It
			// is safe to call delete even if StopFsWatch already removed it.
			s.watchMu.Lock()
			delete(s.watches, id)
			s.watchMu.Unlock()

			close(w.events) // close after map cleanup so readers can finish
		}()

		for {
			select {
			case ev, ok := <-watcher.Events:
				if !ok {
					return
				}
				var evType oapi.FileSystemEventType
				switch {
				case ev.Op&fsnotify.Create != 0:
					evType = "CREATE"
				case ev.Op&fsnotify.Write != 0:
					evType = "WRITE"
				case ev.Op&fsnotify.Remove != 0:
					evType = "DELETE"
				case ev.Op&fsnotify.Rename != 0:
					evType = "RENAME"
				default:
					continue
				}
				info, _ := os.Stat(ev.Name)
				isDir := info != nil && info.IsDir()
				name := filepath.Base(ev.Name)
				// Attempt a non-blocking send so that event production never blocks
				// even if the consumer is slow or absent. When the buffer is full we
				// simply drop the event, preferring liveness over completeness.
				select {
				case w.events <- oapi.FileSystemEvent{Type: evType, Path: ev.Name, Name: &name, IsDir: &isDir}:
				default:
				}

				// If recursive and new directory created, add watch recursively so that
				// any nested sub-directories are also monitored.
				if recursive && evType == "CREATE" && isDir {
					if err := addRecursive(watcher, ev.Name); err != nil {
						log.Error("failed to recursively watch new directory", "err", err, "path", ev.Name)
					}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Error("fsnotify error", "err", err)
			}
		}
	}(s, watchID)

	return oapi.StartFsWatch201JSONResponse{WatchId: &watchID}, nil
}

func (s *ApiService) StopFsWatch(ctx context.Context, req oapi.StopFsWatchRequestObject) (oapi.StopFsWatchResponseObject, error) {
	log := logger.FromContext(ctx)
	id := req.WatchId
	s.watchMu.Lock()
	w, ok := s.watches[id]
	if ok {
		delete(s.watches, id)
		w.Close()
		// channel will be closed by the event forwarding goroutine
	}
	s.watchMu.Unlock()

	if !ok {
		log.Warn("stop requested for unknown watch", "watch_id", id)
		return oapi.StopFsWatch404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "watch not found"}}, nil
	}

	return oapi.StopFsWatch204Response{}, nil
}

func (s *ApiService) StreamFsEvents(ctx context.Context, req oapi.StreamFsEventsRequestObject) (oapi.StreamFsEventsResponseObject, error) {
	log := logger.FromContext(ctx)
	id := req.WatchId
	s.watchMu.RLock()
	w, ok := s.watches[id]
	s.watchMu.RUnlock()
	if !ok {
		log.Warn("stream requested for unknown watch", "watch_id", id)
		return oapi.StreamFsEvents404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "watch not found"}}, nil
	}

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		for ev := range w.events {
			// Build SSE formatted event: data: <json>\n\n using a buffer and write in a single call
			data, err := json.Marshal(ev)
			if err != nil {
				log.Error("failed to marshal fs event", "err", err)
				return
			}

			var buf bytes.Buffer
			buf.Grow(len("data: ") + len(data) + 2) // 2 for the separating newlines
			buf.WriteString("data: ")
			buf.Write(data)
			buf.WriteString("\n\n")

			if _, err := pw.Write(buf.Bytes()); err != nil {
				log.Error("failed to write SSE event", "err", err)
				return
			}
		}
	}()

	headers := oapi.StreamFsEvents200ResponseHeaders{XSSEContentType: "application/json"}
	return oapi.StreamFsEvents200TexteventStreamResponse{Body: pr, Headers: headers, ContentLength: 0}, nil
}

// UploadZip handles a multipart upload of a zip archive and extracts it to dest_path.
func (s *ApiService) UploadZip(ctx context.Context, request oapi.UploadZipRequestObject) (oapi.UploadZipResponseObject, error) {
	log := logger.FromContext(ctx)

	if request.Body == nil {
		return oapi.UploadZip400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body required"}}, nil
	}

	// Create temp file for uploaded zip
	tmpZip, err := os.CreateTemp("", "upload-*.zip")
	if err != nil {
		log.Error("failed to create temporary file", "err", err)
		return oapi.UploadZip500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "internal error"}}, nil
	}
	defer os.Remove(tmpZip.Name())
	defer tmpZip.Close()

	var destPath string
	var fileReceived bool

	for {
		part, err := request.Body.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Error("failed to read form part", "err", err)
			return oapi.UploadZip400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "failed to read form part"}}, nil
		}

		switch part.FormName() {
		case "zip_file":
			fileReceived = true
			if _, err := io.Copy(tmpZip, part); err != nil {
				log.Error("failed to read zip data", "err", err)
				return oapi.UploadZip400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "failed to read zip file"}}, nil
			}
		case "dest_path":
			data, err := io.ReadAll(part)
			if err != nil {
				log.Error("failed to read dest_path", "err", err)
				return oapi.UploadZip400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "failed to read dest_path"}}, nil
			}
			destPath = strings.TrimSpace(string(data))
			if destPath == "" || !filepath.IsAbs(destPath) {
				return oapi.UploadZip400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "dest_path must be an absolute path"}}, nil
			}
		default:
			return oapi.UploadZip400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid form field: " + part.FormName()}}, nil
		}
	}

	// Validate required parts
	if !fileReceived || destPath == "" {
		return oapi.UploadZip400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "zip_file and dest_path are required"}}, nil
	}

	// Ensure destination directory exists
	if err := os.MkdirAll(destPath, 0o755); err != nil {
		log.Error("failed to create destination directory", "err", err, "path", destPath)
		return oapi.UploadZip500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to create destination directory"}}, nil
	}

	// Close temp writer prior to unzip
	if err := tmpZip.Close(); err != nil {
		log.Error("failed to finalize temporary zip", "err", err)
		return oapi.UploadZip500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "internal error"}}, nil
	}

	if err := ziputil.Unzip(tmpZip.Name(), destPath); err != nil {
		// Map common user errors to 400, otherwise 500
		msg := err.Error()
		if strings.Contains(msg, "failed to open zip file") || strings.Contains(msg, "illegal file path") {
			return oapi.UploadZip400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid zip file"}}, nil
		}
		log.Error("failed to extract zip", "err", err)
		return oapi.UploadZip500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to extract zip"}}, nil
	}

	return oapi.UploadZip201Response{}, nil
}

// UploadFiles handles multipart form uploads for one or more files. It supports the following
// field name encodings:
//   - files[<index>].file and files[<index>].dest_path
//   - files[<index>][file] and files[<index>][dest_path]
//   - files.<index>.file and files.<index>.dest_path
//
// Additionally, for single-file uploads it accepts:
//   - file and dest_path
func (s *ApiService) UploadFiles(ctx context.Context, request oapi.UploadFilesRequestObject) (oapi.UploadFilesResponseObject, error) {
	log := logger.FromContext(ctx)

	if request.Body == nil {
		return oapi.UploadFiles400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body required"}}, nil
	}

	// Track per-index pending uploads so order of parts does not matter.
	type pendingUpload struct {
		tempPath     string
		destPath     string
		fileReceived bool
	}

	uploads := map[int]*pendingUpload{}
	createdTemps := []string{}
	defer func() {
		for _, p := range createdTemps {
			_ = os.Remove(p)
		}
	}()

	parseIndexAndField := func(name string) (int, string, bool) {
		// single-file shorthand
		if name == "file" || name == "dest_path" {
			if name == "file" {
				return 0, "file", true
			}
			return 0, "dest_path", true
		}
		if !strings.HasPrefix(name, "files") {
			return 0, "", false
		}
		// forms like files[0].file or files[0][file]
		if strings.HasPrefix(name, "files[") {
			end := strings.Index(name, "]")
			if end == -1 {
				return 0, "", false
			}
			idxStr := name[len("files["):end]
			rest := name[end+1:]
			rest = strings.TrimPrefix(rest, ".")
			var field string
			if strings.HasPrefix(rest, "[") && strings.HasSuffix(rest, "]") {
				field = rest[1 : len(rest)-1]
			} else {
				field = rest
			}
			idx := 0
			if v, err := strconv.Atoi(idxStr); err == nil && v >= 0 {
				idx = v
			} else {
				return 0, "", false
			}
			return idx, field, true
		}
		// forms like files.0.file
		if strings.HasPrefix(name, "files.") {
			parts := strings.Split(name, ".")
			if len(parts) != 3 {
				return 0, "", false
			}
			idx := 0
			if v, err := strconv.Atoi(parts[1]); err == nil && v >= 0 {
				idx = v
			} else {
				return 0, "", false
			}
			return idx, parts[2], true
		}
		return 0, "", false
	}

	for {
		part, err := request.Body.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Error("failed to read form part", "err", err)
			return oapi.UploadFiles400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "failed to read form part"}}, nil
		}

		name := part.FormName()
		idx, field, ok := parseIndexAndField(name)
		if !ok {
			return oapi.UploadFiles400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid form field: " + name}}, nil
		}

		pu, exists := uploads[idx]
		if !exists {
			pu = &pendingUpload{}
			uploads[idx] = pu
		}

		switch field {
		case "file":
			// Create temp for the file contents
			tmp, err := os.CreateTemp("", "upload-*")
			if err != nil {
				log.Error("failed to create temporary file", "err", err)
				return oapi.UploadFiles500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "internal error"}}, nil
			}
			tmpPath := tmp.Name()
			createdTemps = append(createdTemps, tmpPath)
			if _, err := io.Copy(tmp, part); err != nil {
				tmp.Close()
				log.Error("failed to read upload data", "err", err)
				return oapi.UploadFiles400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "failed to read upload data"}}, nil
			}
			if err := tmp.Close(); err != nil {
				log.Error("failed to finalize temporary file", "err", err)
				return oapi.UploadFiles500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "internal error"}}, nil
			}
			pu.tempPath = tmpPath
			pu.fileReceived = true

		case "dest_path":
			data, err := io.ReadAll(part)
			if err != nil {
				log.Error("failed to read dest_path", "err", err)
				return oapi.UploadFiles400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "failed to read dest_path"}}, nil
			}
			dest := strings.TrimSpace(string(data))
			if dest == "" || !filepath.IsAbs(dest) {
				return oapi.UploadFiles400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "dest_path must be an absolute path"}}, nil
			}
			pu.destPath = dest

		default:
			return oapi.UploadFiles400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid field: " + field}}, nil
		}
	}

	// Validate and materialize uploads
	if len(uploads) == 0 {
		return oapi.UploadFiles400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "no files provided"}}, nil
	}

	for _, pu := range uploads {
		if !pu.fileReceived || pu.destPath == "" {
			return oapi.UploadFiles400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "each item must include file and dest_path"}}, nil
		}

		// Ensure parent directory exists
		if err := os.MkdirAll(filepath.Dir(pu.destPath), 0o755); err != nil {
			log.Error("failed to create destination directories", "err", err, "path", pu.destPath)
			return oapi.UploadFiles500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to create destination directories"}}, nil
		}

		// Copy temp -> destination
		src, err := os.Open(pu.tempPath)
		if err != nil {
			log.Error("failed to open temporary file", "err", err)
			return oapi.UploadFiles500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "internal error"}}, nil
		}
		dst, err := os.OpenFile(pu.destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			src.Close()
			if errors.Is(err, os.ErrNotExist) {
				return oapi.UploadFiles404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "destination not found"}}, nil
			}
			log.Error("failed to open destination file", "err", err, "path", pu.destPath)
			return oapi.UploadFiles500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to open destination file"}}, nil
		}
		if _, err := io.Copy(dst, src); err != nil {
			src.Close()
			dst.Close()
			log.Error("failed to write destination file", "err", err)
			return oapi.UploadFiles500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to write destination file"}}, nil
		}
		_ = src.Close()
		if err := dst.Close(); err != nil {
			log.Error("failed to close destination file", "err", err)
			return oapi.UploadFiles500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "internal error"}}, nil
		}
	}

	return oapi.UploadFiles201Response{}, nil
}

func (s *ApiService) DownloadDirZip(ctx context.Context, request oapi.DownloadDirZipRequestObject) (oapi.DownloadDirZipResponseObject, error) {
	log := logger.FromContext(ctx)
	path := request.Params.Path
	if path == "" {
		return oapi.DownloadDirZip400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "path cannot be empty"}}, nil
	}

	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return oapi.DownloadDirZip404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "directory not found"}}, nil
		}
		log.Error("failed to stat path", "err", err, "path", path)
		return oapi.DownloadDirZip500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to stat path"}}, nil
	}
	if !info.IsDir() {
		return oapi.DownloadDirZip400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "path is not a directory"}}, nil
	}

	// Build zip in-memory to provide a single streaming response
	zipBytes, err := ziputil.ZipDir(path)
	if err != nil {
		// Add extra diagnostics for common failure causes
		// Check if directory is readable and walkable
		// We avoid heavy recursion here; just attempt to open directory and read one entry
		var readErr error
		f, oerr := os.Open(path)
		if oerr != nil {
			readErr = oerr
		} else {
			_, readErr = f.Readdir(1)
			_ = f.Close()
		}
		log.Error("failed to create zip archive", "err", err, "path", path, "read_probe_err", readErr)
		return oapi.DownloadDirZip500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to create zip"}}, nil
	}

	body := io.NopCloser(bytes.NewReader(zipBytes))
	return oapi.DownloadDirZip200ApplicationzipResponse{Body: body, ContentLength: int64(len(zipBytes))}, nil
}

func (s *ApiService) DownloadDirZstd(ctx context.Context, request oapi.DownloadDirZstdRequestObject) (oapi.DownloadDirZstdResponseObject, error) {
	log := logger.FromContext(ctx)
	path := request.Params.Path
	if path == "" {
		return oapi.DownloadDirZstd400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "path cannot be empty"}}, nil
	}

	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return oapi.DownloadDirZstd404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "directory not found"}}, nil
		}
		log.Error("failed to stat path", "err", err, "path", path)
		return oapi.DownloadDirZstd500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to stat path"}}, nil
	}
	if !info.IsDir() {
		return oapi.DownloadDirZstd400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "path is not a directory"}}, nil
	}

	// Determine compression level
	level := zstdutil.LevelDefault
	if request.Params.CompressionLevel != nil {
		switch *request.Params.CompressionLevel {
		case oapi.Fastest:
			level = zstdutil.LevelFastest
		case oapi.Better:
			level = zstdutil.LevelBetter
		case oapi.Best:
			level = zstdutil.LevelBest
		default:
			level = zstdutil.LevelDefault
		}
	}

	// Create streaming response using a pipe
	pr, pw := io.Pipe()

	go func() {
		defer pw.Close()
		if err := zstdutil.TarZstdDir(pw, path, level); err != nil {
			log.Error("failed to create tar.zst archive", "err", err, "path", path)
			pw.CloseWithError(err)
		}
	}()

	return oapi.DownloadDirZstd200ApplicationzstdResponse{Body: pr, ContentLength: 0}, nil
}

func (s *ApiService) UploadZstd(ctx context.Context, request oapi.UploadZstdRequestObject) (oapi.UploadZstdResponseObject, error) {
	log := logger.FromContext(ctx)

	if request.Body == nil {
		return oapi.UploadZstd400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body required"}}, nil
	}

	var archiveBuf bytes.Buffer
	var destPath string
	var stripComponents int
	var archiveReceived bool

	for {
		part, err := request.Body.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Error("failed to read form part", "err", err)
			return oapi.UploadZstd400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "failed to read form part"}}, nil
		}

		switch part.FormName() {
		case "archive":
			archiveReceived = true
			if _, err := io.Copy(&archiveBuf, part); err != nil {
				log.Error("failed to read archive data", "err", err)
				return oapi.UploadZstd400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "failed to read archive"}}, nil
			}
		case "dest_path":
			data, err := io.ReadAll(part)
			if err != nil {
				log.Error("failed to read dest_path", "err", err)
				return oapi.UploadZstd400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "failed to read dest_path"}}, nil
			}
			destPath = strings.TrimSpace(string(data))
			if destPath == "" || !filepath.IsAbs(destPath) {
				return oapi.UploadZstd400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "dest_path must be an absolute path"}}, nil
			}
		case "strip_components":
			data, err := io.ReadAll(part)
			if err != nil {
				log.Error("failed to read strip_components", "err", err)
				return oapi.UploadZstd400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "failed to read strip_components"}}, nil
			}
			if v, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && v >= 0 {
				stripComponents = v
			}
		default:
			return oapi.UploadZstd400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid form field: " + part.FormName()}}, nil
		}
	}

	if !archiveReceived || destPath == "" {
		return oapi.UploadZstd400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "archive and dest_path are required"}}, nil
	}

	if err := zstdutil.UntarZstd(bytes.NewReader(archiveBuf.Bytes()), destPath, stripComponents); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "illegal file path") {
			return oapi.UploadZstd400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid archive: path traversal detected"}}, nil
		}
		log.Error("failed to extract tar.zst archive", "err", err)
		return oapi.UploadZstd500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to extract archive"}}, nil
	}

	return oapi.UploadZstd201Response{}, nil
}
