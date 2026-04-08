package typinghumanizer

import (
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUniformJitter(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	t.Run("stays within range", func(t *testing.T) {
		for i := 0; i < 1000; i++ {
			d := UniformJitter(rng, 100, 30, 50)
			ms := d.Milliseconds()
			assert.GreaterOrEqual(t, ms, int64(50), "should be >= minMs")
			assert.LessOrEqual(t, ms, int64(130), "should be <= baseMs+jitterMs")
		}
	})

	t.Run("clamps to minimum", func(t *testing.T) {
		for i := 0; i < 100; i++ {
			d := UniformJitter(rng, 10, 20, 5)
			assert.GreaterOrEqual(t, d.Milliseconds(), int64(5))
		}
	})

	t.Run("zero jitter returns base", func(t *testing.T) {
		d := UniformJitter(rng, 100, 0, 0)
		assert.Equal(t, 100*time.Millisecond, d)
	})
}

func TestSplitWordChunks(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "simple sentence",
			input:    "Hello world",
			expected: []string{"Hello ", "world"},
		},
		{
			name:     "with punctuation",
			input:    "Hello world. How are you?",
			expected: []string{"Hello ", "world. ", "How ", "are ", "you?"},
		},
		{
			name:     "single word",
			input:    "Hello",
			expected: []string{"Hello"},
		},
		{
			name:     "empty string",
			input:    "",
			expected: nil,
		},
		{
			name:     "only spaces",
			input:    "   ",
			expected: []string{"   "},
		},
		{
			name:     "multiple spaces between words",
			input:    "Hello  world",
			expected: []string{"Hello  ", "world"},
		},
		{
			name:     "trailing space",
			input:    "Hello ",
			expected: []string{"Hello "},
		},
		{
			name:     "leading space",
			input:    " Hello",
			expected: []string{" ", "Hello"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SplitWordChunks(tt.input)
			require.Equal(t, tt.expected, result)
		})
	}
}

func TestIsSentenceEnd(t *testing.T) {
	tests := []struct {
		chunk    string
		expected bool
	}{
		{"world. ", true},
		{"you?", true},
		{"wow! ", true},
		{"Hello ", false},
		{"word", false},
		{"", false},
		{"   ", false},
		{"end.", true},
	}

	for _, tt := range tests {
		t.Run(tt.chunk, func(t *testing.T) {
			assert.Equal(t, tt.expected, IsSentenceEnd(tt.chunk))
		})
	}
}

func TestAdjacentKey(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	t.Run("returns a neighbor for lowercase letters", func(t *testing.T) {
		for i := 0; i < 100; i++ {
			adj := AdjacentKey(rng, 'a')
			assert.Contains(t, []rune{'q', 'w', 's', 'z'}, adj)
		}
	})

	t.Run("preserves uppercase", func(t *testing.T) {
		for i := 0; i < 100; i++ {
			adj := AdjacentKey(rng, 'A')
			assert.Contains(t, []rune{'Q', 'W', 'S', 'Z'}, adj)
		}
	})

	t.Run("returns same char for non-letters", func(t *testing.T) {
		assert.Equal(t, '5', AdjacentKey(rng, '5'))
		assert.Equal(t, '.', AdjacentKey(rng, '.'))
		assert.Equal(t, ' ', AdjacentKey(rng, ' '))
	})
}

func TestGenerateTypoPositions(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	t.Run("zero rate returns nil", func(t *testing.T) {
		assert.Nil(t, GenerateTypoPositions(rng, 100, 0))
	})

	t.Run("short text returns nil or few typos", func(t *testing.T) {
		typos := GenerateTypoPositions(rng, 1, 0.05)
		assert.Nil(t, typos)
	})

	t.Run("positions are within bounds and sorted", func(t *testing.T) {
		textLen := 200
		typos := GenerateTypoPositions(rng, textLen, 0.03)
		for i, typo := range typos {
			assert.GreaterOrEqual(t, typo.Pos, 0)
			assert.Less(t, typo.Pos, textLen)
			if i > 0 {
				assert.Greater(t, typo.Pos, typos[i-1].Pos, "positions must be strictly increasing")
			}
		}
	})

	t.Run("roughly matches expected count", func(t *testing.T) {
		textLen := 1000
		rate := 0.03
		totalTypos := 0
		runs := 200
		for i := 0; i < runs; i++ {
			localRng := rand.New(rand.NewSource(int64(i)))
			typos := GenerateTypoPositions(localRng, textLen, rate)
			totalTypos += len(typos)
		}
		avgTypos := float64(totalTypos) / float64(runs)
		expected := float64(textLen) * rate
		assert.InDelta(t, expected, avgTypos, expected*0.3, "average typo count should be near expected")
	})

	t.Run("kind distribution is weighted", func(t *testing.T) {
		counts := map[TypoKind]int{}
		for i := 0; i < 500; i++ {
			localRng := rand.New(rand.NewSource(int64(i)))
			typos := GenerateTypoPositions(localRng, 500, 0.05)
			for _, typo := range typos {
				counts[typo.Kind]++
			}
		}
		total := counts[TypoAdjacentKey] + counts[TypoDoubling] + counts[TypoTranspose] + counts[TypoExtraChar]
		require.Greater(t, total, 0)
		adjPct := float64(counts[TypoAdjacentKey]) / float64(total)
		doubPct := float64(counts[TypoDoubling]) / float64(total)
		transPct := float64(counts[TypoTranspose]) / float64(total)
		extraPct := float64(counts[TypoExtraChar]) / float64(total)
		assert.InDelta(t, 0.60, adjPct, 0.10, "adjacent key should be ~60%%")
		assert.InDelta(t, 0.20, doubPct, 0.10, "doubling should be ~20%%")
		assert.InDelta(t, 0.15, transPct, 0.10, "transpose should be ~15%%")
		assert.InDelta(t, 0.05, extraPct, 0.06, "extra char should be ~5%%")
	})
}
