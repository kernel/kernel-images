package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kernel/kernel-images/server/lib/extensionfetch"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func refPart(t *testing.T, w *multipart.Writer, ref extensionfetch.Ref) {
	t.Helper()
	b, err := json.Marshal(ref)
	require.NoError(t, err)
	require.NoError(t, w.WriteField("extensions.ref", string(b)))
}

func sumOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func metroFetcher(t *testing.T, archives map[string][]byte) *extensionfetch.Fetcher {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := archives[strings.TrimPrefix(r.URL.Path, "/extension-archive/")]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return &extensionfetch.Fetcher{
		BaseURL: func() string { return srv.URL },
		Token:   func() string { return "instance-jwt" },
		Client:  srv.Client(),
	}
}

func TestChromiumCfgParseMultipartExtensionRefs(t *testing.T) {
	buf := bytes.NewBuffer(nil)
	w := multipart.NewWriter(buf)
	refPart(t, w, extensionfetch.Ref{Name: "ext1", SHA256: sumOf([]byte("a")), Grant: "g1"})
	refPart(t, w, extensionfetch.Ref{Name: "ext2", SHA256: sumOf([]byte("b")), Grant: "g2"})
	require.NoError(t, w.Close())

	st := &chromiumConfigureState{}
	err := chromiumCfgParseMultipart(multipart.NewReader(buf, w.Boundary()), st)
	defer st.cleanup()
	require.NoError(t, err)
	require.Len(t, st.extRefs, 2)
	require.Equal(t, "ext1", st.extRefs[0].Name)
	require.Equal(t, "g2", st.extRefs[1].Grant)
	require.Empty(t, st.extItems, "references are fetched after parsing")
}

func TestChromiumCfgParseMultipartExtensionRefValidation(t *testing.T) {
	for name, value := range map[string]string{
		"not json":       "{",
		"bad checksum":   `{"name":"ext1","sha256":"abc","grant":"g"}`,
		"path traversal": `{"name":"../ext1","sha256":"` + sumOf(nil) + `","grant":"g"}`,
		"too large":      `{"name":"ext1","sha256":"` + sumOf(nil) + `","grant":"` + strings.Repeat("g", maxExtensionRefBytes) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			buf := bytes.NewBuffer(nil)
			w := multipart.NewWriter(buf)
			require.NoError(t, w.WriteField("extensions.ref", value))
			require.NoError(t, w.Close())

			st := &chromiumConfigureState{}
			err := chromiumCfgParseMultipart(multipart.NewReader(buf, w.Boundary()), st)
			defer st.cleanup()
			require.ErrorContains(t, err, "invalid extensions.ref")
		})
	}
}

func TestChromiumCfgParseMultipartExtensionRefLimit(t *testing.T) {
	buf := bytes.NewBuffer(nil)
	w := multipart.NewWriter(buf)
	for i := 0; i <= maxExtensionUploadCount; i++ {
		refPart(t, w, extensionfetch.Ref{Name: fmt.Sprintf("ext%d", i), SHA256: sumOf(nil), Grant: "g"})
	}
	require.NoError(t, w.Close())

	st := &chromiumConfigureState{}
	err := chromiumCfgParseMultipart(multipart.NewReader(buf, w.Boundary()), st)
	defer st.cleanup()
	require.ErrorContains(t, err, "too many extensions")
}

func TestFetchExtensionRefs(t *testing.T) {
	archives := map[string][]byte{"ext1": []byte("zip-one"), "ext2": []byte("zip-two")}
	s := &ApiService{extensionFetcher: metroFetcher(t, archives)}
	st := &chromiumConfigureState{
		extItems: []extensionZipItem{{zipTemp: "uploaded.zip", name: "uploaded"}},
		extRefs: []extensionfetch.Ref{
			{Name: "ext1", SHA256: sumOf(archives["ext1"]), Grant: "g"},
			{Name: "ext2", SHA256: sumOf(archives["ext2"]), Grant: "g"},
		},
	}
	defer st.cleanup()

	require.NoError(t, s.fetchExtensionRefs(context.Background(), st))
	require.Len(t, st.extItems, 3)
	require.Equal(t, "uploaded", st.extItems[0].name)
	for i, name := range []string{"ext1", "ext2"} {
		item := st.extItems[i+1]
		require.Equal(t, name, item.name)
		got, err := os.ReadFile(item.zipTemp)
		require.NoError(t, err)
		require.Equal(t, archives[name], got)
	}
}

func TestFetchExtensionRefsFailure(t *testing.T) {
	archives := map[string][]byte{"ext1": []byte("zip-one")}
	refs := []extensionfetch.Ref{
		{Name: "ext1", SHA256: sumOf(archives["ext1"]), Grant: "g"},
		{Name: "missing", SHA256: sumOf([]byte("x")), Grant: "g"},
	}

	t.Run("one fetch fails", func(t *testing.T) {
		s := &ApiService{extensionFetcher: metroFetcher(t, archives)}
		st := &chromiumConfigureState{extRefs: refs}
		defer st.cleanup()
		err := s.fetchExtensionRefs(context.Background(), st)
		require.ErrorContains(t, err, "fetch missing: status 404")
		require.Empty(t, st.extItems)
	})

	t.Run("no fetcher", func(t *testing.T) {
		s := &ApiService{}
		st := &chromiumConfigureState{extRefs: refs}
		defer st.cleanup()
		require.ErrorContains(t, s.fetchExtensionRefs(context.Background(), st), "not configured")
	})
}

func TestChromiumConfigureExtensionFetchFailureAppliesNothing(t *testing.T) {
	s := &ApiService{extensionFetcher: metroFetcher(t, nil)}

	buf := bytes.NewBuffer(nil)
	w := multipart.NewWriter(buf)
	refPart(t, w, extensionfetch.Ref{Name: "ext1", SHA256: sumOf([]byte("a")), Grant: "g"})
	require.NoError(t, w.WriteField("chromium_flags", `{"flags":["--kiosk"]}`))
	require.NoError(t, w.Close())

	resp, err := s.ChromiumConfigure(context.Background(), oapi.ChromiumConfigureRequestObject{
		Body: multipart.NewReader(buf, w.Boundary()),
	})
	require.NoError(t, err)
	failure, ok := resp.(oapi.ChromiumConfigure500JSONResponse)
	require.True(t, ok, "got %T", resp)
	require.NotNil(t, failure.Step)
	require.Equal(t, oapi.ExtensionFetch, *failure.Step)
}
