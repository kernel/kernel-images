import { beforeEach, describe, expect, mock, test } from 'bun:test'

mock.module('~/store', () => ({ accessor: {} }))
const { NekoClient } = await import('../src/neko/index')

const reset = { reset: mock() }
let accessor: Record<string, any>

function client() {
  const neko = new NekoClient()
  neko['$accessor'] = accessor as any
  neko['$vue'] = { $notify: mock(), $swal: mock(), $t: (key: string) => key } as any
  return neko
}

beforeEach(() => {
  accessor = {
    setConnected: mock(),
    setSessionGone: mock(),
    remote: reset,
    user: reset,
    video: reset,
    chat: reset,
  }
})

// The overlay says the browser shut down only when the proxy said so.
describe('disconnect overlay state', () => {
  test('a session-gone close is recorded as gone', () => {
    const neko = client()
    neko['_sessionGone'] = true

    neko['DISCONNECTED'](new Error('websocket closed'))

    expect(accessor.setSessionGone).toHaveBeenCalledWith(true)
  })

  test('any other disconnect is recorded as not gone', () => {
    const neko = client()

    neko['DISCONNECTED'](new Error('peer failed'))

    expect(accessor.setSessionGone).toHaveBeenCalledWith(false)
  })
})

describe('server disconnect messages', () => {
  test('neko shutting down is left to the socket close', () => {
    const neko = client()
    neko['onDisconnected'] = mock()

    neko['system/disconnect']({ message: 'connection shutdown' })

    expect(neko['_failure']).toBeUndefined()
    expect(neko['onDisconnected']).not.toHaveBeenCalled()
    expect(neko['$vue'].$swal).not.toHaveBeenCalled()
  })

  test('any other reason ends the viewer as a server disconnect', () => {
    const neko = client()
    neko['onDisconnected'] = mock()

    neko['system/disconnect']({ message: 'connection replaced' })

    expect(neko['_failure']).toBe('server')
    expect(neko['onDisconnected']).toHaveBeenCalledTimes(1)
  })
})
