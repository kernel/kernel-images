import { describe, expect, mock, test } from 'bun:test'
import { isReadOnlyMessage } from '../src/utils/read-only'

const accessor = { connected: true }
mock.module('~/store', () => ({ accessor }))
const { state, mutations, actions } = await import('../src/store/remote')

const parent = {} as Window
const origin = 'https://dashboard.example'
const message = (overrides: Partial<MessageEvent> = {}) =>
  ({
    source: parent,
    origin,
    data: { type: 'KERNEL_SET_READ_ONLY', readOnly: true, requestId: '1' },
    ...overrides,
  } as MessageEvent)

describe('read-only parent messages', () => {
  test('accepts both modes from the exact parent and origin', () => {
    for (const readOnly of [true, false]) {
      expect(
        isReadOnlyMessage(
          message({ data: { type: 'KERNEL_SET_READ_ONLY', readOnly, requestId: '1' } }),
          parent,
          origin,
        ),
      ).toBe(true)
    }
  })

  test('accepts messages from existing parents without a request ID', () => {
    expect(isReadOnlyMessage(message({ data: { type: 'KERNEL_SET_READ_ONLY', readOnly: true } }), parent, origin)).toBe(
      true,
    )
  })

  test('rejects unrelated windows, origins, opaque origins, and malformed payloads', () => {
    expect(isReadOnlyMessage(message({ source: {} as Window }), parent, origin)).toBe(false)
    expect(isReadOnlyMessage(message({ origin: 'https://other.example' }), parent, origin)).toBe(false)
    for (const parentOrigin of ['*', 'null'])
      expect(isReadOnlyMessage(message({ origin: parentOrigin }), parent, parentOrigin)).toBe(false)
    for (const data of [
      null,
      undefined,
      'true',
      {},
      { type: 'KERNEL_SET_READ_ONLY', readOnly: 'true', requestId: '1' },
      { type: 'KERNEL_SET_READ_ONLY', readOnly: true, requestId: 123 },
    ]) {
      expect(isReadOnlyMessage(message({ data }), parent, origin)).toBe(false)
    }
  })
})

describe('read-only input state', () => {
  test('locks and unlocks without resetting the current controller', () => {
    const remote = state()
    remote.id = 'controller'
    mutations.setReadOnly(remote, true)
    expect(remote.locked).toBe(true)
    expect(remote.implicitHosting).toBe(false)
    mutations.setReadOnly(remote, false)
    expect(remote.locked).toBe(false)
    expect(remote.implicitHosting).toBe(true)
    expect(remote.id).toBe('controller')
  })

  test('preserves server configuration and read-only mode across reconnects', () => {
    const remote = state()
    mutations.setReadOnly(remote, true)
    mutations.setImplicitHosting(remote, true)
    mutations.reset(remote)
    expect(remote.locked).toBe(true)
    expect(remote.implicitHosting).toBe(false)
    mutations.setImplicitHosting(remote, false)
    mutations.setReadOnly(remote, false)
    expect(remote.implicitHosting).toBe(false)
  })

  test('blocks keyboard modifier writes and cache updates while read-only', () => {
    const remote = state()
    mutations.setReadOnly(remote, true)
    actions.syncKeyboardModifierState({ state: remote } as never, { capsLock: true, numLock: true, scrollLock: false })
    expect(remote.keyboardModifierState).toBe(-1)
  })

  test('blocks clipboard writes even when the locked viewer still holds control', () => {
    const remote = state()
    mutations.setReadOnly(remote, true)
    // Sending would access the absent global $client and fail this test.
    actions.sendClipboard({ state: remote, getters: { hosting: true } } as never, 'secret')
  })
})
