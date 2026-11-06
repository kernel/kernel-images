package extensionfetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var archive = []byte("PK fake extension")

func archiveSum() string {
	sum := sha256.Sum256(archive)
	return hex.EncodeToString(sum[:])
}

func newFetcher(t *testing.T, handler http.HandlerFunc) *Fetcher {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Fetcher{
		BaseURL: func() string { return srv.URL + "/" },
		Token:   func() string { return "instance-jwt" },
		Client:  srv.Client(),
	}
}

func TestFetch(t *testing.T) {
	f := newFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/extension-archive/ext1", r.URL.Path)
		require.Equal(t, "Bearer instance-jwt", r.Header.Get("Authorization"))
		require.Equal(t, "the-grant", r.Header.Get("X-Kernel-Extension-Grant"))
		_, _ = w.Write(archive)
	})
	dst := filepath.Join(t.TempDir(), "ext.zip")
	require.NoError(t, f.Fetch(context.Background(), Ref{Name: "ext1", SHA256: archiveSum(), Grant: "the-grant"}, dst))

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, archive, got)
	info, err := os.Stat(dst)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestFetchFailures(t *testing.T) {
	ok := Ref{Name: "ext1", SHA256: archiveSum(), Grant: "g"}
	cases := []struct {
		name    string
		ref     Ref
		handler http.HandlerFunc
		want    string
	}{
		{"refused", ok, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "forbidden", http.StatusForbidden)
		}, "status 403"},
		{"checksum mismatch", ok, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("something else"))
		}, "checksum mismatch"},
		{"bad name", Ref{Name: "../x", SHA256: archiveSum(), Grant: "g"}, nil, "invalid name"},
		{"bad checksum", Ref{Name: "ext1", SHA256: strings.ToUpper(archiveSum()), Grant: "g"}, nil, "invalid sha256"},
		{"no grant", Ref{Name: "ext1", SHA256: archiveSum()}, nil, "missing grant"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFetcher(t, tc.handler)
			err := f.Fetch(context.Background(), tc.ref, filepath.Join(t.TempDir(), "ext.zip"))
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestFetchUnconfigured(t *testing.T) {
	f := &Fetcher{BaseURL: func() string { return "" }, Token: func() string { return "" }, Client: http.DefaultClient}
	err := f.Fetch(context.Background(), Ref{Name: "ext1", SHA256: archiveSum(), Grant: "g"}, filepath.Join(t.TempDir(), "ext.zip"))
	require.ErrorContains(t, err, "not configured")
}
