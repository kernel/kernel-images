import { afterEach, beforeEach, describe, expect, test } from 'bun:test'
import { BaseClient, CONNECT_STAGE_TIMEOUT_MS } from '../src/neko/base'

const posted: Record<string, any>[] = []
const timers: { id: number; delay: number; run: () => void }[] = []
const realClearTimeout = globalThis.clearTimeout
let nextTimerId = 1

// The client mixes `window.setTimeout` with a bare `clearTimeout`, so the global
// has to be stubbed too for a cleared stage bound to actually be cancelled.
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

  openSocket() {
    this['_ws']!.readyState = FakeSocket.OPEN
    this['_ws']!.onopen()
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

describe('live view connect failures', () => {
  test('a throw from peer construction is reported with reason peer', async () => {
    setPeerConstructor(() => {
      throw new Error('RTCPeerConnection blocked')
    })

    const client = new TestClient()
    client.connect('ws://host/ws', 'pw', 'kernel')
    client.openSocket()
    await client.provide()

    expect(posted).toEqual([
      {
        type: 'KERNEL_CONNECTION_FAILED',
        reason: 'peer',
        iceConnectionState: 'disconnected',
        connectionState: undefined,
        signalingState: undefined,
        socketOpen: true,
      },
    ])
    expect(client.reasons.map((r) => r?.message)).toEqual(['RTCPeerConnection blocked'])
  })

  test('a socket that closes before any peer is established reports reason transport', () => {
    const client = new TestClient()
    client.connect('ws://host/ws', 'pw', 'kernel')
    client['_ws']!.readyState = FakeSocket.OPEN

    client['_ws']!.onclose({ code: 1005, reason: '' } as CloseEvent)

    expect(posted).toHaveLength(1)
    expect(posted[0].type).toBe('KERNEL_CONNECTION_FAILED')
    expect(posted[0].reason).toBe('transport')
  })

  test('an unsupported browser fails once and opens no socket', () => {
    Reflect.deleteProperty(globalThis, 'RTCPeerConnection')
    const client = new TestClient()

    client.connect('ws://host/ws', 'pw', 'kernel')
    client.connect('ws://host/ws', 'pw', 'kernel')

    expect(posted).toHaveLength(1)
    expect(posted[0].reason).toBe('unsupported')
    expect(client['_ws']).toBeUndefined()
  })

  test('a transport stage timeout reports one event, tagged with the stage', () => {
    const client = new TestClient()
    client.connect('ws://host/ws', 'pw', 'kernel')

    expect(client['_stage']).toBe('transport')

    runTimers(CONNECT_STAGE_TIMEOUT_MS.transport)

    expect(posted.map((m) => m.type)).toEqual(['KERNEL_CONNECTION_TIMEOUT'])
    expect(posted[0].reason).toBe('transport')
  })

  test('the signaling stage starts when the socket opens and keeps its own reason', () => {
    const client = new TestClient()
    client.connect('ws://host/ws', 'pw', 'kernel')
    client.openSocket()

    expect(client['_stage']).toBe('signaling')

    runTimers(CONNECT_STAGE_TIMEOUT_MS.signaling)

    expect(posted.map((m) => m.type)).toEqual(['KERNEL_CONNECTION_TIMEOUT'])
    expect(posted[0].reason).toBe('signaling')
  })

  test('after signal/provide the media stage starts and reports its own reason', async () => {
    const client = new TestClient()
    client.connect('ws://host/ws', 'pw', 'kernel')
    client.openSocket()
    await client.provide()

    expect(client['_stage']).toBe('media')

    runTimers(CONNECT_STAGE_TIMEOUT_MS.media)

    expect(posted).toHaveLength(1)
    expect(posted[0].type).toBe('KERNEL_CONNECTION_TIMEOUT')
    expect(posted[0].reason).toBe('media')
  })

  test('ICE reaching checking clears the stage bound', async () => {
    const client = new TestClient()
    client.connect('ws://host/ws', 'pw', 'kernel')
    client.openSocket()
    await client.provide()

    lastPeer!.iceConnectionState = 'checking'
    client['_peer']!.oniceconnectionstatechange()

    expect(client['_timeout']).toBeUndefined()

    runTimers(CONNECT_STAGE_TIMEOUT_MS.media)

    expect(posted).toEqual([])
  })

  test('a stage timeout is not followed by a second event for the same connect', () => {
    const client = new TestClient()
    client.connect('ws://host/ws', 'pw', 'kernel')
    runTimers(CONNECT_STAGE_TIMEOUT_MS.transport)

    client['onDisconnected'](new Error('late close'))

    expect(posted).toHaveLength(1)
  })

  test('the payload carries the connection state at the moment of failure', async () => {
    const client = new TestClient()
    client.connect('ws://host/ws', 'pw', 'kernel')
    client.openSocket()
    await client.provide()

    runTimers(CONNECT_STAGE_TIMEOUT_MS.media)

    expect(posted[0]).toEqual({
      type: 'KERNEL_CONNECTION_TIMEOUT',
      reason: 'media',
      iceConnectionState: 'new',
      connectionState: 'new',
      signalingState: 'stable',
      socketOpen: true,
    })
    expect('attempts' in posted[0]).toBe(false)
  })

  test('an untagged pre-connect disconnect falls back to reason peer', () => {
    const client = new TestClient()
    client.connect('ws://host/ws', 'pw', 'kernel')

    client['onDisconnected'](new Error('peer failed'))

    expect(posted).toHaveLength(1)
    expect(posted[0].reason).toBe('peer')
  })

  test('a server-initiated disconnect before connecting keeps its own reason', () => {
    const client = new TestClient()
    client.connect('ws://host/ws', 'pw', 'kernel')

    client['_failure'] = 'server'
    client['onDisconnected'](new Error('kicked'))

    expect(posted).toHaveLength(1)
    expect(posted[0].reason).toBe('server')
  })

  test('a disconnect after connecting is reported as a disconnect, not a connect failure', () => {
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

  test('stops opening sockets once it has given up', () => {
    const client = new TestClient()
    client.connect('ws://host/ws', 'pw', 'kernel')
    client['_ws']!.readyState = FakeSocket.OPEN
    client['_ws']!.onclose({ code: 1005, reason: '' } as CloseEvent)

    const socket = client['_ws']
    client.connect('ws://host/ws', 'pw', 'kernel')

    expect(client['_ws']).toBeUndefined()
    expect(socket).toBeUndefined()
    expect(posted).toHaveLength(1)
  })
})
