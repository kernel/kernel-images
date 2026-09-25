import { afterEach, beforeEach, describe, expect, test } from 'bun:test'
import { BaseClient, CONNECT_STAGE_TIMEOUT_MS, RETRY_DELAY_MS } from '../src/neko/base'

const posted: Record<string, any>[] = []
const timers: { id: number; delay: number; run: () => void }[] = []
const realClearTimeout = globalThis.clearTimeout
let nextTimerId = 1

// The client mixes `window.setTimeout` with a bare `clearTimeout`, so the global
// has to be stubbed too for a cancelled retry to actually be cancelled.
function clearTimer(id: number) {
  const index = timers.findIndex((timer) => timer.id === id)
  if (index !== -1) timers.splice(index, 1)
}

function runTimers(delay: number) {
  for (const timer of [...timers]) {
    if (timer.delay !== delay) continue
    clearTimer(timer.id)
    timer.run()
  }
}

function runRetries() {
  runTimers(RETRY_DELAY_MS)
}

let lastPeer: FakePeer | undefined

class FakePeer {
  iceConnectionState = 'new'
  connectionState = 'new'
  signalingState = 'stable'
  onconnectionstatechange: () => void = () => {}
  onsignalingstatechange: () => void = () => {}
  oniceconnectionstatechange: () => void = () => {}
  onicecandidate: (event: RTCPeerConnectionIceEvent) => void = () => {}
  onnegotiationneeded: () => void = () => {}
  ontrack: (event: RTCTrackEvent) => void = () => {}
  async setRemoteDescription() {}
  async setLocalDescription() {}
  async createAnswer() {
    return { sdp: 'v=0' }
  }
  async createOffer() {
    return { sdp: 'v=0' }
  }
  async addIceCandidate() {}
  createDataChannel() {
    return { onerror: () => {}, onmessage: () => {}, onclose: () => {}, close: () => {} }
  }
  close() {}
}

class FakeSocket {
  static OPEN = 1
  readyState = 0
  onopen: () => void = () => {}
  onmessage: (e: MessageEvent) => void = () => {}
  onerror: (e: Event) => void = () => {}
  onclose: (e: CloseEvent) => void = () => {}
  constructor(public url: string) {}
  send() {}
  close() {
    this.readyState = 3
  }
}

class TestClient extends BaseClient {
  reasons: (Error | undefined)[] = []
  protected RECONNECTING() {}
  protected CONNECTING() {}
  protected CONNECTED() {}
  protected DISCONNECTED(reason?: Error) {
    this.reasons.push(reason)
  }
  protected TRACK() {}
  protected DATA() {}

  provide() {
    return this['onMessage']({
      data: JSON.stringify({ event: 'signal/provide', id: 'x', lite: false, ice: [], sdp: 'v=0' }),
    } as MessageEvent)
  }
}

function setPeerConstructor(impl: () => unknown) {
  const ctor = function () {
    lastPeer = (impl() ?? {}) as FakePeer
    return lastPeer
  } as unknown as typeof RTCPeerConnection
  ctor.prototype = { addTransceiver() {} }
  Object.defineProperty(globalThis, 'RTCPeerConnection', { value: ctor, configurable: true })
}

beforeEach(() => {
  posted.length = 0
  timers.length = 0
  lastPeer = undefined
  nextTimerId = 1
  const parent = { postMessage: (m: Record<string, any>) => posted.push(m) }
  Object.defineProperty(globalThis, 'window', {
    value: {
      parent,
      setTimeout: (run: () => void, delay: number) => {
        const id = nextTimerId++
        timers.push({ id, delay, run })
        return id
      },
      clearTimeout: clearTimer,
    },
    configurable: true,
  })
  globalThis.clearTimeout = clearTimer
  Object.defineProperty(globalThis, 'document', { value: { referrer: '' }, configurable: true })
  Object.defineProperty(globalThis, 'WebSocket', { value: FakeSocket, configurable: true })
  setPeerConstructor(() => new FakePeer())
})

afterEach(() => {
  globalThis.clearTimeout = realClearTimeout
  for (const key of ['window', 'document', 'WebSocket', 'RTCPeerConnection']) {
    Reflect.deleteProperty(globalThis, key)
  }
})

describe('live view connect attempts', () => {
  test('a throw from peer construction reaches onDisconnected instead of being discarded', async () => {
    setPeerConstructor(() => {
      throw new Error('RTCPeerConnection blocked')
    })

    const client = new TestClient()
    client.connect('ws://host/ws', 'pw', 'kernel')
    client['_ws']!.readyState = FakeSocket.OPEN
    await client.provide()

    expect(client.reasons.map((r) => r?.message)).toEqual(['RTCPeerConnection blocked'])
    expect(posted).toEqual([
      {
        type: 'KERNEL_CONNECTION_FAILED',
        reason: 'RTCPeerConnection blocked',
        attempts: 1,
        iceConnectionState: 'disconnected',
        connectionState: undefined,
        signalingState: undefined,
        socketOpen: true,
      },
    ])
  })

  test('a socket that closes before any peer is established is retried', () => {
    const client = new TestClient()
    client.connect('ws://host/ws', 'pw', 'kernel')
    client['_ws']!.readyState = FakeSocket.OPEN

    client['_ws']!.onclose({ code: 1005, reason: '' } as CloseEvent)

    expect(posted).toEqual([])
    expect(client['_connectAttempts']).toBe(1)

    runRetries()

    expect(client['_connectAttempts']).toBe(2)
    expect(posted).toEqual([])
  })

  test('a transport failure gives up once the attempt bound is reached', () => {
    const client = new TestClient()
    client.connect('ws://host/ws', 'pw', 'kernel')

    for (let i = 0; i < 3; i++) {
      client['_ws']!.readyState = FakeSocket.OPEN
      client['_ws']!.onclose({ code: 1005, reason: '' } as CloseEvent)
      runRetries()
    }

    expect(posted.map((m) => m.type)).toEqual(['KERNEL_CONNECTION_FAILED'])
    expect(posted[0].attempts).toBe(3)
  })

  test('a transport stage timeout retries silently until the attempt bound is reached', () => {
    const client = new TestClient()
    client.connect('ws://host/ws', 'pw', 'kernel')

    expect(client['_stage']).toBe('transport')

    for (let i = 0; i < 2; i++) {
      runTimers(CONNECT_STAGE_TIMEOUT_MS.transport)
      expect(posted).toEqual([])
      runRetries()
    }

    runTimers(CONNECT_STAGE_TIMEOUT_MS.transport)

    expect(posted.map((m) => m.type)).toEqual(['KERNEL_CONNECTION_TIMEOUT', 'KERNEL_CONNECTION_FAILED'])
    expect(posted.map((m) => m.reason)).toEqual(['transport timeout', 'transport timeout'])
  })

  test('the signaling stage starts when the socket opens and is reported as its own reason', async () => {
    const client = new TestClient()
    client.connect('ws://host/ws', 'pw', 'kernel')

    client['_ws']!.readyState = FakeSocket.OPEN
    client['_ws']!.onopen()

    expect(client['_stage']).toBe('signaling')

    runTimers(CONNECT_STAGE_TIMEOUT_MS.signaling)

    expect(client['_connectAttempts']).toBe(1)
    expect(posted).toEqual([])

    runRetries()

    expect(client['_connectAttempts']).toBe(2)
    expect(posted).toEqual([])
  })

  test('after signal/provide the media stage starts and its timeout is terminal', async () => {
    const client = new TestClient()
    client.connect('ws://host/ws', 'pw', 'kernel')
    client['_ws']!.readyState = FakeSocket.OPEN
    client['_ws']!.onopen()
    await client.provide()

    expect(client['_stage']).toBe('media')

    runTimers(CONNECT_STAGE_TIMEOUT_MS.media)

    expect(posted.map((m) => m.type)).toEqual(['KERNEL_CONNECTION_TIMEOUT', 'KERNEL_CONNECTION_FAILED'])
    expect(posted[0].reason).toBe('media timeout')
    expect(posted[1].attempts).toBe(1)
    expect(client['_connectAttempts']).toBe(1)
  })

  test('ICE reaching checking clears the media stage before its bound', async () => {
    const client = new TestClient()
    client.connect('ws://host/ws', 'pw', 'kernel')
    client['_ws']!.readyState = FakeSocket.OPEN
    client['_ws']!.onopen()
    await client.provide()

    lastPeer!.iceConnectionState = 'checking'
    client['_peer']!.oniceconnectionstatechange()

    expect(client['_timeout']).toBeUndefined()

    runTimers(CONNECT_STAGE_TIMEOUT_MS.media)

    expect(posted).toEqual([])
  })

  test('disconnecting cancels a scheduled retry', () => {
    const client = new TestClient()
    client.connect('ws://host/ws', 'pw', 'kernel')
    client['_ws']!.readyState = FakeSocket.OPEN
    client['_ws']!.onclose({ code: 1005, reason: '' } as CloseEvent)

    expect(client['_retry']).toBeDefined()
    client['disconnect']()
    runRetries()

    expect(client['_connectAttempts']).toBe(1)
    expect(posted).toEqual([])
  })

  test('a disconnect after media started is reported as a disconnect, not a connect failure', () => {
    const client = new TestClient()
    client.connect('ws://host/ws', 'pw', 'kernel')
    client['_ws']!.readyState = FakeSocket.OPEN
    client['_peer'] = {} as RTCPeerConnection
    client['_state'] = 'connected'
    client['onConnected']()

    client['onDisconnected'](new Error('network blip'))

    expect(posted).toEqual([])
    expect(client.reasons.map((r) => r?.message)).toEqual(['network blip'])
  })

  test('gives up after the attempt bound and reports the reason to the parent frame', () => {
    const client = new TestClient()

    for (let i = 0; i < 4; i++) client.connect('ws://host/ws', 'pw', 'kernel')

    expect(posted).toEqual([
      {
        type: 'KERNEL_CONNECTION_FAILED',
        reason: 'live view did not start after 3 attempts',
        attempts: 3,
        iceConnectionState: 'disconnected',
        connectionState: undefined,
        signalingState: undefined,
        socketOpen: false,
      },
    ])
  })

  test('stops opening sockets once it has given up', () => {
    const client = new TestClient()

    for (let i = 0; i < 4; i++) client.connect('ws://host/ws', 'pw', 'kernel')
    const socket = client['_ws']
    client.connect('ws://host/ws', 'pw', 'kernel')

    expect(client['_ws']).toBe(socket)
    expect(posted).toHaveLength(1)
  })

  test('an unsupported browser is terminal rather than retried forever', () => {
    Reflect.deleteProperty(globalThis, 'RTCPeerConnection')
    const client = new TestClient()

    client.connect('ws://host/ws', 'pw', 'kernel')
    client.connect('ws://host/ws', 'pw', 'kernel')

    expect(posted).toHaveLength(1)
    expect(posted[0].reason).toBe('browser does not support webrtc (RTCPeerConnection missing)')
  })

  test('a successful connection clears the attempt count', () => {
    const client = new TestClient()

    client.connect('ws://host/ws', 'pw', 'kernel')
    client.connect('ws://host/ws', 'pw', 'kernel')
    expect(client['_connectAttempts']).toBe(2)

    client['_ws']!.readyState = FakeSocket.OPEN
    client['_peer'] = {} as RTCPeerConnection
    client['_state'] = 'connected'
    client['onConnected']()

    expect(client['_connectAttempts']).toBe(0)
  })
})
