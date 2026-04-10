package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
)

const oldBlock = `
	w.Header().Set("X-SSE-Content-Type", fmt.Sprint(response.Headers.XSSEContentType))
	w.WriteHeader(200)

	if closer, ok := response.Body.(io.ReadCloser); ok {
		defer closer.Close()
	}
	_, err := io.Copy(w, response.Body)
	return err`

const newBlock = `
	w.Header().Set("X-SSE-Content-Type", fmt.Sprint(response.Headers.XSSEContentType))
	w.WriteHeader(200)

	if closer, ok := response.Body.(io.ReadCloser); ok {
		defer closer.Close()
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		// If w doesn't support flushing, might as well use io.Copy
		_, err := io.Copy(w, response.Body)
		return err
	}

	// Use a buffer for efficient copying and flushing
	buf := make([]byte, 4096) // text/event-stream are usually very small messages
	for {
		n, err := response.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			flusher.Flush() // Flush after each write
		}
		if err != nil {
			if err == io.EOF {
				return nil // End of file, no error
			}
			return err
		}
	}`

func main() {
	filePath := flag.String("file", "", "path to oapi.go to rewrite")
	expectedReplacements := flag.Int("expected-replacements", 0, "expected number of SSE code blocks to replace")
	flag.Parse()

	if *filePath == "" || *expectedReplacements <= 0 {
		log.Fatal("usage: -file=<path> -expected-replacements=<count>")
	}
	path := *filePath

	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("fix_sse: failed to read %s: %v", path, err)
	}

	// count occurrences of the old block before replacement
	occurrences := bytes.Count(data, []byte(oldBlock))
	if occurrences != *expectedReplacements {
		log.Fatalf("expected exactly %d SSE code blocks to replace, found %d in %s", *expectedReplacements, occurrences, path)
	}

	updated := bytes.ReplaceAll(data, []byte(oldBlock), []byte(newBlock))
	if bytes.Equal(data, updated) {
		log.Fatalf("no changes made to %s - expected to find and replace SSE code blocks", path)
	}

	if err := os.WriteFile(path, updated, 0644); err != nil {
		log.Fatalf("failed to write %s: %v", path, err)
	}
	fmt.Printf("✓ SSE flush fix applied to %s\n", path)
}
