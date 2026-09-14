import { describe, expect, test } from 'bun:test'
import { readFileSync } from 'node:fs'
import { runInNewContext } from 'node:vm'
import ts from 'typescript'
import { parseComponent } from 'vue-template-compiler'

// Load the real component methods without mounting its media player or child components.
const script = parseComponent(readFileSync(new URL('../src/components/video.vue', import.meta.url), 'utf8')).script!
  .content
const compiled = ts.transpileModule(script, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020, experimentalDecorators: true },
}).outputText

function videoMethods(readText: () => Promise<string>, granted = async () => true) {
  const exports: { default?: { prototype: Record<string, Function> } } = {}
  const noDecorator = () => () => undefined
  runInNewContext(compiled, {
    exports,
    require: (name: string) => {
      if (name === 'vue-property-decorator') {
        return {
          Vue: class {},
          Component: () => (type: unknown) => type,
          Ref: noDecorator,
          Watch: noDecorator,
          Prop: noDecorator,
        }
      }
      if (name === '~/utils/clipboard') return { isClipboardReadGranted: granted }
      return {}
    },
    window: { self: {}, top: {}, document: { hasFocus: () => true } },
    navigator: { clipboard: { readText } },
    setTimeout,
  })
  return exports.default!.prototype
}

function context() {
  const sent: string[] = []
  const data: string[] = []
  const ctx = {
    hosting: true,
    locked: false,
    clipboard_read_available: true,
    clipboard: 'old remote clipboard',
    $accessor: {
      remote: {
        setClipboard(text: string) {
          ctx.clipboard = text
        },
        sendClipboard(text: string) {
          sent.push(text)
        },
      },
    },
    $log: {
      error: (error: unknown) => {
        throw error
      },
    },
    $client: { sendData: (event: string) => data.push(event) },
    keyMap: (key: number) => key,
    pastePending: false,
  }
  return { ctx, sent, data }
}

describe('video input during read-only transitions', () => {
  test('does not cache a clipboard read that resolves after locking; paste syncs after unlocking', async () => {
    let resolve!: (text: string) => void
    let begin!: () => void
    const started = new Promise<void>((done) => {
      begin = done
    })
    const methods = videoMethods(
      () =>
        new Promise<string>((done) => {
          resolve = done
          begin()
        }),
    )
    const { ctx, sent, data } = context()
    const pending = methods.syncClipboard.call(ctx)
    await started
    ctx.locked = true
    resolve('new clipboard')
    await pending
    expect(ctx.clipboard).toBe('old remote clipboard')
    expect(sent).toEqual([])

    ctx.locked = false
    await methods.onPaste.call(ctx, { clipboardData: { getData: () => 'new clipboard' } })
    expect(sent).toEqual(['new clipboard'])
    expect(data).toEqual(['keydown', 'keydown', 'keyup', 'keyup'])
  })

  test('does not begin a clipboard read if locked while waiting for permission', async () => {
    let resolve!: (granted: boolean) => void
    let reads = 0
    const methods = videoMethods(
      async () => {
        reads++
        return 'clipboard'
      },
      () =>
        new Promise<boolean>((done) => {
          resolve = done
        }),
    )
    const { ctx } = context()
    const pending = methods.syncClipboard.call(ctx)
    ctx.locked = true
    resolve(true)
    await pending
    expect(reads).toBe(0)
  })

  test('does not send a paste shortcut if locked during the clipboard propagation delay', async () => {
    const methods = videoMethods(async () => '')
    const { ctx, data } = context()
    const pending = methods.onPaste.call(ctx, { clipboardData: { getData: () => 'new clipboard' } })
    ctx.locked = true
    await pending
    expect(data).toEqual([])
  })

  test('mouse entry does not synchronize keyboard modifiers or clipboard while locked', () => {
    const methods = videoMethods(async () => '')
    // These unprovided callbacks would throw if invoked while locked.
    methods.onMouseEnter.call({ hosting: true, locked: true }, {})
  })
})
