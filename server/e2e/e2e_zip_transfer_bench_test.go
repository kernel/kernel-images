package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os/exec"
	"testing"
	"time"

	instanceoapi "github.com/onkernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

// TestZipTransferTiming measures the time to download a directory as a zip and re-upload it.
// This is useful for understanding the performance characteristics of the zip transfer endpoints
// and evaluating whether alternative compression methods (like zstd) would be beneficial.
//
// Run with: go test -v -run TestZipTransferTiming ./e2e/...
func TestZipTransferTiming(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}

	env := map[string]string{
		"WIDTH":  "1024",
		"HEIGHT": "768",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Start container with dynamic ports
	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{Env: env}), "failed to start container")
	defer c.Stop(ctx)

	t.Log("Waiting for API...")
	require.NoError(t, c.WaitReady(ctx), "api not ready")

	client, err := c.APIClient()
	require.NoError(t, err, "failed to create API client")

	// First, let's populate user-data with some content by navigating to a page
	// This ensures we have a realistic directory to transfer
	t.Logf("Populating user-data by browsing...")
	populateStart := time.Now()
	err = populateUserData(ctx, client)
	require.NoError(t, err, "failed to populate user-data")
	t.Logf("User-data population took %dms", time.Since(populateStart).Milliseconds())

	// Get initial directory size for reference
	dirSize, fileCount, err := getDirStats(ctx, client, "/home/kernel/user-data")
	require.NoError(t, err, "failed to get dir stats")
	t.Logf("Directory stats: %d files, ~%d KB", fileCount, dirSize/1024)

	const iterations = 3
	var downloadTimes, uploadTimes []time.Duration
	var zipSizes []int64

	for i := 0; i < iterations; i++ {
		t.Logf("\n--- Iteration %d ---", i+1)

		// Download /home/kernel/user-data as zip
		downloadStart := time.Now()
		zipData, err := downloadDirAsZip(ctx, client, "/home/kernel/user-data")
		downloadTime := time.Since(downloadStart)
		require.NoError(t, err, "download failed")
		downloadTimes = append(downloadTimes, downloadTime)
		zipSizes = append(zipSizes, int64(len(zipData)))

		t.Logf("  Download: %dms (zip size: %d KB, compression ratio: %.1f%%)",
			downloadTime.Milliseconds(),
			len(zipData)/1024,
			float64(len(zipData))/float64(dirSize)*100)

		// Upload to a different location
		destPath := fmt.Sprintf("/tmp/upload-test-%d", i)
		uploadStart := time.Now()
		err = uploadZip(ctx, client, zipData, destPath)
		uploadTime := time.Since(uploadStart)
		require.NoError(t, err, "upload failed")
		uploadTimes = append(uploadTimes, uploadTime)

		t.Logf("  Upload:   %dms", uploadTime.Milliseconds())
		t.Logf("  Total:    %dms", (downloadTime + uploadTime).Milliseconds())
	}

	// Calculate averages
	avgDownload := avg(downloadTimes)
	avgUpload := avg(uploadTimes)
	avgZipSize := avgInt64(zipSizes)

	t.Logf("\n=== Zip Transfer Results (%d iterations) ===", iterations)
	t.Logf("  Directory size:       ~%d KB (%d files)", dirSize/1024, fileCount)
	t.Logf("  Average zip size:     %d KB (%.1f%% of original)",
		avgZipSize/1024,
		float64(avgZipSize)/float64(dirSize)*100)
	t.Logf("  Average download:     %dms", avgDownload.Milliseconds())
	t.Logf("  Average upload:       %dms", avgUpload.Milliseconds())
	t.Logf("  Average round-trip:   %dms", (avgDownload + avgUpload).Milliseconds())
	t.Logf("  Download throughput:  %.1f MB/s (uncompressed)", float64(dirSize)/1024/1024/avgDownload.Seconds())
	t.Logf("  Upload throughput:    %.1f MB/s (uncompressed)", float64(dirSize)/1024/1024/avgUpload.Seconds())
}

// populateUserData creates some realistic content in the user-data directory
// by executing a playwright script that navigates to a page.
func populateUserData(ctx context.Context, client *instanceoapi.ClientWithResponses) error {
	// Navigate to example.com to generate some browser state
	code := `
		await page.goto('https://example.com');
		await page.waitForTimeout(500);
		// Visit another page to generate more cache/state
		await page.goto('https://www.google.com');
		await page.waitForTimeout(500);
		return 'done';
	`
	req := instanceoapi.ExecutePlaywrightCodeJSONRequestBody{Code: code}
	rsp, err := client.ExecutePlaywrightCodeWithResponse(ctx, req)
	if err != nil {
		return fmt.Errorf("playwright execute failed: %w", err)
	}
	if rsp.StatusCode() != http.StatusOK {
		return fmt.Errorf("playwright execute returned %d: %s", rsp.StatusCode(), string(rsp.Body))
	}
	if rsp.JSON200 != nil && !rsp.JSON200.Success {
		errMsg := "unknown error"
		if rsp.JSON200.Error != nil {
			errMsg = *rsp.JSON200.Error
		}
		return fmt.Errorf("playwright execution failed: %s", errMsg)
	}
	return nil
}

// getDirStats returns approximate size and file count of a directory
func getDirStats(ctx context.Context, client *instanceoapi.ClientWithResponses, path string) (int64, int, error) {
	// Use du command via process exec to get accurate size
	args := []string{"-sb", path}
	req := instanceoapi.ProcessExecJSONRequestBody{
		Command: "du",
		Args:    &args,
	}
	rsp, err := client.ProcessExecWithResponse(ctx, req)
	if err != nil {
		return 0, 0, err
	}
	if rsp.JSON200 == nil || (rsp.JSON200.ExitCode != nil && *rsp.JSON200.ExitCode != 0) {
		return 0, 0, fmt.Errorf("du command failed")
	}

	var size int64
	if rsp.JSON200.StdoutB64 != nil {
		// Parse du output: "SIZE\tPATH"
		stdout := decodeBase64(*rsp.JSON200.StdoutB64)
		fmt.Sscanf(stdout, "%d", &size)
	}

	// Get file count
	args2 := []string{path, "-type", "f"}
	req2 := instanceoapi.ProcessExecJSONRequestBody{
		Command: "find",
		Args:    &args2,
	}
	rsp2, err := client.ProcessExecWithResponse(ctx, req2)
	if err != nil {
		return size, 0, err
	}

	fileCount := 0
	if rsp2.JSON200 != nil && rsp2.JSON200.StdoutB64 != nil {
		stdout := decodeBase64(*rsp2.JSON200.StdoutB64)
		// Count lines
		for _, c := range stdout {
			if c == '\n' {
				fileCount++
			}
		}
	}

	return size, fileCount, nil
}

// downloadDirAsZip downloads a directory as a zip file
func downloadDirAsZip(ctx context.Context, client *instanceoapi.ClientWithResponses, path string) ([]byte, error) {
	params := &instanceoapi.DownloadDirZipParams{Path: path}
	rsp, err := client.DownloadDirZipWithResponse(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("download request failed: %w", err)
	}
	if rsp.StatusCode() != http.StatusOK {
		return nil, fmt.Errorf("download returned %d: %s", rsp.StatusCode(), string(rsp.Body))
	}
	return rsp.Body, nil
}

// uploadZip uploads a zip file to the specified destination
func uploadZip(ctx context.Context, client *instanceoapi.ClientWithResponses, zipData []byte, destPath string) error {
	// Create multipart form
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	// Add zip_file
	part, err := writer.CreateFormFile("zip_file", "archive.zip")
	if err != nil {
		return fmt.Errorf("create form file failed: %w", err)
	}
	if _, err := io.Copy(part, bytes.NewReader(zipData)); err != nil {
		return fmt.Errorf("copy zip data failed: %w", err)
	}

	// Add dest_path
	if err := writer.WriteField("dest_path", destPath); err != nil {
		return fmt.Errorf("write dest_path failed: %w", err)
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("close writer failed: %w", err)
	}

	rsp, err := client.UploadZipWithBodyWithResponse(ctx, writer.FormDataContentType(), &body)
	if err != nil {
		return fmt.Errorf("upload request failed: %w", err)
	}
	if rsp.StatusCode() != http.StatusCreated {
		return fmt.Errorf("upload returned %d: %s", rsp.StatusCode(), string(rsp.Body))
	}
	return nil
}

func decodeBase64(s string) string {
	b, _ := base64.StdEncoding.DecodeString(s)
	return string(b)
}

func avgInt64(vals []int64) int64 {
	if len(vals) == 0 {
		return 0
	}
	var total int64
	for _, v := range vals {
		total += v
	}
	return total / int64(len(vals))
}

// TestZstdTransferTiming measures the time to download a directory as a tar.zst and re-upload it.
// This compares performance against the zip endpoint baseline.
//
// Run with: go test -v -run TestZstdTransferTiming ./e2e/...
func TestZstdTransferTiming(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}

	env := map[string]string{
		"WIDTH":  "1024",
		"HEIGHT": "768",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Start container with dynamic ports
	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{Env: env}), "failed to start container")
	defer c.Stop(ctx)

	t.Log("Waiting for API...")
	require.NoError(t, c.WaitReady(ctx), "api not ready")

	client, err := c.APIClient()
	require.NoError(t, err, "failed to create API client")

	// Populate user-data with some content
	t.Logf("Populating user-data by browsing...")
	populateStart := time.Now()
	err = populateUserData(ctx, client)
	require.NoError(t, err, "failed to populate user-data")
	t.Logf("User-data population took %dms", time.Since(populateStart).Milliseconds())

	// Get directory stats for reference
	dirSize, fileCount, err := getDirStats(ctx, client, "/home/kernel/user-data")
	require.NoError(t, err, "failed to get dir stats")
	t.Logf("Directory stats: %d files, ~%d KB", fileCount, dirSize/1024)

	const iterations = 3
	levels := []string{"fastest", "default", "better"}

	for _, level := range levels {
		t.Logf("\n=== Zstd Level: %s ===", level)
		var downloadTimes, uploadTimes []time.Duration
		var archiveSizes []int64

		for i := 0; i < iterations; i++ {
			t.Logf("--- Iteration %d ---", i+1)

			// Download as zstd
			downloadStart := time.Now()
			zstdData, err := downloadDirAsZstd(ctx, client, "/home/kernel/user-data", level)
			downloadTime := time.Since(downloadStart)
			require.NoError(t, err, "zstd download failed")
			downloadTimes = append(downloadTimes, downloadTime)
			archiveSizes = append(archiveSizes, int64(len(zstdData)))

			t.Logf("  Download: %dms (size: %d KB, ratio: %.1f%%)",
				downloadTime.Milliseconds(),
				len(zstdData)/1024,
				float64(len(zstdData))/float64(dirSize)*100)

			// Upload to a different location
			destPath := fmt.Sprintf("/tmp/zstd-upload-%s-%d", level, i)
			uploadStart := time.Now()
			err = uploadZstd(ctx, client, zstdData, destPath, 0)
			uploadTime := time.Since(uploadStart)
			require.NoError(t, err, "zstd upload failed")
			uploadTimes = append(uploadTimes, uploadTime)

			t.Logf("  Upload:   %dms", uploadTime.Milliseconds())
			t.Logf("  Total:    %dms", (downloadTime + uploadTime).Milliseconds())
		}

		// Calculate averages
		avgDownload := avg(downloadTimes)
		avgUpload := avg(uploadTimes)
		avgArchiveSize := avgInt64(archiveSizes)

		t.Logf("\n--- Level %s Results ---", level)
		t.Logf("  Average archive size: %d KB (%.1f%% of original)",
			avgArchiveSize/1024,
			float64(avgArchiveSize)/float64(dirSize)*100)
		t.Logf("  Average download:     %dms", avgDownload.Milliseconds())
		t.Logf("  Average upload:       %dms", avgUpload.Milliseconds())
		t.Logf("  Average round-trip:   %dms", (avgDownload + avgUpload).Milliseconds())
		t.Logf("  Download throughput:  %.1f MB/s (uncompressed)", float64(dirSize)/1024/1024/avgDownload.Seconds())
		t.Logf("  Upload throughput:    %.1f MB/s (uncompressed)", float64(dirSize)/1024/1024/avgUpload.Seconds())
	}
}

// downloadDirAsZstd downloads a directory as a tar.zst archive
func downloadDirAsZstd(ctx context.Context, client *instanceoapi.ClientWithResponses, path string, level string) ([]byte, error) {
	compressionLevel := instanceoapi.DownloadDirZstdParamsCompressionLevel(level)
	params := &instanceoapi.DownloadDirZstdParams{
		Path:             path,
		CompressionLevel: &compressionLevel,
	}
	rsp, err := client.DownloadDirZstdWithResponse(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("download request failed: %w", err)
	}
	if rsp.StatusCode() != http.StatusOK {
		return nil, fmt.Errorf("download returned %d: %s", rsp.StatusCode(), string(rsp.Body))
	}
	return rsp.Body, nil
}

// uploadZstd uploads a tar.zst archive to the specified destination
func uploadZstd(ctx context.Context, client *instanceoapi.ClientWithResponses, archiveData []byte, destPath string, stripComponents int) error {
	// Create multipart form
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	// Add archive file
	part, err := writer.CreateFormFile("archive", "archive.tar.zst")
	if err != nil {
		return fmt.Errorf("create form file failed: %w", err)
	}
	if _, err := io.Copy(part, bytes.NewReader(archiveData)); err != nil {
		return fmt.Errorf("copy archive data failed: %w", err)
	}

	// Add dest_path
	if err := writer.WriteField("dest_path", destPath); err != nil {
		return fmt.Errorf("write dest_path failed: %w", err)
	}

	// Add strip_components if non-zero
	if stripComponents > 0 {
		if err := writer.WriteField("strip_components", fmt.Sprintf("%d", stripComponents)); err != nil {
			return fmt.Errorf("write strip_components failed: %w", err)
		}
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("close writer failed: %w", err)
	}

	rsp, err := client.UploadZstdWithBodyWithResponse(ctx, writer.FormDataContentType(), &body)
	if err != nil {
		return fmt.Errorf("upload request failed: %w", err)
	}
	if rsp.StatusCode() != http.StatusCreated {
		return fmt.Errorf("upload returned %d: %s", rsp.StatusCode(), string(rsp.Body))
	}
	return nil
}

// TestZipVsZstdComparison runs a direct comparison of zip and zstd endpoints
//
// Run with: go test -v -run TestZipVsZstdComparison ./e2e/...
func TestZipVsZstdComparison(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}

	env := map[string]string{
		"WIDTH":  "1024",
		"HEIGHT": "768",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Start container with dynamic ports
	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{Env: env}), "failed to start container")
	defer c.Stop(ctx)

	t.Log("Waiting for API...")
	require.NoError(t, c.WaitReady(ctx), "api not ready")

	client, err := c.APIClient()
	require.NoError(t, err, "failed to create API client")

	// Populate user-data
	t.Logf("Populating user-data by browsing...")
	err = populateUserData(ctx, client)
	require.NoError(t, err, "failed to populate user-data")

	// Get directory stats
	dirSize, fileCount, err := getDirStats(ctx, client, "/home/kernel/user-data")
	require.NoError(t, err, "failed to get dir stats")
	t.Logf("Directory stats: %d files, ~%d KB\n", fileCount, dirSize/1024)

	const iterations = 3
	type result struct {
		name        string
		downloadMs  int64
		uploadMs    int64
		archiveSize int64
	}
	var results []result

	// Test Zip
	{
		var downloadTotal, uploadTotal, sizeTotal int64
		for i := 0; i < iterations; i++ {
			start := time.Now()
			zipData, err := downloadDirAsZip(ctx, client, "/home/kernel/user-data")
			downloadTime := time.Since(start).Milliseconds()
			require.NoError(t, err)

			start = time.Now()
			err = uploadZip(ctx, client, zipData, fmt.Sprintf("/tmp/zip-test-%d", i))
			uploadTime := time.Since(start).Milliseconds()
			require.NoError(t, err)

			downloadTotal += downloadTime
			uploadTotal += uploadTime
			sizeTotal += int64(len(zipData))
		}
		results = append(results, result{
			name:        "Zip",
			downloadMs:  downloadTotal / iterations,
			uploadMs:    uploadTotal / iterations,
			archiveSize: sizeTotal / iterations,
		})
	}

	// Test Zstd (fastest)
	{
		var downloadTotal, uploadTotal, sizeTotal int64
		for i := 0; i < iterations; i++ {
			start := time.Now()
			zstdData, err := downloadDirAsZstd(ctx, client, "/home/kernel/user-data", "fastest")
			downloadTime := time.Since(start).Milliseconds()
			require.NoError(t, err)

			start = time.Now()
			err = uploadZstd(ctx, client, zstdData, fmt.Sprintf("/tmp/zstd-fastest-%d", i), 0)
			uploadTime := time.Since(start).Milliseconds()
			require.NoError(t, err)

			downloadTotal += downloadTime
			uploadTotal += uploadTime
			sizeTotal += int64(len(zstdData))
		}
		results = append(results, result{
			name:        "Zstd (fastest)",
			downloadMs:  downloadTotal / iterations,
			uploadMs:    uploadTotal / iterations,
			archiveSize: sizeTotal / iterations,
		})
	}

	// Test Zstd (default)
	{
		var downloadTotal, uploadTotal, sizeTotal int64
		for i := 0; i < iterations; i++ {
			start := time.Now()
			zstdData, err := downloadDirAsZstd(ctx, client, "/home/kernel/user-data", "default")
			downloadTime := time.Since(start).Milliseconds()
			require.NoError(t, err)

			start = time.Now()
			err = uploadZstd(ctx, client, zstdData, fmt.Sprintf("/tmp/zstd-default-%d", i), 0)
			uploadTime := time.Since(start).Milliseconds()
			require.NoError(t, err)

			downloadTotal += downloadTime
			uploadTotal += uploadTime
			sizeTotal += int64(len(zstdData))
		}
		results = append(results, result{
			name:        "Zstd (default)",
			downloadMs:  downloadTotal / iterations,
			uploadMs:    uploadTotal / iterations,
			archiveSize: sizeTotal / iterations,
		})
	}

	// Print comparison table
	t.Logf("\n=== Zip vs Zstd Comparison (%d iterations each) ===", iterations)
	t.Logf("Directory: %d files, %d KB\n", fileCount, dirSize/1024)
	t.Logf("%-18s | %-10s | %-10s | %-10s | %-12s", "Method", "Download", "Upload", "Total", "Archive Size")
	t.Logf("%-18s-+-%-10s-+-%-10s-+-%-10s-+-%-12s", "--------", "--------", "------", "-----", "------------")

	baseline := results[0]
	for _, r := range results {
		totalMs := r.downloadMs + r.uploadMs
		baselineTotal := baseline.downloadMs + baseline.uploadMs
		speedup := float64(baselineTotal) / float64(totalMs)
		sizeRatio := float64(r.archiveSize) / float64(dirSize) * 100

		t.Logf("%-18s | %7dms | %7dms | %7dms | %6dKB (%.0f%%)",
			r.name,
			r.downloadMs,
			r.uploadMs,
			totalMs,
			r.archiveSize/1024,
			sizeRatio)

		if r.name != baseline.name {
			t.Logf("  -> %.2fx %s than %s", speedup, speedDesc(speedup), baseline.name)
		}
	}
}

func speedDesc(ratio float64) string {
	if ratio > 1 {
		return "faster"
	}
	return "slower"
}
