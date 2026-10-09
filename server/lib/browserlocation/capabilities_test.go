package browserlocation

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCurrentCapabilitiesAreNormalized(t *testing.T) {
	c := Current()
	assert.Equal(t, New(c.Browser, c.TZData, c.Locales, c.TimeZones), c)
	assert.NotEmpty(t, c.Browser)
	assert.NotEmpty(t, c.TZData)
}

func TestCurrentCapabilitiesCoverRepresentativeValues(t *testing.T) {
	c := Current()
	for _, locale := range []string{"en-US", "en-SG", "de-DE", "pt-BR", "es-MX", "zh-TW", "ja-JP"} {
		assert.True(t, c.SupportsLocale(locale), locale)
	}
	for _, locale := range []string{"en-QQ", "de-de", "en_US", "", "zh-Hant-XX"} {
		assert.False(t, c.SupportsLocale(locale), locale)
	}
	for _, timezone := range []string{"America/Los_Angeles", "America/New_York", "Europe/Berlin", "Asia/Singapore", "Etc/UTC"} {
		assert.True(t, c.SupportsTimeZone(timezone), timezone)
	}
	for _, timezone := range []string{"Mars/Olympus_Mons", "../etc/passwd", "posix/Europe/Berlin", ""} {
		assert.False(t, c.SupportsTimeZone(timezone), timezone)
	}
}

func TestNewSortsAndDeduplicates(t *testing.T) {
	c := New("Chrome/1.0", "2026a", []string{"fr-FR", "de-DE", "fr-FR"}, []string{"Etc/UTC", "Europe/Berlin", "Etc/UTC"})
	assert.Equal(t, []string{"de-DE", "fr-FR"}, c.Locales)
	assert.Equal(t, []string{"Etc/UTC", "Europe/Berlin"}, c.TimeZones)
	assert.Contains(t, c.Version, "Chrome/1.0+tzdata-2026a+")
	assert.NotEqual(t, c.Version, New("Chrome/1.0", "2026a", []string{"de-DE"}, c.TimeZones).Version)
}
