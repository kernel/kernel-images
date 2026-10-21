// Local zoom and pan of the live view video. The remote screen is unchanged;
// this only scales what the viewer sees so a large stream is usable on a phone.

import { Point } from './touch-gestures'

export interface Box {
  left: number
  top: number
  width: number
  height: number
}

export class ZoomPan {
  scale = 1
  tx = 0
  ty = 0

  private startScale = 1
  private startDistance = 1
  private anchor: Point = { x: 0, y: 0 }

  // element: the video box before the transform. viewport: the visible area the
  // zoomed video may fill, which includes any letterbox bars around it.
  constructor(private element: () => Box, private viewport: () => Box, private maxScale: () => number) {}

  get zoomed() {
    return this.scale > 1.01
  }

  pinchStart(mid: Point, distance: number) {
    const e = this.element()
    this.startScale = this.scale
    this.startDistance = Math.max(distance, 1)
    // content point (in unscaled element px) under the fingers
    this.anchor = {
      x: (mid.x - e.left - this.tx) / this.scale,
      y: (mid.y - e.top - this.ty) / this.scale,
    }
  }

  pinchMove(mid: Point, distance: number) {
    const e = this.element()
    const scale = Math.min(this.maxScale(), Math.max(1, (this.startScale * distance) / this.startDistance))
    this.scale = scale
    this.tx = clampAxis(mid.x - e.left - this.anchor.x * scale, e.left, e.width * scale, this.viewport(), 'x')
    this.ty = clampAxis(mid.y - e.top - this.anchor.y * scale, e.top, e.height * scale, this.viewport(), 'y')
  }

  reset() {
    this.scale = 1
    this.tx = 0
    this.ty = 0
  }

  get transform() {
    if (!this.zoomed) return ''
    return `translate(${this.tx}px, ${this.ty}px) scale(${this.scale})`
  }
}

// Content larger than the viewport must cover it; smaller content stays centered.
function clampAxis(offset: number, origin: number, size: number, viewport: Box, axis: 'x' | 'y') {
  const start = axis === 'x' ? viewport.left : viewport.top
  const length = axis === 'x' ? viewport.width : viewport.height
  if (size <= length) return start + (length - size) / 2 - origin
  return Math.min(start - origin, Math.max(start + length - size - origin, offset))
}
