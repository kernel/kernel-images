// Restarts the live view's video when the browser stopped it and nothing else
// would: Android Chrome pauses a muted autoplay it refuses to unmute, and a
// tab can come back from the background paused. Input still reaches the remote
// over the data channel meanwhile, so the picture just stays on its last frame.
// A video that is not paused but has decoded no frame since a check at least
// STALL_MS earlier is reloaded from its stream. Safari reports no frames for a
// MediaStream (totalVideoFrames stays 0), so a counter that has never reported
// a frame says nothing about a stall.

export const STALL_MS = 1000

export interface ResumableVideo {
  paused: boolean
  muted: boolean
  currentTime: number
  srcObject: unknown
  play(): Promise<void>
  getVideoPlaybackQuality?(): { totalVideoFrames: number }
}

export class VideoResume {
  private progress = NaN
  private progressAt = -Infinity
  private counting = false

  constructor(
    private video: ResumableVideo,
    private now: () => number,
    private mute: () => void,
    // play() was refused even muted, so only a tap can start it
    private onBlocked: (blocked: boolean) => void,
  ) {}

  async resume() {
    const video = this.video
    if (!video.srcObject) return
    if (this.stalled()) {
      // setting the source reloads the element onto the stream's current frames
      video.srcObject = video.srcObject
    } else if (!video.paused) {
      return
    }
    try {
      await video.play()
    } catch {
      if (video.muted) return this.onBlocked(true)
      // without a user activation only a muted video may play
      video.muted = true
      this.mute()
      try {
        await video.play()
      } catch {
        return this.onBlocked(true)
      }
    }
    this.onBlocked(false)
  }

  private stalled() {
    const video = this.video
    const progress = video.getVideoPlaybackQuality
      ? video.getVideoPlaybackQuality().totalVideoFrames
      : video.currentTime
    const at = this.now()
    if (progress > 0) this.counting = true
    if (!this.counting) return false
    if (video.paused || progress !== this.progress) {
      this.progress = progress
      this.progressAt = at
      return false
    }
    if (at - this.progressAt < STALL_MS) return false
    this.progressAt = at
    return true
  }
}
