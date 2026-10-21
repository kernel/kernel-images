package browserlocation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// icuHostTimeZone names the host timezone the way ICU's uprv_tzname does on
// Linux: the realpath of /etc/localtime from "/zoneinfo/" on.
func icuHostTimeZone(t *testing.T, localtime string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(localtime)
	require.NoError(t, err)
	_, id, found := strings.Cut(resolved, "/zoneinfo/")
	require.True(t, found, "%s resolves to %s", localtime, resolved)
	return id
}

func TestSetLocaltimeNamesTheRequestedZone(t *testing.T) {
	root := t.TempDir()
	zoneinfo := filepath.Join(root, "share", "zoneinfo")
	require.NoError(t, os.MkdirAll(filepath.Join(zoneinfo, "Etc"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(zoneinfo, "Europe"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(zoneinfo, "Etc", "UTC"), []byte("utc"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(zoneinfo, "Europe", "Prague"), []byte("prague"), 0o644))
	// tzdata ships these identifiers as links to other zones.
	require.NoError(t, os.Symlink("Etc/UTC", filepath.Join(zoneinfo, "UTC")))
	require.NoError(t, os.Symlink("Prague", filepath.Join(zoneinfo, "Europe", "Bratislava")))
	require.NoError(t, os.Symlink("Etc/UTC", filepath.Join(zoneinfo, "GMT")))
	copies := filepath.Join(root, "run", "zoneinfo")
	localtime := filepath.Join(root, "localtime")

	for _, tc := range []struct{ zone, host, data string }{
		{"UTC", "UTC", "utc"},
		{"Europe/Bratislava", "Europe/Bratislava", "prague"},
		{"Europe/Prague", "Europe/Prague", "prague"},
		{"GMT", "Etc/GMT", "utc"},
		{"Etc/UTC", "Etc/UTC", "utc"},
	} {
		require.NoError(t, setLocaltime(zoneinfo, copies, localtime, tc.zone))
		assert.Equal(t, tc.host, HostTimeZone(tc.zone))
		assert.Equal(t, tc.host, icuHostTimeZone(t, localtime))
		data, err := os.ReadFile(localtime)
		require.NoError(t, err)
		assert.Equal(t, tc.data, string(data))
		info, err := os.Stat(filepath.Join(copies, filepath.FromSlash(tc.host)))
		require.NoError(t, err)
		assert.True(t, info.Mode().IsRegular())
		assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
	}

	for _, zone := range []string{"../etc/passwd", "/etc/passwd", "", "Mars/Olympus_Mons"} {
		assert.Error(t, setLocaltime(zoneinfo, copies, localtime, zone), zone)
	}
	assert.Equal(t, "Etc/UTC", icuHostTimeZone(t, localtime), "a rejected zone must leave localtime alone")
}
