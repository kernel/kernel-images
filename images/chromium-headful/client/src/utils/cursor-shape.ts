// Classifies the remote X cursor image that neko pushes over the data channel.
// Chromium switches to the text (I-beam) cursor over editable fields, which is
// the only signal the client has that a tap landed on something typeable.

export type CursorKind = 'text' | 'other' | 'unknown'

export interface CursorImage {
  width: number
  height: number
  xhot: number
  yhot: number
  png: Uint8Array
}

// data channel opcode for cursor images (neko server/internal/webrtc/payload/send.go)
const OP_CURSOR_IMAGE = 0x02
const HEADER_SIZE = 3
const CURSOR_HEADER_SIZE = 8

export function parseCursorImage(buffer: ArrayBuffer): CursorImage | undefined {
  if (buffer.byteLength < HEADER_SIZE + CURSOR_HEADER_SIZE) return
  const view = new DataView(buffer)
  if (view.getUint8(0) !== OP_CURSOR_IMAGE) return
  return {
    width: view.getUint16(3),
    height: view.getUint16(5),
    xhot: view.getUint16(7),
    yhot: view.getUint16(9),
    png: new Uint8Array(buffer, HEADER_SIZE + CURSOR_HEADER_SIZE),
  }
}

const cache = new Map<string, CursorKind>()

function cacheKey(img: CursorImage) {
  // FNV-1a over the image bytes; cursor images are small
  let hash = 0x811c9dc5
  for (let i = 0; i < img.png.length; i++) {
    hash ^= img.png[i]
    hash = Math.imul(hash, 0x01000193)
  }
  return `${img.width}x${img.height}@${img.xhot},${img.yhot}:${img.png.length}:${hash >>> 0}`
}

export function cachedCursorKind(img: CursorImage): CursorKind | undefined {
  return cache.get(cacheKey(img))
}

export async function classifyCursor(img: CursorImage): Promise<CursorKind> {
  const key = cacheKey(img)
  const cached = cache.get(key)
  if (cached) return cached

  let kind: CursorKind = 'unknown'
  try {
    const bitmap = await createImageBitmap(new Blob([img.png], { type: 'image/png' }))
    const canvas = document.createElement('canvas')
    canvas.width = bitmap.width
    canvas.height = bitmap.height
    const ctx = canvas.getContext('2d')
    if (ctx) {
      ctx.drawImage(bitmap, 0, 0)
      kind = classifyPixels(ctx.getImageData(0, 0, bitmap.width, bitmap.height), img.xhot, img.yhot)
    }
    bitmap.close()
  } catch (e) {
    kind = 'unknown'
  }

  cache.set(key, kind)
  return kind
}

function classifyPixels(data: ImageData, xhot: number, yhot: number): CursorKind {
  let minX = Infinity
  let minY = Infinity
  let maxX = -Infinity
  let maxY = -Infinity
  for (let y = 0; y < data.height; y++) {
    for (let x = 0; x < data.width; x++) {
      if (data.data[(y * data.width + x) * 4 + 3] > 64) {
        minX = Math.min(minX, x)
        maxX = Math.max(maxX, x)
        minY = Math.min(minY, y)
        maxY = Math.max(maxY, y)
      }
    }
  }
  // an empty image means the cursor is hidden
  if (maxX < 0) return 'unknown'

  const width = maxX - minX + 1
  const height = maxY - minY + 1
  // I-beam: tall and narrow, hotspot in the middle of the shape. Arrows and
  // hands have their hotspot at the tip, near the top edge.
  const narrow = height >= 8 && width / height <= 0.6
  const centeredX = Math.abs(xhot - (minX + maxX) / 2) <= Math.max(2, width * 0.35)
  const middleY = yhot > minY + height * 0.2 && yhot < maxY - height * 0.2
  return narrow && centeredX && middleY ? 'text' : 'other'
}
