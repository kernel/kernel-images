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

function videoPrototype() {
  const exports: { default?: { prototype: Record<string, any> } } = {}
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
      return {}
    },
  })
  return exports.default!.prototype
}

function refusal(name: string) {
  return Object.assign(new Error(name), { name })
}

// The first play() after the stream loads, with the browser's answer to an
// unmuted and a muted play(): null plays, a string rejects with that error.
function firstLoad(options: { touch: boolean; unmuted: string | null; muted: string | null; startMuted?: boolean }) {
  const store: string[] = []
  const video = {
    paused: true,
    muted: !!options.startMuted,
    play() {
      const error = this.muted ? options.muted : options.unmuted
      if (error) return Promise.reject(refusal(error))
      this.paused = false
      return Promise.resolve()
    },
  }
  const component = Object.create(videoPrototype(), {
    is_touch_device: { get: () => options.touch },
  })
  Object.assign(component, {
    _video: video,
    isVideoSyncing: false,
    videoBlocked: false,
    $accessor: {
      video: {
        setMuted: (muted: boolean) => store.push(`muted ${muted}`),
        pause: () => store.push('pause'),
      },
    },
  })
  return { component, video, store, start: () => component.onPlayingChanged(true) }
}

describe('first load autoplay', () => {
  test('asks for a tap on a phone that refuses even a muted play', async () => {
    // iOS Safari in Low Power Mode
    const { component, video, store, start } = firstLoad({
      touch: true,
      unmuted: 'NotAllowedError',
      muted: 'NotAllowedError',
    })
    await start()
    expect(video.paused).toBe(true)
    expect(store).toEqual(['muted true', 'pause'])
    expect(component.videoBlocked).toBe(true)
  })

  test('asks for a tap when an already muted play is refused', async () => {
    const { component, start } = firstLoad({
      touch: true,
      unmuted: 'NotAllowedError',
      muted: 'NotAllowedError',
      startMuted: true,
    })
    await start()
    expect(component.videoBlocked).toBe(true)
  })

  test('plays muted without a prompt when only sound is refused', async () => {
    // iOS and Android browsers outside Low Power Mode
    const { component, video, store, start } = firstLoad({ touch: true, unmuted: 'NotAllowedError', muted: null })
    await start()
    expect(video).toMatchObject({ paused: false, muted: true })
    expect(store).toEqual(['muted true'])
    expect(component.videoBlocked).toBe(false)
  })

  test('plays with sound without a prompt when allowed', async () => {
    const { component, video, store, start } = firstLoad({ touch: true, unmuted: null, muted: null })
    await start()
    expect(video).toMatchObject({ paused: false, muted: false })
    expect(store).toEqual([])
    expect(component.videoBlocked).toBe(false)
  })

  test('shows no prompt for a play interrupted by a new stream', async () => {
    const { component, start } = firstLoad({ touch: true, unmuted: 'AbortError', muted: 'AbortError' })
    await start()
    expect(component.videoBlocked).toBe(false)
  })

  test('shows no prompt on a desktop, where the button cannot resume the video', async () => {
    const { component, start } = firstLoad({ touch: false, unmuted: 'NotAllowedError', muted: 'NotAllowedError' })
    await start()
    expect(component.videoBlocked).toBe(false)
  })

  test('a drop hides the prompt, and a refused play after reconnecting asks again', async () => {
    const { component, start } = firstLoad({ touch: true, unmuted: 'NotAllowedError', muted: 'NotAllowedError' })
    await start()
    expect(component.videoBlocked).toBe(true)

    component.resetKeyboard = () => {}
    component.onConnectedChanged(false)
    expect(component.videoBlocked).toBe(false)

    // the reconnected stream plays again, and iOS refuses it again
    component._video.paused = true
    await start()
    expect(component.videoBlocked).toBe(true)
  })
})
