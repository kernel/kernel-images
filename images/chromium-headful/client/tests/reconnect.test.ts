import { describe, expect, test } from 'bun:test'
import {
  ProbeResult,
  RECONNECT_MAX_DELAY_MS,
  RECONNECT_STABLE_MS,
  Reconnector,
  probeResult,
  reconnectDelay,
} from '../src/utils/reconnect'

// A reconnector on a fake clock. `probe` answers each attempt's page fetch.
function harness(probe: () => ProbeResult = () => 'ok') {
  let now = 0
  let hidden = false
  let timers: { id: number; at: number; fn: () => void }[] = []
  let nextId = 1
  const events: string[] = []
  const reconnect = new Reconnector({
    now: () => now,
    setTimeout: (fn, ms) => {
      timers.push({ id: nextId, at: now + ms, fn })
      return nextId++
    },
    clearTimeout: (id) => {
      timers = timers.filter((t) => t.id !== id)
    },
    hidden: () => hidden,
    probe: async () => probe(),
    connect: () => events.push(`connect@${now}`),
    onWaiting: (attempt) => events.push(`waiting ${attempt}`),
    onEnded: () => events.push('ended'),
  })
  // runs every timer due within ms, then lets the probe promise settle
  const advance = async (ms: number) => {
    const until = now + ms
    for (;;) {
      const due = timers.filter((t) => t.at <= until).sort((a, b) => a.at - b.at)[0]
      if (!due) break
      timers = timers.filter((t) => t !== due)
      now = due.at
      due.fn()
      await Promise.resolve()
      await Promise.resolve()
    }
    now = until
  }
  const flush = async () => {
    await Promise.resolve()
    await Promise.resolve()
  }
  return {
    reconnect,
    events,
    advance,
    flush,
    setHidden: (h: boolean) => (hidden = h),
    pending: () => timers.length,
    tick: (ms: number) => (now += ms),
  }
}

describe('reconnect backoff', () => {
  test('retries at once, then backs off and caps', () => {
    expect([0, 1, 2, 3, 4].map(reconnectDelay)).toEqual([0, 1000, 2000, 4000, 8000])
    expect(reconnectDelay(5)).toBe(RECONNECT_MAX_DELAY_MS)
    expect(reconnectDelay(50)).toBe(RECONNECT_MAX_DELAY_MS)
  })

  test('the proxy ends the session only with a refusal', () => {
    expect(probeResult(200)).toBe('ok')
    for (const status of [401, 403, 404, 410]) expect(probeResult(status)).toBe('ended')
    // the proxy answers 502 while a browser wakes, and 5xx says nothing about the session
    for (const status of [429, 500, 502, 503]) expect(probeResult(status)).toBe('unknown')
  })
})

describe('reconnector', () => {
  test('a dropped session reconnects right away', async () => {
    const h = harness()
    h.tick(RECONNECT_STABLE_MS)
    h.reconnect.dropped()
    expect(h.reconnect.active).toBe(true)
    await h.advance(0)
    expect(h.events).toEqual(['waiting 1', 'connect@10000'])
  })

  test('attempts that keep failing back off', async () => {
    const h = harness()
    h.tick(RECONNECT_STABLE_MS)
    h.reconnect.dropped()
    await h.advance(0)
    for (let i = 0; i < 3; i++) {
      h.reconnect.dropped()
      await h.advance(10000)
    }
    expect(h.events).toEqual([
      'waiting 1',
      'connect@10000',
      'waiting 2',
      'connect@11000',
      'waiting 3',
      'connect@22000',
      'waiting 4',
      'connect@34000',
    ])
  })

  test('a session that connects and drops at once keeps backing off', async () => {
    const h = harness()
    h.reconnect.succeeded()
    h.reconnect.dropped()
    await h.advance(0)
    expect(h.events).toEqual(['waiting 2'])
    await h.advance(1000)
    expect(h.events).toEqual(['waiting 2', 'connect@1000'])
  })

  test('a session that stayed up starts over', async () => {
    const h = harness()
    h.tick(RECONNECT_STABLE_MS)
    h.reconnect.dropped()
    await h.advance(0)
    h.reconnect.dropped()
    await h.advance(1000)
    h.reconnect.succeeded()
    h.tick(RECONNECT_STABLE_MS)
    h.events.length = 0
    h.reconnect.dropped()
    await h.advance(0)
    expect(h.events).toEqual(['waiting 1', 'connect@21000'])
  })

  test('a hidden page waits, and connects as soon as it is shown', async () => {
    const h = harness()
    h.setHidden(true)
    h.tick(RECONNECT_STABLE_MS)
    h.reconnect.dropped()
    await h.advance(30000)
    expect(h.events).toEqual(['waiting 1'])
    h.setHidden(false)
    h.reconnect.wake()
    await h.flush()
    expect(h.events).toEqual(['waiting 1', 'connect@40000'])
  })

  test('waking skips the backoff', async () => {
    const h = harness()
    h.tick(RECONNECT_STABLE_MS)
    h.reconnect.dropped()
    await h.advance(0)
    h.reconnect.dropped()
    h.reconnect.dropped()
    h.reconnect.wake()
    await h.flush()
    expect(h.events.at(-1)).toBe('connect@10000')
    expect(h.pending()).toBe(0)
  })

  test('waking while an attempt runs does not start another', async () => {
    const h = harness()
    h.tick(RECONNECT_STABLE_MS)
    h.reconnect.dropped()
    await h.advance(0)
    h.reconnect.wake()
    h.reconnect.wake()
    await h.flush()
    expect(h.events.filter((e) => e.startsWith('connect'))).toHaveLength(1)
  })

  test('a refused page ends the session without connecting', async () => {
    const h = harness(() => 'ended')
    h.tick(RECONNECT_STABLE_MS)
    h.reconnect.dropped()
    await h.advance(0)
    expect(h.events).toEqual(['waiting 1', 'ended'])
    expect(h.reconnect.ended).toBe(true)
    h.reconnect.dropped()
    h.reconnect.wake()
    await h.advance(60000)
    expect(h.events).toEqual(['waiting 1', 'ended'])
  })

  test('a network error still attempts the connect', async () => {
    const h = harness(() => 'unknown')
    h.tick(RECONNECT_STABLE_MS)
    h.reconnect.dropped()
    await h.advance(0)
    expect(h.events).toEqual(['waiting 1', 'connect@10000'])
  })

  test('stopping cancels a pending attempt', async () => {
    const h = harness()
    h.tick(RECONNECT_STABLE_MS)
    h.reconnect.dropped()
    await h.advance(0)
    h.reconnect.dropped()
    h.reconnect.stop()
    await h.advance(60000)
    expect(h.events).toEqual(['waiting 1', 'connect@10000', 'waiting 2'])
    expect(h.reconnect.active).toBe(false)
  })
})
