package events

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTruncateCaptured(t *testing.T) {
	t.Run("a value within the cap is untouched", func(t *testing.T) {
		assert.Equal(t, "page.click('#login')", TruncateCaptured("page.click('#login')", CapturedFieldCap))
	})

	t.Run("a value over the cap is marked", func(t *testing.T) {
		got := TruncateCaptured(strings.Repeat("a", CapturedFieldCap+1), CapturedFieldCap)
		require.True(t, strings.HasSuffix(got, TruncatedSuffix))
		assert.LessOrEqual(t, len(got), CapturedFieldCap)
	})

	t.Run("the cut never splits a character", func(t *testing.T) {
		// Every rune is 3 bytes, so a byte-wise cut lands inside one.
		got := TruncateCaptured(strings.Repeat("あ", 40), 41)
		assert.True(t, utf8.ValidString(got), "result is not valid UTF-8: %q", got)
		assert.LessOrEqual(t, len(got), 41)
	})

	t.Run("a cap too small for the marker yields only the marker", func(t *testing.T) {
		assert.Equal(t, TruncatedSuffix[:4], TruncateCaptured("abcdefghij", 4))
		assert.Equal(t, "", TruncateCaptured("abcdefghij", 0))
	})

	// The cap exists so one field cannot push an envelope past the record limit,
	// which would null the whole payload instead of clipping the field.
	t.Run("the field cap leaves room under the record limit", func(t *testing.T) {
		assert.Less(t, CapturedFieldCap, maxS2RecordBytes)
	})
}
