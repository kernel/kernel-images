package typinghumanizer

import (
	"math/rand"
	"strings"
	"time"
	"unicode"
)

// UniformJitter returns a random duration in [baseMs-jitterMs, baseMs+jitterMs],
// clamped to a minimum of minMs.
func UniformJitter(rng *rand.Rand, baseMs, jitterMs, minMs int) time.Duration {
	ms := baseMs - jitterMs + rng.Intn(2*jitterMs+1)
	if ms < minMs {
		ms = minMs
	}
	return time.Duration(ms) * time.Millisecond
}

// SplitWordChunks splits text into word-sized chunks, keeping trailing
// whitespace and punctuation attached to the preceding word.
// For example, "Hello world. How are you?" becomes:
//
//	["Hello ", "world. ", "How ", "are ", "you?"]
func SplitWordChunks(text string) []string {
	if len(text) == 0 {
		return nil
	}

	var chunks []string
	var current strings.Builder

	runes := []rune(text)
	i := 0
	for i < len(runes) {
		r := runes[i]
		current.WriteRune(r)
		i++

		if unicode.IsSpace(r) {
			for i < len(runes) && unicode.IsSpace(runes[i]) {
				current.WriteRune(runes[i])
				i++
			}
			chunks = append(chunks, current.String())
			current.Reset()
		}
	}

	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}

	return chunks
}

// IsSentenceEnd returns true if the chunk ends with sentence-ending punctuation
// (before any trailing whitespace).
func IsSentenceEnd(chunk string) bool {
	trimmed := strings.TrimRightFunc(chunk, unicode.IsSpace)
	if len(trimmed) == 0 {
		return false
	}
	last := trimmed[len(trimmed)-1]
	return last == '.' || last == '!' || last == '?'
}

// TypoKind identifies the type of typo to inject.
type TypoKind int

const (
	TypoAdjacentKey TypoKind = iota // Hit a neighboring key
	TypoDoubling                    // Type the character twice
	TypoTranspose                   // Swap current and next character
	TypoExtraChar                   // Insert a random adjacent key before the correct one
)

// Typo describes a single typo at a position in the text.
type Typo struct {
	Pos  int      // Character index in the rune slice
	Kind TypoKind // What kind of typo
}

// qwertyAdj maps each lowercase letter to its adjacent keys on a QWERTY layout.
var qwertyAdj = [26][]byte{
	{'q', 'w', 's', 'z'},           // a
	{'v', 'g', 'h', 'n'},           // b
	{'x', 'd', 'f', 'v'},           // c
	{'s', 'e', 'r', 'f', 'c', 'x'}, // d
	{'w', 's', 'd', 'r'},           // e
	{'d', 'r', 't', 'g', 'v', 'c'}, // f
	{'f', 't', 'y', 'h', 'b', 'v'}, // g
	{'g', 'y', 'u', 'j', 'n', 'b'}, // h
	{'u', 'j', 'k', 'o'},           // i
	{'h', 'u', 'i', 'k', 'n', 'm'}, // j
	{'j', 'i', 'o', 'l', 'm'},      // k
	{'k', 'o', 'p'},                // l
	{'n', 'j', 'k'},                // m
	{'b', 'h', 'j', 'm'},           // n
	{'i', 'k', 'l', 'p'},           // o
	{'o', 'l'},                     // p
	{'w', 'a'},                     // q
	{'e', 'd', 'f', 't'},           // r
	{'a', 'w', 'e', 'd', 'x', 'z'}, // s
	{'r', 'f', 'g', 'y'},           // t
	{'y', 'h', 'j', 'i'},           // u
	{'c', 'f', 'g', 'b'},           // v
	{'q', 'a', 's', 'e'},           // w
	{'z', 's', 'd', 'c'},           // x
	{'t', 'g', 'h', 'u'},           // y
	{'a', 's', 'x'},                // z
}

// AdjacentKey returns a random QWERTY neighbor of the given character.
// If the character has no known neighbors (non-letter), it returns the
// character itself unchanged.
func AdjacentKey(rng *rand.Rand, ch rune) rune {
	lower := unicode.ToLower(ch)
	if lower < 'a' || lower > 'z' {
		return ch
	}
	neighbors := qwertyAdj[lower-'a']
	if len(neighbors) == 0 {
		return ch
	}
	adj := rune(neighbors[rng.Intn(len(neighbors))])
	if unicode.IsUpper(ch) {
		adj = unicode.ToUpper(adj)
	}
	return adj
}

// GenerateTypoPositions computes typo positions using uniform gap sampling
// (each gap is uniform on [halfGap, halfGap+avgGap)). O(typos) random calls,
// not O(chars). Returns a sorted slice of Typo structs.
func GenerateTypoPositions(rng *rand.Rand, textLen int, typoRate float64) []Typo {
	if typoRate <= 0 || textLen <= 1 {
		return nil
	}
	avgGap := int(1.0 / typoRate)
	if avgGap < 2 {
		avgGap = 2
	}

	var typos []Typo
	halfGap := avgGap / 2
	if halfGap < 1 {
		halfGap = 1
	}
	pos := halfGap + rng.Intn(avgGap)
	for pos < textLen {
		roll := rng.Intn(100)
		var kind TypoKind
		switch {
		case roll < 60:
			kind = TypoAdjacentKey
		case roll < 80:
			kind = TypoDoubling
		case roll < 95:
			kind = TypoTranspose
		default:
			kind = TypoExtraChar
		}
		typos = append(typos, Typo{Pos: pos, Kind: kind})
		pos += halfGap + rng.Intn(avgGap)
	}
	return typos
}
