package api

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"testing"

	oapi "github.com/onkernel/kernel-images/server/lib/oapi"
	"github.com/onkernel/kernel-images/server/lib/ziputil"
)

// TestWriteReadFile verifies that files can be written and read back successfully.
func TestWriteReadFile(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc := &ApiService{defaultRecorderID: "default"}

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.txt")
	content := "hello world"

	// Write the file
	if resp, err := svc.WriteFile(ctx, oapi.WriteFileRequestObject{
		Params: oapi.WriteFileParams{Path: filePath},
		Body:   strings.NewReader(content),
	}); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	} else {
		if _, ok := resp.(oapi.WriteFile201Response); !ok {
			t.Fatalf("unexpected response type from WriteFile: %T", resp)
		}
	}

	// Read the file
	readResp, err := svc.ReadFile(ctx, oapi.ReadFileRequestObject{Params: oapi.ReadFileParams{Path: filePath}})
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	r200, ok := readResp.(oapi.ReadFile200ApplicationoctetStreamResponse)
	if !ok {
		t.Fatalf("unexpected response type from ReadFile: %T", readResp)
	}
	data, _ := io.ReadAll(r200.Body)
	if got := string(data); got != content {
		t.Fatalf("ReadFile content mismatch: got %q want %q", got, content)
	}

	// (Download functionality removed)

	// Attempt to read non-existent file
	missingResp, err := svc.ReadFile(ctx, oapi.ReadFileRequestObject{Params: oapi.ReadFileParams{Path: filepath.Join(tmpDir, "missing.txt")}})
	if err != nil {
		t.Fatalf("ReadFile missing file returned error: %v", err)
	}
	if _, ok := missingResp.(oapi.ReadFile404JSONResponse); !ok {
		t.Fatalf("expected 404 response for missing file, got %T", missingResp)
	}

	// Attempt to write with empty path
	badResp, err := svc.WriteFile(ctx, oapi.WriteFileRequestObject{Params: oapi.WriteFileParams{Path: ""}, Body: strings.NewReader("data")})
	if err != nil {
		t.Fatalf("WriteFile bad path returned error: %v", err)
	}
	if _, ok := badResp.(oapi.WriteFile400JSONResponse); !ok {
		t.Fatalf("expected 400 response for empty path, got %T", badResp)
	}
}

// TestWriteFileAndWatch verifies WriteFile operation and filesystem watch event generation.
func TestWriteFileAndWatch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc := &ApiService{defaultRecorderID: "default", watches: make(map[string]*fsWatch)}

	// Prepare watch
	dir := t.TempDir()
	recursive := true
	startReq := oapi.StartFsWatchRequestObject{Body: &oapi.StartFsWatchRequest{Path: dir, Recursive: &recursive}}
	startResp, err := svc.StartFsWatch(ctx, startReq)
	if err != nil {
		t.Fatalf("StartFsWatch error: %v", err)
	}
	sr201, ok := startResp.(oapi.StartFsWatch201JSONResponse)
	if !ok {
		t.Fatalf("unexpected response type from StartFsWatch: %T", startResp)
	}
	if sr201.WatchId == nil {
		t.Fatalf("watch id nil")
	}
	watchID := *sr201.WatchId

	destPath := filepath.Join(dir, "write.txt")
	content := "write content"

	// Perform WriteFile to trigger watch events
	if resp, err := svc.WriteFile(ctx, oapi.WriteFileRequestObject{
		Params: oapi.WriteFileParams{Path: destPath},
		Body:   strings.NewReader(content),
	}); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	} else {
		if _, ok := resp.(oapi.WriteFile201Response); !ok {
			t.Fatalf("unexpected response type from WriteFile: %T", resp)
		}
	}

	// Verify file exists
	data, err := os.ReadFile(destPath)
	if err != nil || string(data) != content {
		t.Fatalf("written file mismatch: %v", err)
	}

	// Stream events (should at least receive one)
	streamReq := oapi.StreamFsEventsRequestObject{WatchId: watchID}
	streamResp, err := svc.StreamFsEvents(ctx, streamReq)
	if err != nil {
		t.Fatalf("StreamFsEvents error: %v", err)
	}
	st200, ok := streamResp.(oapi.StreamFsEvents200TexteventStreamResponse)
	if !ok {
		t.Fatalf("unexpected response type from StreamFsEvents: %T", streamResp)
	}

	reader := bufio.NewReader(st200.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("failed to read SSE line: %v", err)
	}
	if !strings.HasPrefix(line, "data: ") {
		t.Fatalf("unexpected SSE format: %s", line)
	}

	// Cleanup
	stopResp, err := svc.StopFsWatch(ctx, oapi.StopFsWatchRequestObject{WatchId: watchID})
	if err != nil {
		t.Fatalf("StopFsWatch error: %v", err)
	}
	if _, ok := stopResp.(oapi.StopFsWatch204Response); !ok {
		t.Fatalf("unexpected response type from StopFsWatch: %T", stopResp)
	}
}

// TestFileDirOperations covers the new filesystem management endpoints.
func TestFileDirOperations(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc := &ApiService{}

	tmp := t.TempDir()
	dirPath := filepath.Join(tmp, "mydir")

	// Create directory
	modeStr := "755"
	createReq := oapi.CreateDirectoryRequestObject{Body: &oapi.CreateDirectoryRequest{Path: dirPath, Mode: &modeStr}}
	if resp, err := svc.CreateDirectory(ctx, createReq); err != nil {
		t.Fatalf("CreateDirectory error: %v", err)
	} else {
		if _, ok := resp.(oapi.CreateDirectory201Response); !ok {
			t.Fatalf("unexpected response type from CreateDirectory: %T", resp)
		}
	}

	// Write a file inside the directory
	filePath := filepath.Join(dirPath, "file.txt")
	content := "data"
	if resp, err := svc.WriteFile(ctx, oapi.WriteFileRequestObject{Params: oapi.WriteFileParams{Path: filePath}, Body: strings.NewReader(content)}); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	} else if _, ok := resp.(oapi.WriteFile201Response); !ok {
		t.Fatalf("unexpected WriteFile resp type: %T", resp)
	}

	// List files
	listResp, err := svc.ListFiles(ctx, oapi.ListFilesRequestObject{Params: oapi.ListFilesParams{Path: dirPath}})
	if err != nil {
		t.Fatalf("ListFiles error: %v", err)
	}
	lf200, ok := listResp.(oapi.ListFiles200JSONResponse)
	if !ok {
		t.Fatalf("unexpected ListFiles resp type: %T", listResp)
	}
	if len(lf200) != 1 || lf200[0].Name != "file.txt" {
		t.Fatalf("ListFiles unexpected content: %+v", lf200)
	}

	// FileInfo
	fiResp, err := svc.FileInfo(ctx, oapi.FileInfoRequestObject{Params: oapi.FileInfoParams{Path: filePath}})
	if err != nil {
		t.Fatalf("FileInfo error: %v", err)
	}
	fi200, ok := fiResp.(oapi.FileInfo200JSONResponse)
	if !ok {
		t.Fatalf("unexpected FileInfo resp: %T", fiResp)
	}
	if fi200.Name != "file.txt" || fi200.SizeBytes == 0 {
		t.Fatalf("FileInfo unexpected: %+v", fi200)
	}

	// Move file
	newFilePath := filepath.Join(dirPath, "moved.txt")
	moveReq := oapi.MovePathRequestObject{Body: &oapi.MovePathRequest{SrcPath: filePath, DestPath: newFilePath}}
	if resp, err := svc.MovePath(ctx, moveReq); err != nil {
		t.Fatalf("MovePath error: %v", err)
	} else if _, ok := resp.(oapi.MovePath200Response); !ok {
		t.Fatalf("unexpected MovePath resp type: %T", resp)
	}

	// Set permissions
	chmodReq := oapi.SetFilePermissionsRequestObject{Body: &oapi.SetFilePermissionsRequest{Path: newFilePath, Mode: "600"}}
	if resp, err := svc.SetFilePermissions(ctx, chmodReq); err != nil {
		t.Fatalf("SetFilePermissions error: %v", err)
	} else if _, ok := resp.(oapi.SetFilePermissions200Response); !ok {
		t.Fatalf("unexpected SetFilePermissions resp: %T", resp)
	}

	// Delete file
	if resp, err := svc.DeleteFile(ctx, oapi.DeleteFileRequestObject{Body: &oapi.DeletePathRequest{Path: newFilePath}}); err != nil {
		t.Fatalf("DeleteFile error: %v", err)
	} else if _, ok := resp.(oapi.DeleteFile200Response); !ok {
		t.Fatalf("unexpected DeleteFile resp: %T", resp)
	}

	// Delete directory
	if resp, err := svc.DeleteDirectory(ctx, oapi.DeleteDirectoryRequestObject{Body: &oapi.DeletePathRequest{Path: dirPath}}); err != nil {
		t.Fatalf("DeleteDirectory error: %v", err)
	} else if _, ok := resp.(oapi.DeleteDirectory200Response); !ok {
		t.Fatalf("unexpected DeleteDirectory resp: %T", resp)
	}
}

// helper to build multipart form for uploadFiles
func buildUploadMultipart(t *testing.T, parts map[string]string, files map[string]string) *multipart.Reader {
	t.Helper()
	pr, pw := io.Pipe()
	mpw := multipart.NewWriter(pw)

	go func() {
		// write fields
		for name, val := range parts {
			_ = mpw.WriteField(name, val)
		}
		// write files (string content)
		for name, content := range files {
			fw, _ := mpw.CreateFormField(name)
			_, _ = io.Copy(fw, strings.NewReader(content))
		}
		mpw.Close()
		pw.Close()
	}()

	return multipart.NewReader(pr, mpw.Boundary())
}

// helper to build multipart for UploadZip with binary zip bytes
func buildUploadZipMultipart(t *testing.T, destPath string, zipBytes []byte) *multipart.Reader {
	t.Helper()
	pr, pw := io.Pipe()
	mpw := multipart.NewWriter(pw)

	go func() {
		// dest_path field
		if destPath != "" {
			_ = mpw.WriteField("dest_path", destPath)
		}
		// binary zip part
		if zipBytes != nil {
			// Use form field named zip_file; file vs field does not matter for our handler
			fw, _ := mpw.CreateFormFile("zip_file", "upload.zip")
			_, _ = fw.Write(zipBytes)
		}
		mpw.Close()
		pw.Close()
	}()

	return multipart.NewReader(pr, mpw.Boundary())
}

func TestUploadFilesSingle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := &ApiService{}

	tmp := t.TempDir()
	dest := filepath.Join(tmp, "single.txt")

	// single-file shorthand: file + dest_path
	reader := buildUploadMultipart(t,
		map[string]string{"dest_path": dest},
		map[string]string{"file": "hello"},
	)

	resp, err := svc.UploadFiles(ctx, oapi.UploadFilesRequestObject{Body: reader})
	if err != nil {
		t.Fatalf("UploadFiles error: %v", err)
	}
	if _, ok := resp.(oapi.UploadFiles201Response); !ok {
		t.Fatalf("unexpected response type: %T", resp)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "hello" {
		t.Fatalf("uploaded file mismatch: %v %q", err, string(data))
	}
}

func TestUploadFilesMultipleAndOutOfOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := &ApiService{}

	tmp := t.TempDir()
	d1 := filepath.Join(tmp, "a.txt")
	d2 := filepath.Join(tmp, "b.txt")

	// Use indexed fields with mixed ordering and bracket/dot styles
	parts := map[string]string{
		"files[1][dest_path]": d2,
		"files.0.dest_path":   d1,
	}
	files := map[string]string{
		"files[1][file]": "two",
		"files.0.file":   "one",
	}
	reader := buildUploadMultipart(t, parts, files)

	resp, err := svc.UploadFiles(ctx, oapi.UploadFilesRequestObject{Body: reader})
	if err != nil {
		t.Fatalf("UploadFiles error: %v", err)
	}
	if _, ok := resp.(oapi.UploadFiles201Response); !ok {
		t.Fatalf("unexpected response type: %T", resp)
	}

	if b, _ := os.ReadFile(d1); string(b) != "one" {
		t.Fatalf("d1 mismatch: %q", string(b))
	}
	if b, _ := os.ReadFile(d2); string(b) != "two" {
		t.Fatalf("d2 mismatch: %q", string(b))
	}
}

func TestUploadFilesCommaFormat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := &ApiService{}

	tmp := t.TempDir()
	dest := filepath.Join(tmp, "comma.txt")

	// SDK "comma" array format: files.dest_path, files.file (no index)
	reader := buildUploadMultipart(t,
		map[string]string{"files.dest_path": dest},
		map[string]string{"files.file": "hello comma"},
	)

	resp, err := svc.UploadFiles(ctx, oapi.UploadFilesRequestObject{Body: reader})
	if err != nil {
		t.Fatalf("UploadFiles error: %v", err)
	}
	if _, ok := resp.(oapi.UploadFiles201Response); !ok {
		t.Fatalf("unexpected response type: %T", resp)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "hello comma" {
		t.Fatalf("uploaded file mismatch: %v %q", err, string(data))
	}
}

func TestUploadZipSuccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := &ApiService{}

	// Create a source directory with content
	srcDir := t.TempDir()
	nested := filepath.Join(srcDir, "dir", "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	filePath := filepath.Join(nested, "a.txt")
	if err := os.WriteFile(filePath, []byte("hello-zip"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Zip the directory
	zipBytes, err := ziputil.ZipDir(srcDir)
	if err != nil {
		t.Fatalf("ZipDir error: %v", err)
	}

	// Destination directory for extraction
	destDir := t.TempDir()

	reader := buildUploadZipMultipart(t, destDir, zipBytes)
	resp, err := svc.UploadZip(ctx, oapi.UploadZipRequestObject{Body: reader})
	if err != nil {
		t.Fatalf("UploadZip error: %v", err)
	}
	if _, ok := resp.(oapi.UploadZip201Response); !ok {
		t.Fatalf("unexpected UploadZip resp type: %T", resp)
	}

	// Verify extracted content exists
	extracted := filepath.Join(destDir, "dir", "sub", "a.txt")
	data, err := os.ReadFile(extracted)
	if err != nil {
		t.Fatalf("read extracted: %v", err)
	}
	if string(data) != "hello-zip" {
		t.Fatalf("extracted content mismatch: %q", string(data))
	}
}

func TestUploadZipTraversalBlocked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := &ApiService{}

	// Build a malicious zip with a path traversal entry
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	fh, _ := zw.Create("../evil.txt")
	_, _ = fh.Write([]byte("pwned"))
	_ = zw.Close()

	destDir := t.TempDir()
	reader := buildUploadZipMultipart(t, destDir, buf.Bytes())
	resp, err := svc.UploadZip(ctx, oapi.UploadZipRequestObject{Body: reader})
	if err != nil {
		t.Fatalf("UploadZip error: %v", err)
	}
	if _, ok := resp.(oapi.UploadZip400JSONResponse); !ok {
		t.Fatalf("expected 400 for traversal, got %T", resp)
	}
	if _, err := os.Stat(filepath.Join(destDir, "evil.txt")); err == nil {
		t.Fatalf("traversal file unexpectedly created")
	}
}

func TestUploadZipValidationErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := &ApiService{}

	// Missing dest_path
	reader1 := func() *multipart.Reader {
		pr, pw := io.Pipe()
		mpw := multipart.NewWriter(pw)
		go func() {
			fw, _ := mpw.CreateFormFile("zip_file", "z.zip")
			_, _ = fw.Write([]byte("not-a-zip"))
			mpw.Close()
			pw.Close()
		}()
		return multipart.NewReader(pr, mpw.Boundary())
	}()
	resp1, err := svc.UploadZip(ctx, oapi.UploadZipRequestObject{Body: reader1})
	if err != nil {
		t.Fatalf("UploadZip error: %v", err)
	}
	if _, ok := resp1.(oapi.UploadZip400JSONResponse); !ok {
		t.Fatalf("expected 400 for missing dest_path, got %T", resp1)
	}

	// Missing zip_file
	destDir := t.TempDir()
	reader2 := func() *multipart.Reader {
		pr, pw := io.Pipe()
		mpw := multipart.NewWriter(pw)
		go func() {
			_ = mpw.WriteField("dest_path", destDir)
			mpw.Close()
			pw.Close()
		}()
		return multipart.NewReader(pr, mpw.Boundary())
	}()
	resp2, err := svc.UploadZip(ctx, oapi.UploadZipRequestObject{Body: reader2})
	if err != nil {
		t.Fatalf("UploadZip error: %v", err)
	}
	if _, ok := resp2.(oapi.UploadZip400JSONResponse); !ok {
		t.Fatalf("expected 400 for missing zip_file, got %T", resp2)
	}
}

func TestDownloadDirZipSuccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := &ApiService{}

	// Prepare a directory with nested content
	root := t.TempDir()
	nested := filepath.Join(root, "dir", "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f1 := filepath.Join(root, "top.txt")
	f2 := filepath.Join(nested, "a.txt")
	if err := os.WriteFile(f1, []byte("top"), 0o644); err != nil {
		t.Fatalf("write top: %v", err)
	}
	if err := os.WriteFile(f2, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write nested: %v", err)
	}

	resp, err := svc.DownloadDirZip(ctx, oapi.DownloadDirZipRequestObject{Params: oapi.DownloadDirZipParams{Path: root}})
	if err != nil {
		t.Fatalf("DownloadDirZip error: %v", err)
	}
	r200, ok := resp.(oapi.DownloadDirZip200ApplicationzipResponse)
	if !ok {
		t.Fatalf("unexpected response type: %T", resp)
	}
	data, err := io.ReadAll(r200.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}

	// Expect both files present with paths relative to root
	want := map[string]string{
		"top.txt":       "top",
		"dir/sub/a.txt": "hello",
	}
	found := map[string]bool{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if _, ok := want[f.Name]; ok {
			rc, err := f.Open()
			if err != nil {
				t.Fatalf("open zip entry: %v", err)
			}
			b, _ := io.ReadAll(rc)
			rc.Close()
			if string(b) != want[f.Name] {
				t.Fatalf("content mismatch for %s: %q", f.Name, string(b))
			}
			found[f.Name] = true
		}
	}
	for k := range want {
		if !found[k] {
			t.Fatalf("missing zip entry: %s", k)
		}
	}
}

func TestDownloadDirZipErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := &ApiService{}

	// Empty path
	if resp, err := svc.DownloadDirZip(ctx, oapi.DownloadDirZipRequestObject{Params: oapi.DownloadDirZipParams{Path: ""}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	} else if _, ok := resp.(oapi.DownloadDirZip400JSONResponse); !ok {
		t.Fatalf("expected 400 for empty path, got %T", resp)
	}

	// Non-existent path
	missing := filepath.Join(t.TempDir(), "nope")
	if resp, err := svc.DownloadDirZip(ctx, oapi.DownloadDirZipRequestObject{Params: oapi.DownloadDirZipParams{Path: missing}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	} else if _, ok := resp.(oapi.DownloadDirZip404JSONResponse); !ok {
		t.Fatalf("expected 404 for missing dir, got %T", resp)
	}

	// Path is a file, not a directory
	tmp := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(tmp, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if resp, err := svc.DownloadDirZip(ctx, oapi.DownloadDirZipRequestObject{Params: oapi.DownloadDirZipParams{Path: tmp}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	} else if _, ok := resp.(oapi.DownloadDirZip400JSONResponse); !ok {
		t.Fatalf("expected 400 for file path, got %T", resp)
	}
}
