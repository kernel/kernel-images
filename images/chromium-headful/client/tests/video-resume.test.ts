import { describe, expect, test } from 'bun:test'
import { STALL_MS, VideoResume } from '../src/utils/video-resume'

// A video element under Chrome's autoplay policy: before a user activation it
// may only play muted, and unmuting a playing video pauses it instead.
function chromeVideo() {
  const video = {
    activated: false,
    refuseAll: false,
    paused: false,
    _muted: true,
    currentTime: 3,
    frames: 70,
    srcObject: {} as unknown,
    loads: 0,
    get muted() {
      return this._muted
    },
    set muted(muted: boolean) {
      this._muted = muted
      if (!muted && !this.activated) this.paused = true
    },
    play() {
      if (this.refuseAll || (!this._muted && !this.activated)) return Promise.reject(new Error('NotAllowedError'))
      this.paused = false
      return Promise.resolve()
    },
    getVideoPlaybackQuality() {
      return { totalVideoFrames: this.frames }
    },
  }
  return new Proxy(video, {
    set(target, key, value) {
      if (key === 'srcObject') target.loads++
      return Reflect.set(target, key, value)
    },
  })
}

function resumer(video: ReturnType<typeof chromeVideo>) {
  let now = 0
  const calls: string[] = []
  const resume = new VideoResume(
    video,
    () => now,
    () => calls.push('store muted'),
    (blocked) => calls.push(`blocked ${blocked}`),
  )
  return { resume, calls, tick: (ms: number) => (now += ms) }
}

describe('video resume', () => {
  test('a second tap resumes the video a refused first unmute froze', async () => {
    const video = chromeVideo()
    const { resume, calls } = resumer(video)
    // the old client unmuted on touchstart, which is no user activation
    video.muted = false
    expect(video.paused).toBe(true)
    // the tap's touchend is one, so the next check plays it with sound
    video.activated = true
    await resume.resume()
    expect(video).toMatchObject({ paused: false, muted: false })
    expect(calls).toEqual(['blocked false'])
  })

  test('plays muted when sound is not allowed yet', async () => {
    const video = chromeVideo()
    const { resume, calls } = resumer(video)
    video.muted = false
    await resume.resume()
    expect(video).toMatchObject({ paused: false, muted: true })
    expect(calls).toEqual(['store muted', 'blocked false'])
  })

  test('asks for a tap when even a muted play is refused', async () => {
    const video = chromeVideo()
    const { resume, calls } = resumer(video)
    video.paused = true
    video.refuseAll = true
    await resume.resume()
    expect(calls).toEqual(['blocked true'])
    video.refuseAll = false
    await resume.resume()
    expect(video.paused).toBe(false)
    expect(calls).toEqual(['blocked true', 'blocked false'])
  })

  test('leaves a playing video alone', async () => {
    const video = chromeVideo()
    const { resume, calls, tick } = resumer(video)
    await resume.resume()
    tick(STALL_MS)
    video.frames += 25
    await resume.resume()
    expect(video.loads).toBe(0)
    expect(calls).toEqual([])
  })

  test('reloads a playing video that stopped decoding frames', async () => {
    const video = chromeVideo()
    const { resume, calls, tick } = resumer(video)
    await resume.resume()
    tick(STALL_MS / 2)
    await resume.resume()
    expect(video.loads).toBe(0)
    tick(STALL_MS / 2)
    await resume.resume()
    expect(video.loads).toBe(1)
    expect(calls).toEqual(['blocked false'])
  })

  test('leaves a video alone whose browser counts no frames', async () => {
    // Safari reports no frames for a MediaStream
    const video = chromeVideo()
    video.frames = 0
    const { resume, calls, tick } = resumer(video)
    for (let i = 0; i < 3; i++) {
      await resume.resume()
      tick(STALL_MS)
    }
    expect(video.loads).toBe(0)
    expect(calls).toEqual([])
  })

  test('does nothing without a stream', async () => {
    const video = chromeVideo()
    video.srcObject = null
    video.paused = true
    const { resume, calls } = resumer(video)
    await resume.resume()
    expect(video.paused).toBe(true)
    expect(calls).toEqual([])
  })
})
