<template>
  <div ref="component" class="video">
    <div ref="player" class="player">
      <div ref="container" class="player-container">
        <video ref="video" playsinline />
        <div class="emotes">
          <template v-for="(emote, index) in emotes">
            <neko-emote :id="index" :key="index" />
          </template>
        </div>
        <textarea
          ref="overlay"
          class="overlay"
          spellcheck="false"
          autocapitalize="off"
          autocorrect="off"
          autocomplete="off"
          tabindex="0"
          data-gramm="false"
          :style="{ pointerEvents: hosting || is_touch_device ? 'auto' : 'none' }"
          @click.stop.prevent
          @contextmenu.stop.prevent
          @mousemove.stop.prevent="onMouseMove"
          @mousedown.stop.prevent="onMouseDown"
          @mouseup.stop.prevent="onMouseUp"
          @mouseenter.stop.prevent="onMouseEnter"
          @mouseleave.stop.prevent="onMouseLeave"
          @touchstart.stop.prevent="onTouchStart"
          @touchmove.stop.prevent="onTouchMove"
          @touchend.stop.prevent="onTouchEnd"
          @touchcancel.stop.prevent="onTouchCancel"
          @input="onOverlayInput"
          @compositionstart="onCompositionStart"
          @compositionend="onCompositionEnd"
          @paste.stop.prevent="onPaste"
          @focus="onOverlayFocus"
          @blur="onOverlayBlur"
        />
        <!-- KERNEL
        <div v-if="!playing && playable" class="player-overlay" @click.stop.prevent="playAndUnmute">
          <i class="fas fa-play-circle" />
        </div>
        <div v-else-if="mutedOverlay && muted" class="player-overlay" @click.stop.prevent="unmute">
          <i class="fas fa-volume-up" />
        </div>
-->
        <div ref="aspect" class="player-aspect" />
      </div>
      <ul v-if="!fullscreen && !hideControls" class="video-menu top">
        <!-- KERNEL: disable fullscreen and resolution controls
        <li><i @click.stop.prevent="requestFullscreen" class="fas fa-expand"></i></li>
        <li v-if="admin"><i @click.stop.prevent="openResolution" class="fas fa-desktop"></i></li>
        -->
        <li
          v-if="!controlLocked && !implicitHosting && !readOnly"
          :class="extraControls || is_touch_device ? '' : 'extra-control'"
        >
          <i
            :class="[
              hosted && !hosting ? 'disabled' : '',
              !hosted && !hosting ? 'faded' : '',
              'fas',
              'fa-computer-mouse',
            ]"
            @click.stop.prevent="toggleControl"
          />
        </li>
      </ul>
      <ul v-if="!fullscreen && !hideControls" class="video-menu bottom">
        <li v-if="hosting && (!clipboard_read_available || !clipboard_write_available)">
          <!-- KERNEL: disable clipboard controls
          <i @click.stop.prevent="openClipboard" class="fas fa-clipboard"></i>
          -->
        </li>
        <li>
          <!-- KERNEL: disable pip
          <i
            v-if="pip_available"
            @click.stop.prevent="requestPictureInPicture"
            v-tooltip="{ content: 'Picture-in-Picture', placement: 'left', offset: 5, boundariesElement: 'body' }"
            class="fas fa-external-link-alt"
          />
          -->
        </li>
      </ul>
      <neko-touch-controls
        v-if="showTouchControls"
        ref="controls"
        :mode="touchLayout.mode"
        :position="controlPosition"
        :safe-area="safeArea"
        :keyboard-open="keyboardOpen"
        :area-width="playerWidth"
        :area-height="playerHeight"
        :keyboard-inset="keyboardInset"
        @toggle="toggleMobileKeyboard"
        @move="onControlMove"
      />
      <button
        v-if="zoomed && !hideControls"
        class="touch-button zoom-chip"
        @touchend.stop.prevent="resetZoom"
        @click.stop.prevent="resetZoom"
      >
        {{ zoomScale.toFixed(1) }}× · Reset
      </button>
      <button
        v-if="typeChip"
        class="touch-button type-chip"
        :style="{ left: `${typeChip.x}px`, top: `${typeChip.y}px` }"
        @touchend.stop.prevent="onTypeChip"
        @click.stop.prevent="onTypeChip"
      >
        <i class="fas fa-keyboard" />
        <span>Tap to type</span>
      </button>
      <neko-resolution ref="resolution" v-if="admin" />
      <neko-clipboard ref="clipboard" v-if="hosting && (!clipboard_read_available || !clipboard_write_available)" />
    </div>
  </div>
</template>

<style lang="scss" scoped>
  .video {
    width: 100%;
    height: 100%;

    .player {
      position: absolute;
      box-sizing: border-box;
      display: flex;
      justify-content: center;
      align-items: center;
      background: #000;
      overflow: hidden;
      touch-action: none;

      .touch-button {
        position: absolute;
        z-index: 2;
        display: flex;
        align-items: center;
        gap: 8px;
        min-height: 44px;
        padding: 0 16px;
        border: 0;
        border-radius: 22px;
        background: rgba($color: #000, $alpha: 0.7);
        color: #fff;
        font-size: 15px;
        font-weight: 600;
        white-space: nowrap;
        cursor: pointer;
        touch-action: manipulation;
        -webkit-tap-highlight-color: transparent;
      }

      .zoom-chip {
        left: calc(12px + env(safe-area-inset-left));
        top: calc(12px + env(safe-area-inset-top));
      }

      .type-chip {
        transform: translateX(-50%);
        background: #81b300;
        color: #000;
      }

      .video-menu {
        position: absolute;
        right: 20px;

        &.top {
          top: 15px;
        }

        &.bottom {
          bottom: 15px;
        }

        li {
          margin: 0 0 10px 0;

          i {
            width: 30px;
            height: 30px;
            background: rgba($color: #fff, $alpha: 0.2);
            border-radius: 5px;
            line-height: 30px;
            font-size: 16px;
            text-align: center;
            color: rgba($color: #fff, $alpha: 0.6);
            cursor: pointer;

            &.faded {
              color: rgba($color: $text-normal, $alpha: 0.4);
            }

            &.disabled {
              color: rgba($color: $style-error, $alpha: 0.4);
            }
          }

          /* usually extra controls are only shown on mobile */
          &.extra-control {
            display: none;
          }
          @media (max-width: 768px) {
            &.extra-control {
              display: block;
            }
          }

          &:last-child {
            margin: 0;
          }
        }
      }

      .player-container {
        position: relative;
        width: 100%;
        max-width: calc(16 / 9 * 100vh);
        transform-origin: 0 0;

        video {
          position: absolute;
          top: 0;
          bottom: 0;
          width: 100%;
          height: 100%;
          display: flex;
          background: #000;

          &::-webkit-media-controls {
            display: none !important;
          }
        }

        .player-overlay,
        .emotes {
          position: absolute;
          top: 0;
          bottom: 0;
          width: 100%;
          height: 100%;
          overflow: hidden;
        }

        .player-overlay {
          background: rgba($color: #000, $alpha: 0.2);
          display: flex;
          justify-content: center;
          align-items: center;
          cursor: pointer;

          i::before {
            font-size: 120px;
            text-align: center;
          }

          &.hidden {
            display: none;
          }
        }

        .overlay {
          position: absolute;
          top: 0;
          bottom: 0;
          width: 100%;
          height: 100%;
          cursor: default;
          outline: 0;
          border: 0;
          color: transparent;
          caret-color: transparent;
          background: transparent;
          resize: none;
          touch-action: none;
          // iOS zooms the page when focusing a field with a smaller font
          font-size: 16px;
        }

        .player-aspect {
          display: block;
          padding-bottom: 56.25%;
        }
      }
    }
  }
</style>

<script lang="ts">
  import { Component, Ref, Watch, Vue, Prop } from 'vue-property-decorator'
  import ResizeObserver from 'resize-observer-polyfill'
  import { elementRequestFullscreen, onFullscreenChange, isFullscreen, lockKeyboard, unlockKeyboard } from '~/utils'
  import { isClipboardReadGranted } from '~/utils/clipboard'
  import { TouchGestures, Point } from '~/utils/touch-gestures'
  import { ZoomPan, Box } from '~/utils/zoom-pan'
  import {
    ControlLayout,
    ControlPosition,
    SafeArea,
    controlLayout,
    decodePosition,
    encodePosition,
  } from '~/utils/touch-controls'
  import { get, set } from '~/utils/localstorage'
  import { CursorImage, CursorKind, cachedCursorKind, classifyCursor } from '~/utils/cursor-shape'
  import {
    XK_BACKSPACE,
    XK_RETURN,
    XK_SHIFT_L,
    charToKeysym,
    keysymNeedsShift,
    needsShift,
    textDiff,
  } from '~/utils/text-input'

  import Emote from './emote.vue'
  import Resolution from './resolution.vue'
  import Clipboard from './clipboard.vue'
  import TouchControls from './touch-controls.vue'

  // @ts-ignore
  import GuacamoleKeyboard from '~/utils/guacamole-keyboard.ts'

  const WHEEL_LINE_HEIGHT = 19
  const SCROLL_SENSITIVITY_BASE = 10
  const INT16_MAX = 32767

  // wheel units per remote pixel of finger travel, so content tracks the finger
  const TOUCH_SCROLL_UNITS_PER_PX = 1.1
  const DOUBLE_TAP_MS = 300
  const DOUBLE_TAP_SLOP = 40
  // a cursor shape unchanged this long after the pointer moved belongs to the new position
  const CURSOR_SETTLE_MS = 200
  // how long a late cursor update can still act on the tap that caused it
  const CURSOR_WAIT_MS = 800
  const TYPE_CHIP_MS = 4000
  // kept in the overlay so soft keyboards that only emit input events still report backspace
  const INPUT_SENTINEL = ' '

  type TapKeyboardMode = 'auto' | 'chip' | 'always' | 'off'

  const CONTROL_POSITION_KEY = 'touch_control_position'

  @Component({
    name: 'neko-video',
    components: {
      'neko-emote': Emote,
      'neko-resolution': Resolution,
      'neko-clipboard': Clipboard,
      'neko-touch-controls': TouchControls,
    },
  })
  export default class extends Vue {
    @Ref('component') readonly _component!: HTMLElement
    @Ref('container') readonly _container!: HTMLElement
    @Ref('overlay') readonly _overlay!: HTMLTextAreaElement
    @Ref('aspect') readonly _aspect!: HTMLElement
    @Ref('player') readonly _player!: HTMLElement
    @Ref('video') readonly _video!: HTMLVideoElement
    @Ref('resolution') readonly _resolution!: Resolution

    private _wheelHandler: ((e: WheelEvent) => void) | null = null
    @Ref('clipboard') readonly _clipboard!: Clipboard
    @Ref('controls') readonly _controls?: TouchControls

    // all controls are hidden (e.g. for cast mode)
    @Prop(Boolean) readonly hideControls!: boolean
    // extra controls are shown (e.g. for embed mode)
    @Prop(Boolean) readonly extraControls!: boolean
    // hide the request-control toggle (input is locked, so it can't do anything)
    @Prop(Boolean) readonly readOnly!: boolean

    private keyboard = GuacamoleKeyboard()
    private pressedMouseButtons = new Set<number>()
    private observer = new ResizeObserver(this.onResize.bind(this))
    private focused = false
    private pastePending = false
    private fullscreen = false
    private mutedOverlay = true
    private isVideoSyncing = false

    private gestures!: TouchGestures
    private zoom!: ZoomPan
    private zoomScale = 1
    private keyboardOpen = false
    private typeChip: Point | null = null
    private typeChipTimer = 0
    private cursorKind: CursorKind = 'unknown'
    private cursorChangedAt = 0
    private cursorSeq = 0
    private keyboardInset = 0
    private touchLayout: ControlLayout = { mode: 'overlay', band: 0 }
    private controlPosition: ControlPosition = decodePosition(get<string>(CONTROL_POSITION_KEY, ''))
    private playerWidth = 0
    private playerHeight = 0
    private safeAreaProbe: HTMLElement | null = null
    private safeArea: SafeArea = { top: 0, bottom: 0, left: 0, right: 0 }
    private touchBeganAt = 0
    private lastTap: { p: Point; at: number } | null = null
    private pendingTap: { p: Point; at: number } | null = null
    private pendingTapTimer = 0
    private scrollRemainder = { x: 0, y: 0 }
    private composing = false
    private composed = ''
    private lastCompositionEnd = { data: '', at: 0 }
    private lastKeydownAt = 0
    private shiftWrapped = new Set<number>()

    get admin() {
      return this.$accessor.user.admin
    }

    get connected() {
      return this.$accessor.connected
    }

    get connecting() {
      return this.$accessor.connecting
    }

    get controlling() {
      return this.$accessor.remote.controlling
    }

    get hosting() {
      return this.$accessor.remote.hosting
    }

    get implicitHosting() {
      return this.$accessor.remote.implicitHosting
    }

    get hosted() {
      return this.$accessor.remote.hosted
    }

    get volume() {
      return this.$accessor.video.volume
    }

    get muted() {
      return this.$accessor.video.muted
    }

    get stream() {
      return this.$accessor.video.stream
    }

    get playing() {
      return this.$accessor.video.playing
    }

    get playable() {
      return this.$accessor.video.playable
    }

    get emotes() {
      return this.$accessor.chat.emotes
    }

    get autoplay() {
      return this.$accessor.settings.autoplay
    }

    // server-side lock
    get controlLocked() {
      return 'control' in this.$accessor.locked && this.$accessor.locked['control'] && !this.$accessor.user.admin
    }

    get locked() {
      return this.readOnly || this.$accessor.remote.locked || (this.controlLocked && (!this.hosting || this.implicitHosting))
    }

    get scroll() {
      return this.$accessor.settings.scroll
    }

    get scroll_invert() {
      return this.$accessor.settings.scroll_invert
    }

    get pip_available() {
      //@ts-ignore
      return typeof document.createElement('video').requestPictureInPicture === 'function'
    }

    get clipboard_read_available() {
      return (
        'clipboard' in navigator &&
        typeof navigator.clipboard.readText === 'function' &&
        // Firefox 122+ incorrectly reports that it can read the clipboard but it can't
        // instead it hangs when reading clipboard, until user clicks on the page
        // and the click itself is not handled by the page at all, also the clipboard
        // reads always fail with "Clipboard read operation is not allowed."
        navigator.userAgent.indexOf('Firefox') == -1
      )
    }

    get clipboard_write_available() {
      return 'clipboard' in navigator && typeof navigator.clipboard.writeText === 'function'
    }

    get clipboard() {
      return this.$accessor.remote.clipboard
    }

    get width() {
      return this.$accessor.video.width
    }

    get height() {
      return this.$accessor.video.height
    }

    get rate() {
      return this.$accessor.video.rate
    }

    get vertical() {
      return this.$accessor.video.vertical
    }

    get horizontal() {
      return this.$accessor.video.horizontal
    }

    get is_touch_device() {
      return (
        // detect if the device has touch support
        ('ontouchstart' in window || navigator.maxTouchPoints > 0) &&
        // the primary input mechanism includes a pointing device of
        // limited accuracy, such as a finger on a touchscreen.
        window.matchMedia('(pointer: coarse)').matches
      )
    }

    get showTouchControls() {
      return this.hosting && this.is_touch_device && !this.hideControls && !this.fullscreen
    }

    get zoomed() {
      return this.zoomScale > 1.01
    }

    // ?tapKeyboard=auto (default) raises the keyboard when a tap lands on text,
    // chip offers a "tap to type" button there instead, always raises it on
    // every tap, off leaves it to the keyboard button
    get tapKeyboard(): TapKeyboardMode {
      const value = new URL(location.href).searchParams.get('tapKeyboard')
      return value === 'chip' || value === 'always' || value === 'off' ? value : 'auto'
    }

    @Watch('width')
    onWidthChanged() {
      this.resetZoom()
      this.onResize()
    }

    @Watch('height')
    onHeightChanged() {
      this.resetZoom()
      this.onResize()
    }

    @Watch('volume')
    onVolumeChanged(volume: number) {
      volume /= 100

      if (this._video && this._video.volume != volume) {
        this._video.volume = volume
      }
    }

    @Watch('muted')
    onMutedChanged(muted: boolean) {
      if (this._video && this._video.muted != muted) {
        this._video.muted = muted

        if (!muted) {
          this.mutedOverlay = false
        }
      }
    }

    @Watch('stream')
    onStreamChanged(stream?: MediaStream) {
      if (!this._video || !stream) {
        return
      }

      if ('srcObject' in this._video) {
        this._video.srcObject = stream
      } else {
        // @ts-ignore
        this._video.src = window.URL.createObjectURL(this.stream) // for older browsers
      }
    }

    @Watch('playing')
    async onPlayingChanged(playing: boolean) {
      // In Safari, native events can fire slightly before the `video.paused` property flips.
      // This anti-echo guard prevents the watcher from fighting the video element's own state changes.
      if (this.isVideoSyncing) return;

      if (this._video && this._video.paused && playing) {
        // if autoplay is disabled, play() will throw an error
        // and we need to properly save the state otherwise we
        // would be thinking we're playing when we're not
        try {
          await this._video.play()
        } catch (err: any) {
          if (!this._video.muted) {
            // video.play() can fail if audio is set due restrictive
            // browsers autoplay policy -> retry with muted audio
            try {
              this.$accessor.video.setMuted(true)
              this._video.muted = true
              await this._video.play()
            } catch (err: any) {
              // if it still fails, we're not playing anything
              this.$accessor.video.pause()
            }
          } else {
            this.$accessor.video.pause()
          }
        }
      }

      if (this._video && !this._video.paused && !playing) {
        this.pause()
      }
    }

    @Watch('clipboard')
    async onClipboardChanged(clipboard: string) {
      if (this.clipboard_write_available) {
        try {
          await navigator.clipboard.writeText(clipboard)
          this.$accessor.remote.setClipboard(clipboard)
        } catch (err: any) {
          this.$log.error(err)
        }
      }
    }

    mounted() {
      this.zoom = new ZoomPan(this.containerBox, this.playerBox, this.maxZoom)
      this.gestures = new TouchGestures({
        onTouchBegin: this.onGestureBegin,
        onTap: this.onGestureTap,
        onLongPress: this.onGestureLongPress,
        onLongPressRelease: this.onGestureLongPressRelease,
        onDragStart: this.onGestureDragStart,
        onDragMove: this.onGestureDragMove,
        onDragEnd: this.onGestureDragEnd,
        onScroll: this.onGestureScroll,
        onPinchStart: this.onGesturePinchStart,
        onPinchMove: this.onGesturePinchMove,
        onPinchEnd: this.onGesturePinchEnd,
      })
      this.$client.on('cursor', this.onCursorImage)

      this._container.addEventListener('resize', this.onResize)
      this.onVolumeChanged(this.volume)
      this.onMutedChanged(this.muted)
      this.onStreamChanged(this.stream)
      this.onResize()

      this.observer.observe(this._component)

      onFullscreenChange(this._player, () => {
        this.fullscreen = isFullscreen()
        this.fullscreen ? lockKeyboard() : unlockKeyboard()
        this.onResize()
      })

      this._video.addEventListener('canplaythrough', () => {
        this.$accessor.video.setPlayable(true)
        if (this.autoplay) {
          this.$nextTick(() => {
            this.$accessor.video.play()
          })
        }
      })

      this._video.addEventListener('ended', () => {
        this.$accessor.video.setPlayable(false)
      })

      this._video.addEventListener('error', (event) => {
        this.$log.error(event.error)
        this.$accessor.video.setPlayable(false)
      })

      this._video.addEventListener('volumechange', () => {
        this.$accessor.video.setMuted(this._video.muted)
        this.$accessor.video.setVolume(this._video.volume * 100)
      })

      this._video.addEventListener('playing', () => {
        this.isVideoSyncing = true
        this.$accessor.video.play()
        this.$nextTick(() => { this.isVideoSyncing = false })
      })

      this._video.addEventListener('pause', () => {
        this.isVideoSyncing = true
        this.$accessor.video.pause()
        this.$nextTick(() => { this.isVideoSyncing = false })
      })

      this._wheelHandler = (e: WheelEvent) => {
        if (!this.hosting) return
        e.preventDefault()
        if (this.locked) return
        this.onWheel(e)
      }
      document.addEventListener('wheel', this._wheelHandler, { passive: false, capture: true })
      window.addEventListener('blur', this.resetKeyboard)
      window.visualViewport?.addEventListener('resize', this.updateKeyboardInset)
      window.visualViewport?.addEventListener('scroll', this.updateKeyboardInset)
      window.addEventListener('pagehide', this.resetKeyboard)
      document.addEventListener('visibilitychange', this.resetKeyboardWhenHidden)

      /* Initialize Guacamole Keyboard */
      this.keyboard.onkeydown = (key: number) => {
        this.unmuteOnInteraction()

        if (!this.hosting || this.locked) {
          return true
        }

        const { ctrl, meta } = this.keyboard.modifiers
        const isPaste = key === 0x0076 && !!(ctrl || meta)

        if (isPaste) {
          // Don't send V keydown yet -- onPaste will sync the clipboard
          // first, then send the keystroke so the remote pastes the
          // correct (freshly-synced) content.
          this.pastePending = true
          return true
        }

        this.lastKeydownAt = performance.now()
        if (this.is_touch_device && keysymNeedsShift(key) && !this.keyboard.modifiers.shift) {
          this.shiftWrapped.add(key)
          this.$client.sendData('keydown', { key: XK_SHIFT_L })
        }

        this.$client.sendData('keydown', { key: this.keyMap(key) })
      }
      this.keyboard.onkeyup = (key: number) => {
        if (!this.hosting || this.locked) {
          return
        }

        if (key === 0x0076 && this.pastePending) {
          this.pastePending = false
          return
        }

        this.$client.sendData('keyup', { key: this.keyMap(key) })
        if (this.shiftWrapped.delete(key)) {
          this.$client.sendData('keyup', { key: XK_SHIFT_L })
        }
      }
      this.keyboard.listenTo(this._overlay)
    }

    beforeDestroy() {
      this.gestures.destroy()
      this.$client.off('cursor', this.onCursorImage)
      window.clearTimeout(this.typeChipTimer)
      window.clearTimeout(this.pendingTapTimer)
      if (this.safeAreaProbe) this.safeAreaProbe.remove()
      if (this._wheelHandler) {
        document.removeEventListener('wheel', this._wheelHandler, { capture: true })
        this._wheelHandler = null
      }
      window.removeEventListener('blur', this.resetKeyboard)
      window.visualViewport?.removeEventListener('resize', this.updateKeyboardInset)
      window.visualViewport?.removeEventListener('scroll', this.updateKeyboardInset)
      window.removeEventListener('pagehide', this.resetKeyboard)
      document.removeEventListener('visibilitychange', this.resetKeyboardWhenHidden)
      this.observer.disconnect()
      this.$accessor.video.setPlayable(false)
      /* Guacamole Keyboard does not provide destroy functions */
    }

    get hasMacOSKbd() {
      return /(Mac|iPhone|iPod|iPad)/i.test(navigator.platform)
    }

    KeyTable = {
      XK_ISO_Level3_Shift: 0xfe03, // AltGr
      XK_Mode_switch: 0xff7e, // Character set switch
      XK_Control_L: 0xffe3, // Left control
      XK_Control_R: 0xffe4, // Right control
      XK_Meta_L: 0xffe7, // Left meta
      XK_Meta_R: 0xffe8, // Right meta
      XK_Alt_L: 0xffe9, // Left alt
      XK_Alt_R: 0xffea, // Right alt
      XK_Super_L: 0xffeb, // Left super
      XK_Super_R: 0xffec, // Right super
    }

    keyMap(key: number): number {
      // Alt behaves more like AltGraph on macOS, so shuffle the
      // keys around a bit to make things more sane for the remote
      // server. This method is used by noVNC, RealVNC and TigerVNC
      // (and possibly others).
      if (this.hasMacOSKbd) {
        switch (key) {
          case this.KeyTable.XK_Meta_L:
            key = this.KeyTable.XK_Control_L
            break
          case this.KeyTable.XK_Super_L:
            key = this.KeyTable.XK_Alt_L
            break
          case this.KeyTable.XK_Super_R:
            key = this.KeyTable.XK_Super_L
            break
          case this.KeyTable.XK_Alt_L:
            key = this.KeyTable.XK_Mode_switch
            break
          case this.KeyTable.XK_Alt_R:
            key = this.KeyTable.XK_ISO_Level3_Shift
            break
        }
      }

      return key
    }

    async play() {
      if (!this._video.paused || !this.playable) {
        return
      }

      try {
        await this._video.play()
        this.onResize()
      } catch (err: any) {
        this.$log.error(err)
      }
    }

    pause() {
      if (this._video.paused || !this.playable) {
        return
      }

      this._video.pause()
    }

    toggle() {
      if (!this.playable) {
        return
      }

      if (!this.playing) {
        this.$accessor.video.play()
      } else {
        this.$accessor.video.pause()
      }
    }

    playAndUnmute() {
      this.$accessor.video.play()
      this.$accessor.video.setMuted(false)
    }

    unmute() {
      this.$accessor.video.setMuted(false)
    }

    // The autoplay policy only lets us unmute inside a real user gesture, and
    // this client hides the unmute overlay. Piggyback on the existing input
    // handlers so the first interaction with the live view unmutes. Guarded on
    // the current state so it is a cheap no-op once unmuted, yet re-applies if a
    // reconnect re-mutes the element. (mousemove is intentionally not used: it
    // is not a user-activation event, so it cannot unlock audio.)
    unmuteOnInteraction() {
      if (this.muted) {
        this.unmute()
      }
    }

    toggleControl() {
      if (!this.playable) {
        return
      }

      this.$accessor.remote.toggle()
    }

    requestControl() {
      this.$accessor.remote.request()
    }

    requestFullscreen() {
      // try to fullscreen player element
      if (elementRequestFullscreen(this._player)) {
        this.onResize()
        return
      }

      // fallback to fullscreen video itself (on mobile devices)
      if (elementRequestFullscreen(this._video)) {
        this.onResize()
        return
      }
    }

    requestPictureInPicture() {
      //@ts-ignore
      this._video.requestPictureInPicture()
      this.onResize()
    }

    openResolution(event: MouseEvent) {
      this._resolution.open(event)
    }

    openClipboard() {
      this._clipboard.open()
    }

    async syncClipboard() {
      if (!this.hosting || this.locked || !this.clipboard_read_available || !window.document.hasFocus()) {
        return
      }

      if (window.self !== window.top && !(await isClipboardReadGranted())) {
        return
      }

      if (!this.hosting || this.locked) return

      try {
        const text = await navigator.clipboard.readText()
        if (!this.hosting || this.locked) return
        if (this.clipboard !== text) {
          this.$accessor.remote.setClipboard(text)
          this.$accessor.remote.sendClipboard(text)
        }
      } catch (err: any) {
        this.$log.error(err)
      }
    }

    sendMousePos(e: MouseEvent) {
      const { w, h } = this.$accessor.video.resolution
      const rect = this._overlay.getBoundingClientRect()

      this.$client.sendData('mousemove', {
        x: Math.round((w / rect.width) * (e.clientX - rect.left)),
        y: Math.round((h / rect.height) * (e.clientY - rect.top)),
      })
    }

    onWheel(e: WheelEvent) {
      this.sendMousePos(e)

      let x = e.deltaX
      let y = e.deltaY

      if (e.deltaMode !== 0) {
        x *= WHEEL_LINE_HEIGHT
        y *= WHEEL_LINE_HEIGHT
      }

      if (this.scroll_invert) {
        x *= -1
        y *= -1
      }

      const sensitivity = this.scroll / SCROLL_SENSITIVITY_BASE
      const dx = Math.max(-INT16_MAX, Math.min(INT16_MAX, Math.round(x * sensitivity)))
      const dy = Math.max(-INT16_MAX, Math.min(INT16_MAX, Math.round(y * sensitivity)))

      if (dx !== 0 || dy !== 0) {
        this.$client.sendData('wheel', { x: dx, y: dy, controlKey: e.ctrlKey || e.metaKey })
      }
    }

    onTouchStart(e: TouchEvent) {
      this.gestures.touchStart(e)
    }

    onTouchMove(e: TouchEvent) {
      this.gestures.touchMove(e)
    }

    onTouchEnd(e: TouchEvent) {
      this.gestures.touchEnd(e)
    }

    onTouchCancel() {
      this.gestures.touchCancel()
    }

    canSendInput() {
      return this.hosting && !this.locked
    }

    // maps a client point on the (possibly zoomed) video to remote screen pixels
    remotePoint(p: Point) {
      const { w, h } = this.$accessor.video.resolution
      const rect = this._overlay.getBoundingClientRect()
      return {
        x: Math.max(0, Math.min(w - 1, Math.round((w / rect.width) * (p.x - rect.left)))),
        y: Math.max(0, Math.min(h - 1, Math.round((h / rect.height) * (p.y - rect.top)))),
      }
    }

    sendPointer(p: Point) {
      this.$client.sendData('mousemove', this.remotePoint(p))
    }

    clickAt(p: Point, button: number) {
      this.sendPointer(p)
      this.$client.sendData('mousedown', { key: button })
      this.$client.sendData('mouseup', { key: button })
    }

    onGestureBegin(p: Point) {
      this.unmuteOnInteraction()
      this.hideTypeChip()
      if (this._controls) this._controls.wake()
      this.touchBeganAt = performance.now()

      if (!this.controlling && this.implicitHosting && !this.locked) {
        this.$accessor.remote.request()
      }
      if (!this.hosting) {
        this.$emit('control-attempt')
      }
      if (this.canSendInput()) {
        this.sendPointer(p)
      }
    }

    onGestureTap(p: Point) {
      const now = performance.now()
      const last = this.lastTap
      this.lastTap = { p, at: now }

      if (
        this.zoomed &&
        last &&
        now - last.at < DOUBLE_TAP_MS &&
        Math.hypot(p.x - last.p.x, p.y - last.p.y) < DOUBLE_TAP_SLOP
      ) {
        this.lastTap = null
        this.resetZoom()
        return
      }

      if (!this.canSendInput()) return
      this.clickAt(p, 1)
      this.keyboardAfterTap(p)
    }

    onGestureLongPress() {
      if (!this.canSendInput()) return
      if (navigator.vibrate) navigator.vibrate(15)
    }

    onGestureLongPressRelease(p: Point) {
      if (!this.canSendInput()) return
      this.clickAt(p, 3)
    }

    onGestureDragStart(p: Point) {
      if (!this.canSendInput()) return
      this.sendPointer(p)
      this.pressedMouseButtons.add(1)
      this.$client.sendData('mousedown', { key: 1 })
    }

    onGestureDragMove(p: Point) {
      if (!this.canSendInput()) return
      this.sendPointer(p)
    }

    onGestureDragEnd(p: Point) {
      if (!this.pressedMouseButtons.has(1)) return
      this.sendPointer(p)
      this.pressedMouseButtons.delete(1)
      this.$client.sendData('mouseup', { key: 1 })
    }

    onGestureScroll(dx: number, dy: number) {
      if (!this.canSendInput()) {
        this.gestures.stopFling()
        return
      }

      // finger travel in remote pixels; content follows the finger, so scroll the opposite way
      const remotePerClient = this.width / this._overlay.getBoundingClientRect().width
      const units = TOUCH_SCROLL_UNITS_PER_PX * remotePerClient
      this.scrollRemainder.x -= dx * units
      this.scrollRemainder.y -= dy * units

      const x = Math.trunc(this.scrollRemainder.x)
      const y = Math.trunc(this.scrollRemainder.y)
      if (x === 0 && y === 0) return
      this.scrollRemainder.x -= x
      this.scrollRemainder.y -= y
      this.$client.sendData('wheel', {
        x: Math.max(-INT16_MAX, Math.min(INT16_MAX, x)),
        y: Math.max(-INT16_MAX, Math.min(INT16_MAX, y)),
      })
    }

    onGesturePinchStart(mid: Point, distance: number) {
      this.hideTypeChip()
      this.zoom.pinchStart(mid, distance)
    }

    onGesturePinchMove(mid: Point, distance: number) {
      this.zoom.pinchMove(mid, distance)
      this.applyZoom(false)
    }

    onGesturePinchEnd() {
      if (!this.zoom.zoomed) this.resetZoom()
    }

    // the container box before the zoom transform, in client coordinates
    containerBox(): Box {
      const player = this._player.getBoundingClientRect()
      return {
        left: player.left + this._container.offsetLeft,
        top: player.top + this._container.offsetTop,
        width: this._container.offsetWidth,
        height: this._container.offsetHeight,
      }
    }

    // the area the zoomed video may fill: the player minus the control band
    playerBox(): Box {
      const { left, top, width, height } = this._player.getBoundingClientRect()
      const { mode, band } = this.touchLayout
      if (mode === 'bottom') return { left, top, width, height: height - band }
      if (mode === 'left') return { left: left + band, top, width: width - band, height }
      if (mode === 'right') return { left, top, width: width - band, height }
      return { left, top, width, height }
    }

    maxZoom() {
      const fit = this.width / Math.max(this._container.offsetWidth, 1)
      return Math.min(8, Math.max(3, fit * 1.5))
    }

    applyZoom(animate: boolean) {
      this._container.style.transition = animate ? 'transform 150ms ease-out' : ''
      this._container.style.transform = this.zoom.transform
      this.zoomScale = this.zoom.scale
    }

    resetZoom() {
      if (!this.zoom) return
      this.zoom.reset()
      this.applyZoom(true)
    }

    // Runs inside touchend, the only place iOS lets focus() raise the keyboard.
    // The remote cursor shape is the signal: Chromium shows an I-beam over text
    // fields (and over selectable page text, which is the false positive), and
    // the pointer was moved to the tap position on touchstart.
    keyboardAfterTap(p: Point) {
      window.clearTimeout(this.pendingTapTimer)
      this.pendingTap = null

      if (this.tapKeyboard === 'off') return
      if (this.tapKeyboard === 'always') {
        this.focusForTyping()
        return
      }

      const now = performance.now()
      const fresh = this.cursorChangedAt >= this.touchBeganAt
      const settled = now - this.touchBeganAt >= CURSOR_SETTLE_MS
      if (fresh || settled) {
        this.applyCursorToKeyboard(p, true)
        return
      }

      // the cursor update for this position may still be in flight
      this.pendingTap = { p, at: now }
      this.pendingTapTimer = window.setTimeout(() => {
        if (!this.pendingTap) return
        const tap = this.pendingTap
        this.pendingTap = null
        this.applyCursorToKeyboard(tap.p, false)
      }, CURSOR_WAIT_MS)
    }

    applyCursorToKeyboard(p: Point, inGesture: boolean) {
      if (this.cursorKind === 'text') {
        if (this.keyboardOpen) return
        if (inGesture && this.tapKeyboard === 'auto') {
          this.focusForTyping()
        } else {
          this.showTypeChip(p)
        }
      } else if (this.cursorKind === 'other' && this.keyboardOpen) {
        this._overlay.blur()
      }
    }

    onCursorImage(image: CursorImage) {
      // classification is async; only the newest cursor image may apply
      const seq = ++this.cursorSeq
      const apply = (kind: CursorKind) => {
        if (seq !== this.cursorSeq) return
        this.cursorKind = kind
        this.cursorChangedAt = performance.now()

        const tap = this.pendingTap
        if (tap && performance.now() - tap.at < CURSOR_WAIT_MS) {
          window.clearTimeout(this.pendingTapTimer)
          this.pendingTap = null
          this.applyCursorToKeyboard(tap.p, false)
        }
      }

      const cached = cachedCursorKind(image)
      if (cached) {
        apply(cached)
      } else {
        classifyCursor(image).then(apply)
      }
    }

    showTypeChip(p: Point) {
      const player = this._player.getBoundingClientRect()
      this.typeChip = {
        x: Math.max(80, Math.min(player.width - 80, p.x - player.left)),
        y: Math.max(8, Math.min(player.height - 52, p.y - player.top - 64)),
      }
      window.clearTimeout(this.typeChipTimer)
      this.typeChipTimer = window.setTimeout(this.hideTypeChip, TYPE_CHIP_MS)
    }

    hideTypeChip() {
      window.clearTimeout(this.typeChipTimer)
      this.typeChip = null
    }

    onTypeChip() {
      this.hideTypeChip()
      this.focusForTyping()
    }

    focusForTyping() {
      this.resetInputSentinel()
      this._overlay.focus({ preventScroll: true })
    }

    // iOS overlays the soft keyboard on the layout viewport instead of resizing
    // it, so lift the keyboard button by the part of the player it covers
    updateKeyboardInset() {
      const viewport = window.visualViewport
      if (!viewport) return
      const covered = this._player.getBoundingClientRect().bottom - (viewport.offsetTop + viewport.height)
      this.keyboardInset = Math.max(0, Math.round(covered))
    }

    toggleMobileKeyboard() {
      if (this.keyboardOpen) {
        this._overlay.blur()
      } else {
        this.focusForTyping()
      }
    }

    resetInputSentinel() {
      if (!this.is_touch_device) return
      this._overlay.value = INPUT_SENTINEL
      this._overlay.setSelectionRange(INPUT_SENTINEL.length, INPUT_SENTINEL.length)
    }

    // Soft keyboards that report keyCode 229 (Android) deliver text only through
    // input events, which the Guacamole keyboard does not handle.
    onOverlayInput(e: Event) {
      if (!this.is_touch_device) return
      const event = e as InputEvent

      if (!this.canSendInput()) {
        if (!this.composing) this.resetInputSentinel()
        return
      }

      if (event.inputType === 'insertCompositionText') {
        this.updateComposition(event.data || '')
        return
      }

      // the Guacamole keyboard already sent this key
      const handledByKeydown = performance.now() - this.lastKeydownAt < 50
      const echoOfComposition =
        event.data === this.lastCompositionEnd.data && performance.now() - this.lastCompositionEnd.at < 50

      if (!handledByKeydown && !echoOfComposition) {
        switch (event.inputType) {
          case 'insertText':
          case 'insertReplacementText':
            this.typeText(event.data || '')
            break
          case 'insertLineBreak':
          case 'insertParagraph':
            this.pressKey(XK_RETURN)
            break
          case 'deleteContentBackward':
            this.pressKey(XK_BACKSPACE)
            break
        }
      }

      if (!this.composing) this.resetInputSentinel()
    }

    onCompositionStart() {
      this.composing = true
      this.composed = ''
    }

    onCompositionEnd(e: CompositionEvent) {
      if (this.canSendInput()) this.updateComposition(e.data || '')
      this.lastCompositionEnd = { data: e.data || '', at: performance.now() }
      this.composing = false
      this.composed = ''
      this.resetInputSentinel()
    }

    updateComposition(text: string) {
      const { deletes, insert } = textDiff(this.composed, text)
      for (let i = 0; i < deletes; i++) this.pressKey(XK_BACKSPACE)
      this.typeText(insert)
      this.composed = text
    }

    typeText(text: string) {
      for (const ch of Array.from(text)) {
        const shift = needsShift(ch) && !this.keyboard.modifiers.shift
        if (shift) this.$client.sendData('keydown', { key: XK_SHIFT_L })
        this.pressKey(charToKeysym(ch))
        if (shift) this.$client.sendData('keyup', { key: XK_SHIFT_L })
      }
    }

    pressKey(key: number) {
      this.$client.sendData('keydown', { key })
      this.$client.sendData('keyup', { key })
    }

    focusOverlay() {
      const focus = () => {
        if (this.hosting && !this.locked) {
          this._overlay.focus()
        }
      }

      focus()
      window.setTimeout(focus, 0)
    }

    onMouseDown(e: MouseEvent) {
      this.unmuteOnInteraction()

      if (!this.controlling && this.implicitHosting && !this.locked) {
        this.$accessor.remote.request()
      }

      if (!this.hosting) {
        this.$emit('control-attempt', e)
      }

      if (!this.hosting || this.locked) {
        return
      }

      this.focusOverlay()

      this.sendMousePos(e)
      this.pressedMouseButtons.add(e.button + 1)
      this.$client.sendData('mousedown', { key: e.button + 1 })
    }

    onMouseUp(e: MouseEvent) {
      if (!this.hosting || this.locked) {
        return
      }

      this.focusOverlay()
      this.sendMousePos(e)
      this.pressedMouseButtons.delete(e.button + 1)
      this.$client.sendData('mouseup', { key: e.button + 1 })
    }

    onMouseMove(e: MouseEvent) {
      if (!this.hosting || this.locked) {
        return
      }

      this.sendMousePos(e)
    }

    onMouseEnter(e: MouseEvent) {
      if (this.hosting && !this.locked) {
        this.$accessor.remote.syncKeyboardModifierState({
          capsLock: e.getModifierState('CapsLock'),
          numLock: e.getModifierState('NumLock'),
          scrollLock: e.getModifierState('ScrollLock'),
        })

        this.syncClipboard()
      }

      this.focused = true
    }

    onMouseLeave(e: MouseEvent) {
      // Keep an invalidated cache until mouse entry synchronizes with the remote.
      if (this.hosting && !this.locked && this.$accessor.remote.keyboardModifierState !== -1) {
        this.$accessor.remote.setKeyboardModifierState({
          capsLock: e.getModifierState('CapsLock'),
          numLock: e.getModifierState('NumLock'),
          scrollLock: e.getModifierState('ScrollLock'),
        })
      }

      this.resetKeyboard()
      this.focused = false
    }

    releaseInput() {
      this.resetKeyboard()
      for (const key of this.pressedMouseButtons) {
        this.$client.sendData('mouseup', { key })
      }
      this.pressedMouseButtons.clear()
    }

    resetKeyboard() {
      this.keyboard.reset()
    }

    resetKeyboardWhenHidden() {
      if (document.hidden) {
        this.resetKeyboard()
      }
    }

    @Watch('connected')
    onConnectedChanged(connected: boolean) {
      if (!connected) {
        this.resetKeyboard()
      }
    }

    async onPaste(event: ClipboardEvent) {
      if (!this.hosting || this.locked) return

      try {
        // Read clipboard text directly from the paste event. This is
        // synchronous and works in every browser — including Safari
        // cross-origin iframes that block navigator.clipboard.readText()
        // when the parent page lacks a Permissions-Policy header.
        const text = event.clipboardData?.getData('text/plain')

        if (text && text !== this.clipboard) {
          this.$accessor.remote.setClipboard(text)
          this.$accessor.remote.sendClipboard(text)
        } else if (!text) {
          await this.syncClipboard()
        }

        // Give the neko server time to write the clipboard text to the Xorg
        // selection before we trigger the paste. The clipboard update arrives
        // via WebSocket while the keystroke travels the WebRTC data channel;
        // without this delay the remote pastes stale content.
        await new Promise((resolve) => setTimeout(resolve, 80))

        if (!this.hosting || this.locked) return

        // Send the full Ctrl+V sequence. We can't rely on Guacamole having
        // captured the original Cmd/Ctrl keydown because Safari may intercept
        // modifier shortcuts before they reach iframe JavaScript.
        const ctrlKey = this.keyMap(0xffe3)
        const vKey = this.keyMap(0x0076)
        this.$client.sendData('keydown', { key: ctrlKey })
        this.$client.sendData('keydown', { key: vKey })
        this.$client.sendData('keyup', { key: vKey })
        this.$client.sendData('keyup', { key: ctrlKey })
      } finally {
        this.pastePending = false
      }
    }

    onOverlayFocus() {
      this.keyboardOpen = true
      if (this.hosting) {
        this.syncClipboard()
      }
    }

    onOverlayBlur() {
      this.keyboardOpen = false
      this.resetKeyboard()
    }

    onResize() {
      const { offsetWidth, offsetHeight } = !this.fullscreen ? this._component : document.body
      if (`${offsetWidth}px` !== this._player.style.width || `${offsetHeight}px` !== this._player.style.height) {
        this.resetZoom()
      }
      this._player.style.width = `${offsetWidth}px`
      this._player.style.height = `${offsetHeight}px`
      this.playerWidth = offsetWidth
      this.playerHeight = offsetHeight

      const aspect = this.horizontal / this.vertical
      const maxWidth = (height: number) => (!this.fullscreen ? Math.min(this.width, aspect * height) : aspect * height)

      // Reserve a band outside the video for the touch controls, shrinking the
      // video slightly if needed; with no room they overlay the stream instead.
      const videoWidth = Math.min(offsetWidth, maxWidth(offsetHeight))
      this.safeArea = this.safeAreaInsets()
      this.touchLayout = this.showTouchControls
        ? controlLayout(
            offsetWidth,
            offsetHeight,
            videoWidth,
            videoWidth / aspect,
            this.safeArea,
            this.controlPosition.side,
          )
        : { mode: 'overlay', band: 0 }
      const { mode, band } = this.touchLayout
      this._player.style.paddingBottom = mode === 'bottom' ? `${band}px` : ''
      this._player.style.paddingLeft = mode === 'left' ? `${band}px` : ''
      this._player.style.paddingRight = mode === 'right' ? `${band}px` : ''

      this._container.style.maxWidth = `${maxWidth(offsetHeight - (mode === 'bottom' ? band : 0))}px`
      this._aspect.style.paddingBottom = `${(this.vertical / this.horizontal) * 100}%`
    }

    safeAreaInsets(): SafeArea {
      if (!this.safeAreaProbe) {
        this.safeAreaProbe = document.createElement('div')
        this.safeAreaProbe.style.cssText =
          'position:fixed;visibility:hidden;pointer-events:none;' +
          'padding:env(safe-area-inset-top) env(safe-area-inset-right) env(safe-area-inset-bottom) env(safe-area-inset-left)'
        document.body.appendChild(this.safeAreaProbe)
      }
      const style = getComputedStyle(this.safeAreaProbe)
      return {
        top: parseFloat(style.paddingTop) || 0,
        bottom: parseFloat(style.paddingBottom) || 0,
        left: parseFloat(style.paddingLeft) || 0,
        right: parseFloat(style.paddingRight) || 0,
      }
    }

    onControlMove(position: ControlPosition) {
      this.controlPosition = position
      set(CONTROL_POSITION_KEY, encodePosition(position))
      this.onResize()
    }

    @Watch('showTouchControls')
    onShowTouchControls() {
      this.$nextTick(this.onResize)
    }

    @Watch('focused')
    @Watch('hosting')
    @Watch('locked')
    onFocus() {
      // focus opens the keyboard on mobile
      if (this.is_touch_device) {
        return
      }

      // in order to capture key events, overlay must be focused
      if (this.focused && this.hosting && !this.locked) {
        this._overlay.focus()
      }
    }
  }
</script>
