package camera

// A scaler converts yuyv422 frames between the ingest-native size and whatever
// size the capture client negotiated, on the hot path with no per-frame
// allocation. It crops the source centrally to the target aspect and then
// bilinear-scales, the way a sensor's native modes crop or bin rather than
// stretch. Coordinate and weight tables are precomputed per target mode, so
// the per-frame cost is one 2-tap horizontal pass per source row plus one
// 2-tap vertical pass per destination row, per plane.
type scaler struct {
	srcW, srcH int
	dstW, dstH int
	// crop window within the source, in source pixels
	ox, oy, cw, ch int
	xmY, xmC       []tap // horizontal maps for luma and chroma planes
	ym             []tap // vertical map (all planes share height)

	// rows holds the horizontally scaled planes: rows[plane][y][x], reused
	// across frames.
	rows [3][][]uint8
}

type tap struct {
	i0, i1 int
	w0, w1 uint16 // w0+w1 == 256
}

// newScaler builds a scaler from srcW×srcH to dstW×dstH yuyv422.
func newScaler(srcW, srcH, dstW, dstH int) *scaler {
	s := &scaler{srcW: srcW, srcH: srcH, dstW: dstW, dstH: dstH}
	// Largest centered source region with the target aspect.
	s.cw, s.ch = srcW, srcH
	if int64(s.cw)*int64(dstH) > int64(s.ch)*int64(dstW) {
		s.cw = s.ch * dstW / dstH
	} else {
		s.ch = s.cw * dstH / dstW
	}
	s.ox = (srcW - s.cw) / 2
	s.oy = (srcH - s.ch) / 2
	s.xmY = buildTaps(dstW, s.cw)
	s.xmC = buildTaps(dstW/2, s.cw/2)
	s.ym = buildTaps(dstH, s.ch)
	for y := 0; y < s.ch; y++ {
		s.rows[0] = append(s.rows[0], make([]uint8, dstW))
		s.rows[1] = append(s.rows[1], make([]uint8, dstW/2))
		s.rows[2] = append(s.rows[2], make([]uint8, dstW/2))
	}
	return s
}

// buildTaps maps n destination samples onto a window of srcN samples: for
// each dst index, the two nearest source indices and their 8-bit weights.
func buildTaps(n, srcN int) []tap {
	taps := make([]tap, n)
	if n <= 0 || srcN <= 0 {
		return taps
	}
	if srcN == 1 {
		for i := range taps {
			taps[i] = tap{0, 0, 256, 0}
		}
		return taps
	}
	for i := range taps {
		// dst sample center in source coordinates, in 1/256ths
		fp := (i*2+1)*srcN*256/(n*2) - 128
		i0 := fp >> 8
		if i0 < 0 {
			i0 = 0
			fp = 0
		}
		i1 := i0 + 1
		if i0 >= srcN-1 {
			i0, i1 = srcN-1, srcN-1
			fp = 0
		} else {
			fp &= 0xff
		}
		taps[i] = tap{i0, i1, uint16(256 - fp), uint16(fp)}
	}
	return taps
}

// frameSize returns the dst frame size in bytes.
func (s *scaler) frameSize() int { return s.dstW * s.dstH * 2 }

// sameSize reports whether no scaling is needed (the crop is the whole
// source and sizes match).
func (s *scaler) sameSize() bool {
	return s.srcW == s.dstW && s.srcH == s.dstH
}

// scale converts src (srcW×srcH yuyv422) into dst (dstW×dstH yuyv422, the
// caller's buffer of frameSize() bytes).
func (s *scaler) scale(dst, src []byte) {
	if s.sameSize() {
		copy(dst, src)
		return
	}
	for y := 0; y < s.ch; y++ {
		row := src[(s.oy+y)*s.srcW*2 : (s.oy+y+1)*s.srcW*2]
		s.scaleRowLuma(s.rows[0][y], row)
		s.scaleRowChroma(s.rows[1][y], s.rows[2][y], row)
	}
	rowsY, rowsU, rowsV := s.rows[0], s.rows[1], s.rows[2]
	for dy := 0; dy < s.dstH; dy++ {
		t := s.ym[dy]
		r0, r1 := rowsY[t.i0], rowsY[t.i1]
		u0, u1 := rowsU[t.i0], rowsU[t.i1]
		v0, v1 := rowsV[t.i0], rowsV[t.i1]
		out := dst[dy*s.dstW*2 : (dy+1)*s.dstW*2]
		for x := 0; x < s.dstW; x++ {
			out[2*x] = blend(r0[x], r1[x], t.w0, t.w1)
		}
		for x := 0; x < s.dstW/2; x++ {
			out[4*x+1] = blend(u0[x], u1[x], t.w0, t.w1)
			out[4*x+3] = blend(v0[x], v1[x], t.w0, t.w1)
		}
	}
}

// scaleRowLuma horizontally scales one source row's luma into dst (dstW bytes).
func (s *scaler) scaleRowLuma(dst, row []byte) {
	base := s.ox * 2
	for x := 0; x < s.dstW; x++ {
		t := s.xmY[x]
		dst[x] = blend(row[base+2*t.i0], row[base+2*t.i1], t.w0, t.w1)
	}
}

// scaleRowChroma horizontally scales one source row's chroma into dstU/dstV
// (dstW/2 bytes each).
func (s *scaler) scaleRowChroma(dstU, dstV, row []byte) {
	base := s.ox * 2
	for x := 0; x < s.dstW/2; x++ {
		t := s.xmC[x]
		dstU[x] = blend(row[base+4*t.i0+1], row[base+4*t.i1+1], t.w0, t.w1)
		dstV[x] = blend(row[base+4*t.i0+3], row[base+4*t.i1+3], t.w0, t.w1)
	}
}

// blend is the 2-tap bilinear kernel: (a*w0 + b*w1) / 256, rounded.
func blend(a, b uint8, w0, w1 uint16) uint8 {
	return uint8((uint16(a)*w0 + uint16(b)*w1 + 128) >> 8)
}
