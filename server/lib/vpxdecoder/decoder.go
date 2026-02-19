package vpxdecoder

/*
#cgo pkg-config: vpx
#include <vpx/vpx_decoder.h>
#include <vpx/vp8dx.h>
#include <stdlib.h>
#include <string.h>

// vpx_codec_dec_init is a macro; wrap it for CGo.
static vpx_codec_err_t init_vp8_decoder(vpx_codec_ctx_t *ctx) {
	vpx_codec_dec_cfg_t cfg = {0};
	return vpx_codec_dec_init(ctx, vpx_codec_vp8_dx(), &cfg, 0);
}
*/
import "C"

import (
	"fmt"
	"image"
	"unsafe"
)

// Decoder is a VP8 video decoder backed by libvpx via CGo.
// It decodes both keyframes AND inter-frames, maintaining full
// reference frame state, so every frame produces a decoded image.
type Decoder struct {
	codec C.vpx_codec_ctx_t
}

func New() (*Decoder, error) {
	d := &Decoder{}
	status := C.init_vp8_decoder(&d.codec)
	if status != C.VPX_CODEC_OK {
		return nil, fmt.Errorf("vpx_codec_dec_init failed: status=%d", int(status))
	}
	return d, nil
}

// Decode decodes a single VP8 frame (keyframe or inter-frame) and returns
// the decoded image as YCbCr 4:2:0. The returned image's data is valid
// until the next Decode call (libvpx reuses internal buffers).
func (d *Decoder) Decode(data []byte) (*image.YCbCr, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty frame data")
	}

	status := C.vpx_codec_decode(
		&d.codec,
		(*C.uint8_t)(unsafe.Pointer(&data[0])),
		C.uint(len(data)),
		nil, 0,
	)
	if status != C.VPX_CODEC_OK {
		return nil, fmt.Errorf("vpx_codec_decode failed: status=%d", int(status))
	}

	var iter C.vpx_codec_iter_t
	img := C.vpx_codec_get_frame(&d.codec, &iter)
	if img == nil {
		return nil, fmt.Errorf("no frame available after decode")
	}

	w := int(img.d_w)
	h := int(img.d_h)

	// libvpx outputs I420 (YUV 4:2:0 planar).
	yStride := int(img.stride[0])
	uStride := int(img.stride[1])
	vStride := int(img.stride[2])

	if uStride != vStride {
		return nil, fmt.Errorf("U and V plane strides differ (%d vs %d); unsupported by image.YCbCr", uStride, vStride)
	}

	// Copy plane data to Go-managed memory so it's safe to hold
	// after the next Decode call.
	yLen := yStride * h
	uLen := uStride * ((h + 1) / 2)
	vLen := vStride * ((h + 1) / 2)

	yData := C.GoBytes(unsafe.Pointer(img.planes[0]), C.int(yLen))
	uData := C.GoBytes(unsafe.Pointer(img.planes[1]), C.int(uLen))
	vData := C.GoBytes(unsafe.Pointer(img.planes[2]), C.int(vLen))

	return &image.YCbCr{
		Y:              yData,
		Cb:             uData,
		Cr:             vData,
		YStride:        yStride,
		CStride:        uStride,
		SubsampleRatio: image.YCbCrSubsampleRatio420,
		Rect:           image.Rect(0, 0, w, h),
	}, nil
}

func (d *Decoder) Close() {
	C.vpx_codec_destroy(&d.codec)
}
