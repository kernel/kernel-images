import { describe, expect, test } from 'bun:test'
import { isClipboardReadGranted } from '../src/utils/clipboard'

describe('isClipboardReadGranted', () => {
  test('allows reads when permission is granted', async () => {
    expect(await isClipboardReadGranted(async () => ({ state: 'granted' }))).toBe(true)
  })

  test.each(['prompt', 'denied'] as const)('blocks reads when permission is %s', async (state) => {
    expect(await isClipboardReadGranted(async () => ({ state }))).toBe(false)
  })

  test('blocks reads when the permission query is unsupported', async () => {
    expect(
      await isClipboardReadGranted(async () => {
        throw new TypeError('Unsupported permission name')
      }),
    ).toBe(false)
  })
})
