import { describe, expect, test } from 'bun:test'
import { charToKeysym, keysymNeedsShift, needsShift, textDiff, XK_RETURN } from '../src/utils/text-input'
import { ZoomPan } from '../src/utils/zoom-pan'
import { GestureHandlers, Point, TouchGestures } from '../src/utils/touch-gestures'

describe('text input', () => {
  test('maps characters to keysyms', () => {
    expect(charToKeysym('a')).toBe(0x61)
    expect(charToKeysym('é')).toBe(0xe9)
    expect(charToKeysym('€')).toBe(0x010020ac)
    expect(charToKeysym('\n')).toBe(XK_RETURN)
  })

  test('detects letters that need shift', () => {
    expect(needsShift('A')).toBe(true)
    expect(needsShift('a')).toBe(false)
    expect(needsShift('!')).toBe(false)
    expect(keysymNeedsShift(0x41)).toBe(true)
    expect(keysymNeedsShift(0x61)).toBe(false)
    expect(keysymNeedsShift(0xc9)).toBe(true)
    expect(keysymNeedsShift(0x01000416)).toBe(true)
  })

  test('diffs composition updates', () => {
    expect(textDiff('', 'te')).toEqual({ deletes: 0, insert: 'te' })
    expect(textDiff('tez', 'tes')).toEqual({ deletes: 1, insert: 's' })
    expect(textDiff('hello', 'help')).toEqual({ deletes: 2, insert: 'p' })
  })
})

describe('zoom and pan', () => {
  const element = { left: 0, top: 200, width: 400, height: 225 }
  const viewport = { left: 0, top: 0, width: 400, height: 625 }

  test('keeps the content point under the fingers while scaling', () => {
    const zoom = new ZoomPan(
      () => element,
      () => viewport,
      () => 8,
    )
    zoom.pinchStart({ x: 200, y: 312 }, 100)
    zoom.pinchMove({ x: 200, y: 312 }, 300)
    expect(zoom.scale).toBe(3)
    // content point (200, 112) in element px stays at client (200, 312)
    expect(element.left + zoom.tx + 200 * zoom.scale).toBeCloseTo(200)
    expect(element.top + zoom.ty + 112 * zoom.scale).toBeCloseTo(312)
  })

  test('clamps scale and keeps the viewport covered', () => {
    const zoom = new ZoomPan(
      () => element,
      () => viewport,
      () => 4,
    )
    zoom.pinchStart({ x: 10, y: 210 }, 100)
    zoom.pinchMove({ x: 390, y: 600 }, 1000)
    expect(zoom.scale).toBe(4)
    expect(element.left + zoom.tx).toBeLessThanOrEqual(viewport.left)
    expect(element.left + zoom.tx + element.width * 4).toBeGreaterThanOrEqual(viewport.left + viewport.width)
    expect(element.top + zoom.ty).toBeLessThanOrEqual(viewport.top)
    expect(element.top + zoom.ty + element.height * 4).toBeGreaterThanOrEqual(viewport.top + viewport.height)
  })

  test('never zooms out below the fitted size', () => {
    const zoom = new ZoomPan(
      () => element,
      () => viewport,
      () => 4,
    )
    zoom.pinchStart({ x: 200, y: 312 }, 300)
    zoom.pinchMove({ x: 200, y: 312 }, 100)
    expect(zoom.scale).toBe(1)
    expect(zoom.transform).toBe('')
  })
})

describe('touch gestures', () => {
  const touches = (...points: Point[]) => points.map((p) => ({ clientX: p.x, clientY: p.y })) as unknown as TouchList
  const event = (timeStamp: number, ...points: Point[]) =>
    ({ timeStamp, touches: touches(...points) } as unknown as TouchEvent)

  const recorder = () => {
    const calls: string[] = []
    const handlers = new Proxy({} as GestureHandlers, {
      get:
        (_, name: string) =>
        (...args: unknown[]) =>
          calls.push(`${name} ${JSON.stringify(args)}`),
    })
    return { calls, handlers }
  }

  test('a short touch without movement is a tap', () => {
    const { calls, handlers } = recorder()
    const g = new TouchGestures(handlers)
    g.touchStart(event(0, { x: 10, y: 10 }))
    g.touchMove(event(20, { x: 12, y: 11 }))
    g.touchEnd(event(60))
    g.destroy()
    expect(calls.map((c) => c.split(' ')[0])).toEqual(['onTouchBegin', 'onTap'])
  })

  test('movement past the slop scrolls by the finger delta', () => {
    const { calls, handlers } = recorder()
    const g = new TouchGestures(handlers)
    g.touchStart(event(0, { x: 100, y: 300 }))
    g.touchMove(event(16, { x: 100, y: 280 }))
    g.touchMove(event(32, { x: 100, y: 250 }))
    g.touchEnd(event(200))
    g.destroy()
    const scrolls = calls.filter((c) => c.startsWith('onScroll'))
    expect(scrolls.length).toBe(2)
    expect(scrolls[0]).toContain('[0,-20,')
    expect(scrolls[1]).toContain('[0,-30,')
    expect(calls.some((c) => c.startsWith('onTap'))).toBe(false)
  })

  test('two fingers pinch and never scroll or tap', () => {
    const { calls, handlers } = recorder()
    const g = new TouchGestures(handlers)
    g.touchStart(event(0, { x: 100, y: 300 }))
    g.touchStart(event(10, { x: 100, y: 300 }, { x: 200, y: 300 }))
    g.touchMove(event(26, { x: 80, y: 300 }, { x: 220, y: 300 }))
    g.touchEnd(event(40, { x: 220, y: 300 }))
    g.touchMove(event(56, { x: 260, y: 300 }))
    g.touchEnd(event(70))
    g.destroy()
    const names = calls.map((c) => c.split(' ')[0])
    expect(names).toEqual(['onTouchBegin', 'onPinchStart', 'onPinchMove', 'onPinchEnd'])
  })
})
