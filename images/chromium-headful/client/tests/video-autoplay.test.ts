import { expect, test } from 'bun:test'
import { readFileSync } from 'node:fs'
import { runInNewContext } from 'node:vm'
import ts from 'typescript'
import { parseComponent } from 'vue-template-compiler'
import { VideoResume } from '../src/utils/video-resume'

// Exercise the actual component's autoplay watcher and recovery button handler.
const script = parseComponent(readFileSync(new URL('../src/components/video.vue', import.meta.url), 'utf8')).script!
  .content
const compiled = ts.transpileModule(script, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020, experimentalDecorators: true },
}).outputText
const exports: { default?: { prototype: Record<string, any> } } = {}
const noDecorator = () => () => undefined
runInNewContext(compiled, {
  exports,
  require: (name: string) => name === 'vue-property-decorator' ? {
    Vue: class {},
    Component: () => (type: unknown) => type,
    Ref: noDecorator,
    Watch: noDecorator,
    Prop: noDecorator,
  } : {},
})
const methods = exports.default!.prototype

for (const initiallyMuted of [false, true]) {
  test(`desktop autoplay refusal exposes a working resume control (muted=${initiallyMuted})`, async () => {
    let activated = false
    const video = {
      paused: true,
      muted: initiallyMuted,
      currentTime: 0,
      srcObject: {},
      async play() {
        if (!activated) throw new Error('NotAllowedError')
        this.paused = false
      },
    }
    const component = {
      _video: video,
      isVideoSyncing: false,
      is_touch_device: false,
      playable: true,
      videoBlocked: false,
      $accessor: { video: { setMuted: () => {}, pause: () => {} } },
      videoResume: new VideoResume(video, () => 0, () => {}, (blocked) => { component.videoBlocked = blocked }),
    }
    await methods.onPlayingChanged.call(component, true)
    expect(video.paused).toBe(true)
    expect(component.videoBlocked).toBe(true)
    activated = true
    methods.resumeVideo.call(component)
    await Promise.resolve()
    expect(video.paused).toBe(false)
    expect(component.videoBlocked).toBe(false)
  })
}
