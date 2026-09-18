package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateBrowserLocationBundle(t *testing.T) {
	zone := filepath.Join("/usr/share/zoneinfo", "America", "New_York")
	if _, err := os.Stat(zone); err != nil {
		t.Skip("zoneinfo unavailable")
	}

	bundle, err := validateBrowserLocationBundle(`{"epoch":"lease-1","generation":2,"timezone":"America/New_York","locale":"en-US","languages":["en-US","en"]}`)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), bundle.Generation)
	assert.Equal(t, []string{"en-US", "en"}, bundle.Languages)

	for _, raw := range []string{
		`{"epoch":"lease-1","generation":2,"timezone":"../../etc/passwd","locale":"en-US","languages":["en-US"]}`,
		`{"epoch":"lease-1","generation":2,"timezone":"America/New_York","locale":"en-us","languages":["en-us"]}`,
		`{"epoch":"lease-1","generation":2,"timezone":"America/New_York","locale":"en-US","languages":["de-DE"]}`,
	} {
		_, err := validateBrowserLocationBundle(raw)
		assert.Error(t, err)
	}
}

func TestBrowserLocationBundlesEqual(t *testing.T) {
	a := browserLocationBundle{Epoch: "lease", Generation: 1, TimeZone: "UTC", Locale: "en-US", Languages: []string{"en-US", "en"}}
	b := a
	b.Languages = append([]string(nil), a.Languages...)
	assert.True(t, browserLocationBundlesEqual(a, b))
	b.Generation++
	assert.False(t, browserLocationBundlesEqual(a, b))
}
