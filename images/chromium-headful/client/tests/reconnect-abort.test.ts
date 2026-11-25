import { afterEach, expect, test } from 'bun:test'
import { NekoClient } from '../src/neko'

const saved = { document: globalThis.document, window: globalThis.window }

afterEach(() => {
  globalThis.document = saved.document
  globalThis.window = saved.window
})

test('aborting a reconnect attempt closes it and clears its media', () => {
  const calls: string[] = []
  const listen = () => {}
  globalThis.document = { addEventListener: listen } as unknown as Document
  globalThis.window = { addEventListener: listen } as unknown as Window & typeof globalThis
  const reset = (name: string) => () => calls.push(`${name} reset`)
  const vue = {
    $http: { defaults: {} },
    $accessor: {
      setConnected: (v: boolean) => calls.push(`connected ${v}`),
      remote: { reset: reset('remote') },
      user: { reset: reset('user') },
      video: { reset: reset('video') },
      chat: { reset: reset('chat') },
    },
  }
  const client = new NekoClient()
  client.initWithURL(vue as never, 'ws://host/ws')

  let closed = false
  let dropped = false
  const socket = { close: () => (closed = true) }
  client['_ws'] = socket as unknown as WebSocket
  client['onDisconnected'] = () => (dropped = true)

  client['reconnector']['deps'].abort()

  expect(closed).toBe(true)
  expect(client['_ws']).toBeUndefined()
  expect(dropped).toBe(false)
  expect(calls).toContain('video reset')
})
