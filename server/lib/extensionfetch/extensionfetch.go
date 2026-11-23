// Package extensionfetch downloads uploaded extensions from the metro API's
// extension cache, so a configure request can name an extension instead of
// carrying its archive.
//
// HTTP interface:
//
//	GET {KERNEL_METRO_API_BASE_URL}/extension-archive/{name}
//	Authorization: Bearer <instance JWT>
//	X-Kernel-Extension-Grant: <grant from the configure request>
//
// A 200 response body is the extension's zip archive. Any other status fails
// the fetch; the caller decides what that means.
package extensionfetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	// fetchTimeout covers a cache miss, where the metro API reads and
	// decrypts the archive before serving it.
	fetchTimeout = 30 * time.Second
	// maxArchiveBytes is above the largest extension the API accepts.
	maxArchiveBytes = 64 << 20

	grantHeader = "X-Kernel-Extension-Grant"
	routePrefix = "/extension-archive/"
)

var (
	namePattern   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,255}$`)
	sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// Ref names an extension to fetch: Name is its ID, SHA256 the archive's
// checksum, and Grant the token authorizing this instance to fetch it.
type Ref struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Grant  string `json:"grant"`
}

// Validate reports whether the ref is well formed.
func (r Ref) Validate() error {
	switch {
	case !namePattern.MatchString(r.Name):
		return errors.New("invalid name")
	case !sha256Pattern.MatchString(r.SHA256):
		return errors.New("invalid sha256")
	case r.Grant == "":
		return errors.New("missing grant")
	}
	return nil
}

// Fetcher downloads extensions as the instance. Both values are read per
// fetch: a forked VM learns them after boot.
type Fetcher struct {
	BaseURL func() string
	Token   func() string
	Client  *http.Client
}

// Fetch writes the ref's archive to dst and verifies its checksum.
func (f *Fetcher) Fetch(ctx context.Context, ref Ref, dst string) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	base, token := strings.TrimRight(f.BaseURL(), "/"), f.Token()
	if base == "" || token == "" {
		return errors.New("metro API is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+routePrefix+url.PathEscape(ref.Name), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(grantHeader, ref.Grant)
	resp, err := f.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, sum), io.LimitReader(resp.Body, maxArchiveBytes+1))
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("read archive: %w", err)
	}
	if n > maxArchiveBytes {
		return errors.New("archive exceeds size cap")
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != ref.SHA256 {
		return errors.New("archive checksum mismatch")
	}
	return nil
}
