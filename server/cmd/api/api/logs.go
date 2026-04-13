package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// LogsStream implements Server-Sent Events log streaming.
// (GET /logs/stream)
func (s *ApiService) LogsStream(ctx context.Context, request oapi.LogsStreamRequestObject) (oapi.LogsStreamResponseObject, error) {
	// Only path-based streaming is implemented. Supervisor streaming can be added later.
	src := string(request.Params.Source)
	follow := true
	if request.Params.Follow != nil {
		follow = *request.Params.Follow
	}

	var logPath string
	if src == "path" {
		if request.Params.Path != nil {
			logPath = *request.Params.Path
		}
	}

	pr, pw := io.Pipe()

	go func() {
		defer pw.Close()

		if logPath == "" || !filepath.IsAbs(logPath) {
			_ = writeSSELogEvent(pw, oapi.LogEvent{Timestamp: time.Now(), Message: "logs source not available"})
			return
		}

		f, err := os.Open(logPath)
		if err != nil {
			_ = writeSSELogEvent(pw, oapi.LogEvent{Timestamp: time.Now(), Message: "failed to open log path"})
			return
		}
		defer f.Close()

		var offset int64 = 0
		if follow {
			if st, err := f.Stat(); err == nil {
				offset = st.Size()
			}
		}

		var remainder []byte
		buf := make([]byte, 16*1024)

		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				st, err := f.Stat()
				if err != nil {
					return
				}
				size := st.Size()
				if size < offset {
					offset = size
				}
				if size == offset {
					continue
				}
				toRead := size - offset
				for toRead > 0 {
					if int64(len(buf)) > toRead {
						buf = buf[:toRead]
					}
					n, err := f.ReadAt(buf, offset)
					if n > 0 {
						offset += int64(n)
						toRead -= int64(n)
						chunk := append(remainder, buf[:n]...)
						for {
							if i := bytes.IndexByte(chunk, '\n'); i >= 0 {
								line := chunk[:i]
								if len(line) > 0 {
									_ = writeSSELogEvent(pw, oapi.LogEvent{Timestamp: time.Now(), Message: string(line)})
								}
								chunk = chunk[i+1:]
								continue
							}
							break
						}
						remainder = chunk
					}
					if err != nil {
						break
					}
				}
			}
		}
	}()

	headers := oapi.LogsStream200ResponseHeaders{XSSEContentType: "application/json"}
	return oapi.LogsStream200TexteventStreamResponse{Body: pr, Headers: headers, ContentLength: 0}, nil
}

func writeSSELogEvent(w io.Writer, ev oapi.LogEvent) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(ev); err != nil {
		return err
	}
	line := bytes.TrimRight(buf.Bytes(), "\n")
	if _, err := w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := w.Write(line); err != nil {
		return err
	}
	if _, err := w.Write([]byte("\n\n")); err != nil {
		return err
	}
	return nil
}
