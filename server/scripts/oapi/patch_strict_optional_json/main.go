// Patch strict-handler JSON decode and response tags after oapi-codegen:
// 1) Allow empty request bodies (io.EOF) for optional-body JSON endpoints.
// 2) Restore omitempty on pointer JSON tags that affect response shape.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

func main() {
	filePath := flag.String("file", "", "path to oapi.go to rewrite")
	flag.Parse()
	if *filePath == "" {
		log.Fatal("usage: -file=<path>")
	}
	path := *filePath

	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read %s: %v", path, err)
	}
	s := string(data)

	s = ensureErrorsImport(s)

	replacements := []struct{ old, new string }{
		{
			old: `	var body TakeScreenshotJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		sh.options.RequestErrorHandlerFunc(w, r, fmt.Errorf("can't decode JSON body: %w", err))
		return
	}
	request.Body = &body`,
			new: `	var body TakeScreenshotJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		if !errors.Is(err, io.EOF) {
			sh.options.RequestErrorHandlerFunc(w, r, fmt.Errorf("can't decode JSON body: %w", err))
			return
		}
	} else {
		request.Body = &body
	}`,
		},
		{
			old: `	var body DeleteRecordingJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		sh.options.RequestErrorHandlerFunc(w, r, fmt.Errorf("can't decode JSON body: %w", err))
		return
	}
	request.Body = &body`,
			new: `	var body DeleteRecordingJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		if !errors.Is(err, io.EOF) {
			sh.options.RequestErrorHandlerFunc(w, r, fmt.Errorf("can't decode JSON body: %w", err))
			return
		}
	} else {
		request.Body = &body
	}`,
		},
		{
			old: `	var body StartRecordingJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		sh.options.RequestErrorHandlerFunc(w, r, fmt.Errorf("can't decode JSON body: %w", err))
		return
	}
	request.Body = &body`,
			new: `	var body StartRecordingJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		if !errors.Is(err, io.EOF) {
			sh.options.RequestErrorHandlerFunc(w, r, fmt.Errorf("can't decode JSON body: %w", err))
			return
		}
	} else {
		request.Body = &body
	}`,
		},
		{
			old: `	var body StopRecordingJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		sh.options.RequestErrorHandlerFunc(w, r, fmt.Errorf("can't decode JSON body: %w", err))
		return
	}
	request.Body = &body`,
			new: `	var body StopRecordingJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		if !errors.Is(err, io.EOF) {
			sh.options.RequestErrorHandlerFunc(w, r, fmt.Errorf("can't decode JSON body: %w", err))
			return
		}
	} else {
		request.Body = &body
	}`,
		},
	}

	for _, r := range replacements {
		if !strings.Contains(s, r.old) {
			log.Fatalf("expected block not found in %s (codegen output may have changed)", path)
		}
		count := strings.Count(s, r.old)
		if count != 1 {
			log.Fatalf("expected exactly 1 occurrence of a decode block, found %d", count)
		}
		s = strings.Replace(s, r.old, r.new, 1)
	}

	tagFixesAll := []struct{ old, new string }{
		{`AsUser *string ` + "`json:\"as_user\"`", `AsUser *string ` + "`json:\"as_user,omitempty\"`"},
		{`Cwd *string ` + "`json:\"cwd\"`", `Cwd *string ` + "`json:\"cwd,omitempty\"`"},
		{`TimeoutSec *int ` + "`json:\"timeout_sec\"`", `TimeoutSec *int ` + "`json:\"timeout_sec,omitempty\"`"},
	}
	for _, tf := range tagFixesAll {
		if c := strings.Count(s, tf.old); c != 2 {
			log.Fatalf("expected exactly 2 occurrences of tag %q, found %d", tf.old, c)
		}
		s = strings.ReplaceAll(s, tf.old, tf.new)
	}
	tagFixesOnce := []struct{ old, new string }{
		{`ExitCode *int ` + "`json:\"exit_code\"`", `ExitCode *int ` + "`json:\"exit_code,omitempty\"`"},
		{`FinishedAt  *time.Time ` + "`json:\"finished_at\"`", `FinishedAt  *time.Time ` + "`json:\"finished_at,omitempty\"`"},
		{`StartedAt *time.Time ` + "`json:\"started_at\"`", `StartedAt *time.Time ` + "`json:\"started_at,omitempty\"`"},
	}
	for _, tf := range tagFixesOnce {
		if c := strings.Count(s, tf.old); c != 1 {
			log.Fatalf("expected exactly 1 occurrence of tag %q, found %d", tf.old, c)
		}
		s = strings.Replace(s, tf.old, tf.new, 1)
	}

	if err := os.WriteFile(path, []byte(s), 0644); err != nil {
		log.Fatalf("write %s: %v", path, err)
	}
	fmt.Printf("✓ strict optional JSON + omitempty patch applied to %s\n", path)
}

func ensureErrorsImport(s string) string {
	if strings.Contains(s, "\n\t\"errors\"\n") {
		return s
	}
	// Insert after "context"
	const anchor = "\t\"context\"\n"
	if !strings.Contains(s, anchor) {
		log.Fatal("could not find import anchor for errors package")
	}
	return strings.Replace(s, anchor, anchor+"\t\"errors\"\n", 1)
}
