import { afterEach, beforeEach, describe, expect, test } from 'bun:test'
import { BaseClient } from '../src/neko/base'

const posted: Record<string, any>[] = []

class FakeSocket {
  static OPEN = 1
  readyState = 0
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
    return impl()
  } as unknown as typeof RTCPeerConnection
  ctor.prototype = { addTransceiver() {} }
  Object.defineProperty(globalThis, 'RTCPeerConnection', { value: ctor, configurable: true })
}

beforeEach(() => {
  posted.length = 0
  const parent = { postMessage: (m: Record<string, any>) => posted.push(m) }
  Object.defineProperty(globalThis, 'window', {
    value: { parent, setTimeout: () => 1, clearTimeout: () => {} },
    configurable: true,
  })
  Object.defineProperty(globalThis, 'document', { value: { referrer: '' }, configurable: true })
  Object.defineProperty(globalThis, 'WebSocket', { value: FakeSocket, configurable: true })
  setPeerConstructor(() => ({}))
})

afterEach(() => {
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

  test('a socket that closes before any peer is established gives up terminally', () => {
    const client = new TestClient()
    client.connect('ws://host/ws', 'pw', 'kernel')
    client['_ws']!.readyState = FakeSocket.OPEN
    client['onDisconnected'](new Error('websocket closed'))

    expect(posted).toHaveLength(1)
    expect(posted[0]).toMatchObject({
      type: 'KERNEL_CONNECTION_FAILED',
      reason: 'websocket closed',
      attempts: 1,
    })
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

  test('a connect timeout reports the legacy timeout and the terminal failure', () => {
    const client = new TestClient()
    client.connect('ws://host/ws', 'pw', 'kernel')
    client['_ws']!.readyState = FakeSocket.OPEN
    client['onTimeout']()

    expect(posted.map((m) => m.type)).toEqual(['KERNEL_CONNECTION_TIMEOUT', 'KERNEL_CONNECTION_FAILED'])
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
