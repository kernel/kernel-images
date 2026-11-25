package camera

import (
	crand "crypto/rand"
	"encoding/binary"
	"math"
	"math/rand/v2"
)

// noiseFilter adds image-sensor-like noise to yuyv422 frames. Every frame a
// real sensor captures differs slightly: temporal shot/read noise scaled by
// brightness, plus a static fixed-pattern component per pixel. Video that has
// been through a codec lacks that signature — flat regions come out perfectly
// smooth — which is one of the ways a relayed camera feed can be told apart
// from a local one. The amplitudes here are deliberately small: at 0.5–3 LSB
// they are invisible to a viewer and to liveness math, but flat regions grain
// like a real sensor and no two frames are ever identical.
type noiseFilter struct {
	fpn []int8   // static per-luma-pixel offset (fixed pattern noise)
	lut [256]int // temporal amplitude per luma value (∝ sqrt brightness)
	rng *rand.Rand
}

// newNoiseFilter builds a filter for w×h yuyv422 frames.
func newNoiseFilter(w, h int) *noiseFilter {
	var seed [16]byte
	_, _ = crand.Read(seed[:])
	rng := rand.New(rand.NewPCG(binary.LittleEndian.Uint64(seed[:8]),
		binary.LittleEndian.Uint64(seed[8:])))

	n := &noiseFilter{fpn: make([]int8, w*h), rng: rng}
	for i := range n.fpn {
		// ±1 LSB static offset per pixel, uniform.
		n.fpn[i] = int8(rng.IntN(3) - 1)
	}
	for y := range n.lut {
		// Shot noise scales with sqrt(signal); keep a floor so flat black
		// (the placeholder) still grains like a dark sensor read. Max ±4
		// LSB at full white.
		n.lut[y] = 2 + int(math.Round(2*math.Sqrt(float64(y)/255)))
	}
	return n
}

// apply adds one fresh noise realization to the frame in place. frame must be
// packed yuyv422 (even offsets luma, odd offsets chroma).
func (n *noiseFilter) apply(frame []byte) {
	pix := 0
	for i := 0; i+1 < len(frame); i += 2 {
		frame[i] = clamp8(int(frame[i]) + int(n.fpn[pix]) + n.gauss(n.lut[frame[i]]))
		frame[i+1] = clamp8(int(frame[i+1]) + n.gauss(1))
		pix++
	}
}

// gauss returns a zero-mean approximately gaussian sample in [-amp, amp]
// (Irwin–Hall from one 32-bit draw, no float math per pixel).
func (n *noiseFilter) gauss(amp int) int {
	r := n.rng.Uint32()
	sum := int(r&0xff) + int(r>>8&0xff) + int(r>>16&0xff) // 0..765, mean 382.5
	return (sum - 383) * amp / 383
}

func clamp8(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}
