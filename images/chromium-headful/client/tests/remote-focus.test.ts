import { describe, expect, test } from 'bun:test'
import { FocusState, RemoteFocus, STREAM_STALE_MS, focusUrls } from '../src/utils/remote-focus'

describe('focus urls', () => {
  test('sits next to the live view on the browser proxy, with its token', () => {
    expect(
      focusUrls({
        origin: 'https://proxy.example:8443',
        pathname: '/browser/live',
        search: '?jwt=a.b%2Bc&readOnly=false',
      }),
    ).toEqual({
      stream: 'https://proxy.example:8443/browser/kernel/live-view/focus?jwt=a.b%2Bc',
      blur: 'https://proxy.example:8443/browser/kernel/live-view/focus/blur?jwt=a.b%2Bc',
    })
  })

  test('is not used outside the proxy, without a token, or when turned off', () => {
    expect(focusUrls({ origin: 'http://localhost:8080', pathname: '/', search: '' })).toBe(null)
    expect(focusUrls({ origin: 'https://p', pathname: '/browser/live', search: '' })).toBe(null)
    expect(focusUrls({ origin: 'https://p', pathname: '/browser/live', search: '?jwt=x&focusSignal=off' })).toBe(null)
  })
})

// a 390x844 screen shown as a 780x1688 stream, with 87px of browser UI
const state = (over: Partial<FocusState> = {}): FocusState => ({
  focus: null,
  fields: [{ type: 'email', rect: { x: 10, y: 107, w: 100, h: 30 } }],
  viewport: { x: 0, y: 87, w: 390, h: 757 },
  pageFocused: true,
  screen: { w: 390, h: 844 },
  ...over,
})
const stream = { w: 780, h: 1688 }

function setup() {
  let clock = 0
  const states: FocusState[] = []
  const sources: any[] = []
  const focus = new RemoteFocus(
    'https://p/browser/kernel/live-view/focus?jwt=x',
    () => {
      const listeners: Record<string, (e: any) => void> = {}
      const source = {
        onmessage: null as any,
        onerror: null as any,
        closed: false,
        addEventListener: (type: string, fn: (e: any) => void) => (listeners[type] = fn),
        close: () => (source.closed = true),
        send: (s: FocusState) => source.onmessage({ data: JSON.stringify(s) }),
        ping: () => listeners.ping({}),
      }
      sources.push(source)
      return source as any
    },
    () => clock,
    (s) => states.push(s),
  )
  return {
    focus,
    states,
    source: () => sources[sources.length - 1],
    sources,
    advance: (ms: number) => (clock += ms),
  }
}

describe('remote focus', () => {
  test('is live while the stream is current and sees a page', () => {
    const t = setup()
    t.focus.start()
    expect(t.focus.live).toBe(false)
    t.source().send(state({ viewport: null }))
    expect(t.focus.live).toBe(false)
    t.source().send(state())
    expect(t.focus.live).toBe(true)
    expect(t.states).toHaveLength(2)

    // pings keep a quiet stream current
    t.advance(STREAM_STALE_MS - 1)
    t.source().ping()
    t.advance(STREAM_STALE_MS - 1)
    expect(t.focus.live).toBe(true)
    t.advance(2)
    expect(t.focus.live).toBe(false)
  })

  test('an error forgets the state without reporting a blur', () => {
    const t = setup()
    t.focus.start()
    t.source().send(state({ focus: { type: 'email' } }))
    t.source().onerror({})
    expect(t.focus.live).toBe(false)
    expect(t.states.map((s) => s.focus)).toEqual([{ type: 'email' }])
  })

  test('finds the page and fields under a tap in stream pixels', () => {
    const t = setup()
    t.focus.start()
    t.source().send(state())
    // the address bar
    expect(t.focus.inViewport({ x: 300, y: 100 }, stream)).toBe(false)
    expect(t.focus.inViewport({ x: 300, y: 400 }, stream)).toBe(true)
    expect(t.focus.fieldAt({ x: 40, y: 230 }, stream)).toEqual({
      type: 'email',
      rect: { x: 10, y: 107, w: 100, h: 30 },
    })
    // a little outside the edge still counts
    expect(t.focus.fieldAt({ x: 222, y: 230 }, stream)).not.toBe(null)
    expect(t.focus.fieldAt({ x: 240, y: 230 }, stream)).toBe(null)
  })

  test('the focused field counts even when it is not among the fields a tap reaches', () => {
    const t = setup()
    t.focus.start()
    // a site's own suggestion list covers the focused box's center
    t.source().send(state({ fields: [], focus: { type: 'search', rect: { x: 10, y: 107, w: 100, h: 30 } } }))
    expect(t.focus.fieldAt({ x: 40, y: 230 }, stream)).toEqual({
      type: 'search',
      rect: { x: 10, y: 107, w: 100, h: 30 },
    })
  })

  test('ignores rects from before a scroll until new ones arrive or the scroll has settled', () => {
    const t = setup()
    t.focus.start()
    t.source().send(state())
    t.advance(100)
    t.focus.scrolled()
    expect(t.focus.fieldAt({ x: 40, y: 230 }, stream)).toBe(null)
    // a report with the same fields says nothing about the scroll
    t.advance(100)
    t.source().send(state({ focus: { type: 'email' } }))
    expect(t.focus.fieldAt({ x: 40, y: 230 }, stream)).toBe(null)
    t.source().send(state({ fields: [{ type: 'email', rect: { x: 10, y: 307, w: 100, h: 30 } }] }))
    expect(t.focus.fieldAt({ x: 40, y: 630 }, stream)).not.toBe(null)

    // a scroll that moved nothing
    t.focus.scrolled()
    t.advance(1001)
    expect(t.focus.fieldAt({ x: 40, y: 630 }, stream)).not.toBe(null)
  })

  test('a new rect for the focused field settles a scroll, even when it is not in fields', () => {
    const t = setup()
    t.focus.start()
    const focus = (y: number) => ({ type: 'search', rect: { x: 10, y, w: 100, h: 30 } })
    t.source().send(state({ fields: [], focus: focus(107) }))
    t.advance(100)
    t.focus.scrolled()
    t.advance(100)
    t.source().send(state({ fields: [], focus: focus(307) }))
    expect(t.focus.fieldAt({ x: 40, y: 630 }, stream)).not.toBe(null)
  })

  test('stop closes the stream and start opens a new one', () => {
    const t = setup()
    t.focus.start()
    t.focus.start()
    expect(t.sources).toHaveLength(1)
    t.source().send(state())
    t.focus.stop()
    expect(t.sources[0].closed).toBe(true)
    expect(t.focus.live).toBe(false)
    t.focus.start()
    expect(t.sources).toHaveLength(2)
  })
})
