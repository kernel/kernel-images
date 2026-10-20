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
          :autocomplete="is_touch_device ? 'one-time-code' : 'off'"
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
          @keydown="onOverlayKey"
          @keyup="onOverlayKey"
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
        :keyboard-open="keyboardShown"
        :area-width="playerWidth"
        :area-height="playerHeight"
        :keyboard-inset="keyboardInset"
        :copy-offered="copyOffered"
        @toggle="toggleMobileKeyboard"
        @move="onControlMove"
        @copy="copyToClipboard"
      />
      <neko-touch-keys
        v-if="showTouchKeys"
        :top="touchKeysTop"
        :ctrl-latched="ctrlLatched"
        @key="pressTouchKey"
        @ctrl="latchCtrl"
        @paste="pasteFromClipboard"
      />
      <button
        v-if="zoomed && !hideControls"
        class="touch-button zoom-chip"
        @touchend.stop.prevent="resetZoom"
        @click.stop.prevent="resetZoom"
      >
        {{ zoomScale.toFixed(1) }}× · Reset
      </button>
      <div v-if="touchNotice && !hideControls" class="touch-button touch-notice" role="status">{{ touchNotice }}</div>
      <neko-resolution ref="resolution" v-if="admin" />
      <neko-clipboard
        ref="clipboard"
        v-if="hosting && (is_touch_device || !clipboard_read_available || !clipboard_write_available)"
        :touch="is_touch_device"
        @paste="pasteText"
      />
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
      transition: transform 150ms ease-out;

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

      .touch-notice {
        left: 50%;
        top: calc(12px + env(safe-area-inset-top));
        transform: translateX(-50%);
        pointer-events: none;
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
  import { TextInput, XK_BACKSPACE, XK_RETURN, focusNeedsGesture } from '~/utils/text-input'
  import { Clock, TapKeyboard, TapKeyboardMode } from '~/utils/tap-keyboard'
  import { FocusKeyboard } from '~/utils/focus-keyboard'
  import { RemoteField, RemoteFocus, focusUrls } from '~/utils/remote-focus'
  import { SoftKeyboard, isPhone } from '~/utils/soft-keyboard'
  import { DEFAULT_KEYBOARD, applyKeyboard, keyboardFor, setInputMode } from '~/utils/field-keyboard'
  import { TypedText, isModifier, typesIntoOverlay } from '~/utils/typed-text'
  import { keyboardPan, keyboardShrink } from '~/utils/keyboard-pan'

  import Emote from './emote.vue'
  import Resolution from './resolution.vue'
  import Clipboard from './clipboard.vue'
  import TouchControls from './touch-controls.vue'
  import TouchKeys, { TOUCH_KEYS_HEIGHT } from './touch-keys.vue'

  // @ts-ignore
  import GuacamoleKeyboard from '~/utils/guacamole-keyboard.ts'

  const WHEEL_LINE_HEIGHT = 19
  const SCROLL_SENSITIVITY_BASE = 10
  const INT16_MAX = 32767

  // wheel units per remote pixel of finger travel, so content tracks the finger
  const TOUCH_SCROLL_UNITS_PER_PX = 1.1
  const TOUCH_NOTICE_MS = 3000
  // how long Copy waits for the remote clipboard to change after Ctrl+C
  const COPY_WAIT_MS = 2000
  // how long Copy stays on the controls after a selection
  const COPY_OFFER_MS = 8000
  // pasted text up to this length is typed instead of pasted
  const TYPED_PASTE_MAX = 32
  // kept in the overlay so soft keyboards that only emit input events still report backspace
  const INPUT_SENTINEL = ' '

  // a keyboard that goes down this soon after the overlay took focus or changed
  // keyboards is iOS swapping it, not the user dismissing it
  const KEYBOARD_SETTLE_MS = 600

  const CONTROL_POSITION_KEY = 'touch_control_position'

  @Component({
    name: 'neko-video',
    components: {
      'neko-emote': Emote,
      'neko-resolution': Resolution,
      'neko-clipboard': Clipboard,
      'neko-touch-controls': TouchControls,
      'neko-touch-keys': TouchKeys,
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
    private tapKeyboard!: TapKeyboard
    private focusKeyboard!: FocusKeyboard
    private remoteFocus?: RemoteFocus
    private blurUrl?: string
    private softKeyboard!: SoftKeyboard
    // the soft keyboard is up, as far as the viewport shows
    private softKeyboardUp = false
    private keyboardMeasurable = false
    // the overlay has focus, held or not
    private overlayFocused = false
    private keyboardChangedAt = -Infinity
    // the overlay is focused with inputmode="none", waiting to raise the keyboard
    private overlayHeld = false
    // the overlay is being blurred by this client, not dismissed by the user
    private ownBlur = false
    private typedText!: TypedText
    // the overlay keeps the typed text, for autocorrect in a plain text field
    private keepText = false
    // keys typed into the overlay instead of sent, whose keyup is not sent either
    private overlayKeys = new Set<number>()
    private textInput!: TextInput
    private zoom!: ZoomPan
    private zoomScale = 1
    private keyboardOpen = false
    private touchNotice: string | null = null
    private touchNoticeTimer = 0
    // Copy control clipboard writes still waiting for their text
    private copyWrites = 0
    private cursorSeq = 0
    private keyboardInset = 0
    // how far the video is moved up above the soft keyboard, and the last tap
    // measured from the player's top, which the move keeps visible
    private keyboardPan = 0
    private keyboardAnchor: number | null = null
    // the extra keys row's top, in the player's coordinates
    private touchKeysTop = 0
    private ctrlLatched = false
    private copyOffered = false
    private copyOfferTimer = 0
    private touchLayout: ControlLayout = { mode: 'overlay', band: 0 }
    private controlPosition: ControlPosition = decodePosition(get<string>(CONTROL_POSITION_KEY, ''))
    private playerWidth = 0
    private playerHeight = 0
    private safeAreaProbe: HTMLElement | null = null
    private safeArea: SafeArea = { top: 0, bottom: 0, left: 0, right: 0 }
    private scrollRemainder = { x: 0, y: 0 }

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

    // ?tapKeyboard=auto (default) raises the keyboard when a tap focuses a text
    // field, always raises it on every tap, off leaves it to the keyboard button
    get tapKeyboardMode(): TapKeyboardMode {
      const value = new URL(location.href).searchParams.get('tapKeyboard')
      return value === 'always' || value === 'off' ? value : 'auto'
    }

    // the extra keys row sits on the soft keyboard, so it shows only with one
    get showTouchKeys() {
      return this.keyboardShown && this.showTouchControls
    }

    // Whether the soft keyboard is on screen, which the keys row, the keyboard
    // control's Hide and the control's action all follow. On a phone, or once
    // the viewport has shown a keyboard, it is a focused overlay with the
    // keyboard measured up, however the focus came about; a focused overlay
    // with no keyboard, after iOS dismissed it or refused it, does not count.
    // Otherwise, as with a tablet's hardware keyboard, the focus for typing is
    // all there is to go by.
    get keyboardShown() {
      if (!this.keyboardMeasurable) return this.keyboardOpen
      return this.overlayFocused && this.softKeyboardUp
    }

    get inputEnabled() {
      return this.hosting && !this.locked
    }

    // Losing input ends the page's part of a touch instead of each handler
    // checking for it.
    @Watch('inputEnabled')
    onInputEnabledChanged(enabled: boolean) {
      if (enabled || !this.gestures) return
      this.gestures.cancel()
      this.tapKeyboard.cancel()
      this.focusKeyboard.cancel()
    }

    // The focus stream runs while this client can type and is shown, so a
    // phone that is put away does not keep it open.
    @Watch('inputEnabled')
    @Watch('connected')
    updateFocusStream() {
      if (!this.remoteFocus) return
      if (this.inputEnabled && this.connected && !document.hidden) {
        this.remoteFocus.start()
      } else {
        this.remoteFocus.stop()
      }
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
        const playing = !this._video.paused
        this._video.muted = muted

        // Chrome pauses a muted autoplay that it does not allow to unmute, and
        // nothing resumes it, so stay muted and keep the video playing instead.
        if (!muted && playing && this._video.paused) {
          this._video.muted = true
          this.$accessor.video.setMuted(true)
          this._video.play().catch((err: any) => this.$log.error(err))
          return
        }

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
      // a second write would replace the Copy control's pending one
      if (this.clipboard_write_available && this.copyWrites === 0) {
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
      const clock: Clock = {
        now: () => performance.now(),
        setTimeout: (fn, ms) => window.setTimeout(fn, ms),
        clearTimeout: (id) => window.clearTimeout(id),
      }
      this.tapKeyboard = new TapKeyboard(
        this.tapKeyboardMode,
        focusNeedsGesture(navigator),
        {
          keyboardOpen: () => this.keyboardOpen,
          focus: () => this.focusForTyping(null),
          hold: this.holdFocus,
          release: this.blurOverlay,
        },
        clock,
      )
      this.focusKeyboard = new FocusKeyboard(
        this.tapKeyboardMode,
        focusNeedsGesture(navigator),
        {
          keyboardOpen: () => this.keyboardOpen,
          focus: this.focusForTyping,
          update: (field) => {
            this.keyboardChangedAt = performance.now()
            setInputMode(this._overlay, keyboardFor(field).inputmode)
          },
          hold: this.holdFocus,
          release: this.blurOverlay,
        },
        clock,
      )
      const urls = this.is_touch_device ? focusUrls(location) : null
      if (urls) {
        this.blurUrl = urls.blur
        this.remoteFocus = new RemoteFocus(
          urls.stream,
          (url) => new EventSource(url),
          () => performance.now(),
          (state) => this.focusKeyboard.focus(state.focus, state.pageFocused),
        )
        this.updateFocusStream()
      }
      this.textInput = new TextInput(
        (event, key) => {
          this.$client.sendData(event, { key })
          if (event !== 'keydown' || !this.is_touch_device) return
          // the focus stream sees a submit as a blur
          if (key === XK_RETURN && !this.focusLive) this.tapKeyboard.submit()
          // a key from anywhere but the overlay's own edits moved the remote caret or changed its text
          if (this.keepText && !this.typedText.editing && !isModifier(key)) this.resetInputSentinel()
        },
        () => this.is_touch_device,
      )
      this.typedText = new TypedText(INPUT_SENTINEL, {
        type: (text) => this.textInput.type(text),
        backspace: () => this.textInput.key(XK_BACKSPACE),
      })
      this.textInput.onCtrlLatch = (latched) => (this.ctrlLatched = latched)
      // only touch input reads the cursor shape
      if (this.is_touch_device) this.$client.on('cursor', this.onCursorImage)

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
      this.softKeyboard = new SoftKeyboard(isPhone(navigator.userAgent))
      this.keyboardMeasurable = this.softKeyboard.measurable
      this.measureSoftKeyboard()
      // measured first, so the pan and the extra keys row see the keyboard
      window.visualViewport?.addEventListener('resize', this.measureSoftKeyboard)
      window.visualViewport?.addEventListener('resize', this.updateKeyboardView)
      window.visualViewport?.addEventListener('scroll', this.updateKeyboardView)
      window.addEventListener('pagehide', this.resetKeyboard)
      document.addEventListener('visibilitychange', this.resetKeyboardWhenHidden)
      document.addEventListener('visibilitychange', this.updateFocusStream)

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

        // Text and Backspace go into the overlay, where autocorrect sees them,
        // and reach the remote through its edits.
        const modified = ctrl || meta || this.keyboard.modifiers.alt || this.ctrlLatched
        if (this.keepText && !modified && typesIntoOverlay(key)) {
          this.overlayKeys.add(key)
          return true
        }

        this.textInput.keyDown(key, this.keyMap(key))
      }
      this.keyboard.onkeyup = (key: number) => {
        if (!this.hosting || this.locked) {
          return
        }

        if (key === 0x0076 && this.pastePending) {
          this.pastePending = false
          return
        }

        if (this.overlayKeys.delete(key)) return

        this.textInput.keyUp(key, this.keyMap(key))
      }
      this.keyboard.listenTo(this._overlay)
    }

    beforeDestroy() {
      this.gestures.destroy()
      this.tapKeyboard.cancel()
      this.focusKeyboard.cancel()
      this.$client.off('cursor', this.onCursorImage)
      window.clearTimeout(this.touchNoticeTimer)
      window.clearTimeout(this.copyOfferTimer)
      if (this.safeAreaProbe) this.safeAreaProbe.remove()
      if (this._wheelHandler) {
        document.removeEventListener('wheel', this._wheelHandler, { capture: true })
        this._wheelHandler = null
      }
      window.removeEventListener('blur', this.resetKeyboard)
      window.visualViewport?.removeEventListener('resize', this.updateKeyboardView)
      window.visualViewport?.removeEventListener('resize', this.measureSoftKeyboard)
      window.visualViewport?.removeEventListener('scroll', this.updateKeyboardView)
      window.removeEventListener('pagehide', this.resetKeyboard)
      document.removeEventListener('visibilitychange', this.resetKeyboardWhenHidden)
      document.removeEventListener('visibilitychange', this.updateFocusStream)
      if (this.remoteFocus) this.remoteFocus.stop()
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

    openClipboard(hint?: string) {
      if (!this._clipboard) return
      // the panel sits at the top of the player, which the keyboard moves up
      if (this.is_touch_device && this.keyboardOpen) this.blurOverlay()
      this._clipboard.open(hint)
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
      // touchend is a user activation and touchstart is not, so only an unmute
      // here is allowed on Android Chrome
      this.unmuteOnInteraction()
      this.gestures.touchEnd(e)
    }

    onTouchCancel() {
      this.gestures.touchCancel()
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

    // remote pixels per client pixel at the current zoom
    remotePerClient() {
      return this.width / this._overlay.getBoundingClientRect().width
    }

    clickAt(here: Point, button: number) {
      this.$client.sendData('mousemove', here)
      this.$client.sendData('mousedown', { key: button })
      this.$client.sendData('mouseup', { key: button })
    }

    onGestureBegin(p: Point) {
      this.tapKeyboard.touchBegin()
      this.focusKeyboard.touchBegin()
      if (this._controls) this._controls.wake()

      if (!this.controlling && this.implicitHosting && !this.locked) {
        this.$accessor.remote.request()
      }
      if (!this.hosting) {
        this.$emit('control-attempt')
      }
      // without input the touch stays local, where only a pinch zooms
      if (!this.inputEnabled) return false
      this.$client.sendData('mousemove', this.remotePoint(p))
      return true
    }

    onGestureTap(p: Point) {
      const here = this.remotePoint(p)
      this.clickAt(here, 1)
      this.keyboardAnchor = p.y - this._player.getBoundingClientRect().top
      // the click clears the remote selection
      this.withdrawCopy()
      // the click can move the remote caret, which autocorrect must not reach back past
      if (this.keepText && !this.textInput.composing) this.resetInputSentinel()
      // Taps on the page follow the remote focus. The browser's own UI, such
      // as the address bar, and a live view without the focus stream use the
      // cursor shape.
      const stream = this.$accessor.video.resolution
      const focus = this.focusLive ? this.remoteFocus : undefined
      if (focus && focus.inViewport(here, stream)) {
        this.tapKeyboard.cancel()
        this.focusKeyboard.tap(focus.fieldAt(here, stream))
      } else {
        this.focusKeyboard.cursorTap(!!focus)
        this.tapKeyboard.tap(here, this.remotePerClient())
      }
      if (this.keyboardOpen) this.updateKeyboardView()
    }

    onGestureLongPress() {
      if (navigator.vibrate) navigator.vibrate(15)
    }

    onGestureLongPressRelease(p: Point) {
      this.clickAt(this.remotePoint(p), 3)
    }

    onGestureDragStart(p: Point) {
      this.$client.sendData('mousemove', this.remotePoint(p))
      this.pressedMouseButtons.add(1)
      this.$client.sendData('mousedown', { key: 1 })
    }

    onGestureDragMove(p: Point) {
      this.$client.sendData('mousemove', this.remotePoint(p))
    }

    onGestureDragEnd(p: Point) {
      if (!this.pressedMouseButtons.has(1)) return
      this.$client.sendData('mousemove', this.remotePoint(p))
      this.pressedMouseButtons.delete(1)
      this.$client.sendData('mouseup', { key: 1 })
      this.offerCopy()
    }

    // a long-press drag usually selects text, so Copy shows on the controls for a while
    offerCopy() {
      this.copyOffered = true
      window.clearTimeout(this.copyOfferTimer)
      this.copyOfferTimer = window.setTimeout(this.withdrawCopy, COPY_OFFER_MS)
    }

    withdrawCopy() {
      window.clearTimeout(this.copyOfferTimer)
      this.copyOffered = false
    }

    onGestureScroll(dx: number, dy: number) {
      // finger travel in remote pixels; content follows the finger, so scroll the opposite way
      const units = TOUCH_SCROLL_UNITS_PER_PX * this.remotePerClient()
      this.scrollRemainder.x -= dx * units
      this.scrollRemainder.y -= dy * units

      const x = Math.trunc(this.scrollRemainder.x)
      const y = Math.trunc(this.scrollRemainder.y)
      if (x === 0 && y === 0) return
      this.scrollRemainder.x -= x
      this.scrollRemainder.y -= y
      if (this.remoteFocus) this.remoteFocus.scrolled()
      this.$client.sendData('wheel', {
        x: Math.max(-INT16_MAX, Math.min(INT16_MAX, x)),
        y: Math.max(-INT16_MAX, Math.min(INT16_MAX, y)),
      })
    }

    onGesturePinchStart(mid: Point, distance: number) {
      this.tapKeyboard.touchBegin()
      this.focusKeyboard.touchBegin()
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
    // and the part an overlaid soft keyboard covers
    playerBox(): Box {
      const player = this._player.getBoundingClientRect()
      const { left, top, width } = player
      const height = player.height - this.keyboardInset
      const { mode, band } = this.touchLayout
      if (mode === 'bottom') return { left, top, width, height: player.height - Math.max(band, this.keyboardInset) }
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

    onCursorImage(image: CursorImage) {
      // classification is async; only the newest cursor image may apply
      const seq = ++this.cursorSeq
      const apply = (kind: CursorKind) => {
        if (seq === this.cursorSeq) this.tapKeyboard.cursor(kind)
      }

      const cached = cachedCursorKind(image)
      if (cached) {
        apply(cached)
      } else {
        classifyCursor(image).then(apply)
      }
    }

    // The Paste key reads the phone's clipboard inside the tap, where Safari
    // allows it after the user confirms. Where it cannot, the clipboard panel
    // takes the text instead.
    pasteFromClipboard() {
      if (!navigator.clipboard || !navigator.clipboard.readText) {
        this.openClipboard('Paste here, then tap Paste into page')
        return
      }
      navigator.clipboard.readText().then(
        (text) => {
          if (text) {
            this.pasteText(text)
          } else {
            this.showTouchNotice('The clipboard is empty')
          }
        },
        () => this.openClipboard('Clipboard access was not allowed. Paste here, then tap Paste into page'),
      )
    }

    // Short text is typed, which also works in fields that block pasting and in
    // code fields with one box per character. Longer text goes through the
    // remote clipboard and Ctrl+V, as a desktop paste does, once the remote
    // confirms the clipboard; without that it is typed.
    pasteText(text: string) {
      text = text.replace(/[\r\n]+$/, '')
      if (!text || !this.inputEnabled) return
      if (text.length <= TYPED_PASTE_MAX && !/[\r\n]/.test(text)) {
        this.textInput.paste(text)
        return
      }
      this.nextRemoteClipboard()
        .then((remote) => {
          if (remote !== text) throw new Error('the remote clipboard holds other text')
          if (this.inputEnabled) this.pressShortcut(0x0076)
        })
        .catch(() => {
          if (this.inputEnabled) this.textInput.paste(text)
        })
      this.$accessor.remote.setClipboard(text)
      this.$accessor.remote.sendClipboard(text)
    }

    // Ctrl and a letter key, mapped for the remote keyboard
    pressShortcut(keysym: number) {
      if (this.keepText) this.resetInputSentinel()
      const ctrl = this.keyMap(0xffe3)
      const key = this.keyMap(keysym)
      this.$client.sendData('keydown', { key: ctrl })
      this.$client.sendData('keydown', { key })
      this.$client.sendData('keyup', { key })
      this.$client.sendData('keyup', { key: ctrl })
    }

    pressTouchKey(keysym: number) {
      if (this.inputEnabled) this.textInput.key(keysym)
    }

    latchCtrl(latched: boolean) {
      this.textInput.latchCtrl(latched)
    }

    // The Copy control copies the remote selection to the phone. Safari only
    // lets a page write the clipboard inside the tap, so the write starts here
    // with its text pending, and the text arrives when the remote clipboard
    // changes after Ctrl+C.
    copyToClipboard() {
      if (!this.inputEnabled) return
      if (typeof ClipboardItem === 'undefined' || !navigator.clipboard || !navigator.clipboard.write) {
        // the panel shows the remote clipboard once Ctrl+C updates it
        this.pressShortcut(0x0063)
        this.openClipboard('Select the text and copy it')
        return
      }

      let copied = false
      const text = this.nextRemoteClipboard().then((t) => {
        copied = true
        return new Blob([t], { type: 'text/plain' })
      })
      this.copyWrites++
      navigator.clipboard
        .write([new ClipboardItem({ 'text/plain': text })])
        .then(
          () => this.showTouchNotice('Copied'),
          () => {
            if (copied) {
              this.openClipboard('Copying to this device was not allowed. Select the text and copy it')
            } else {
              this.showTouchNotice('Nothing was copied')
            }
          },
        )
        .then(() => this.copyWrites--)

      this.pressShortcut(0x0063)
    }

    // the remote clipboard text from the next update, rejected if none comes in time
    nextRemoteClipboard() {
      return new Promise<string>((resolve, reject) => {
        const onClipboard = (text: string) => {
          window.clearTimeout(timer)
          this.$client.off('clipboard', onClipboard)
          if (text) {
            resolve(text)
          } else {
            reject(new Error('the remote clipboard is empty'))
          }
        }
        const timer = window.setTimeout(() => {
          this.$client.off('clipboard', onClipboard)
          reject(new Error('the remote clipboard did not change'))
        }, COPY_WAIT_MS)
        this.$client.on('clipboard', onClipboard)
      })
    }

    showTouchNotice(text: string) {
      this.touchNotice = text
      window.clearTimeout(this.touchNoticeTimer)
      this.touchNoticeTimer = window.setTimeout(() => (this.touchNotice = null), TOUCH_NOTICE_MS)
    }

    // Raises the keyboard set up for a remote field, or the default keyboard
    // for null. inGesture: inside a tap, where the overlay can be refocused.
    focusForTyping(field: RemoteField | null, inGesture = true) {
      const keyboard = keyboardFor(field)
      this.keyboardChangedAt = performance.now()
      const focused = document.activeElement === this._overlay
      if (focused && this.overlayHeld) {
        // A held focus keeps the attributes it was focused with, and replacing
        // inputmode="none" raises the keyboard.
        this.overlayHeld = false
        setInputMode(this._overlay, keyboard.inputmode)
        this.keepText = false
        this.resetInputSentinel()
        this.onOverlayFocus()
        return
      }
      // Inside a tap the overlay is focused afresh when the keyboard needs other
      // attributes, which it reads on focus, or when the overlay kept its focus
      // without a keyboard on screen, which a focus it already has would not raise.
      const changed = applyKeyboard(this._overlay, keyboard)
      if (focused && inGesture && (changed || !this.keyboardShown)) {
        this.blurOverlay()
        applyKeyboard(this._overlay, keyboard)
      }
      this.keepText = keyboard.keepText
      this.resetInputSentinel()
      if (document.activeElement !== this._overlay) this._overlay.focus({ preventScroll: true })
    }

    // Focuses the overlay inside the tap without showing the keyboard. iOS
    // raises the keyboard when inputmode="none" is removed from an element that
    // was focused inside a gesture, so a text cursor that arrives after the
    // finger lifts can still open it.
    holdFocus() {
      applyKeyboard(this._overlay, DEFAULT_KEYBOARD)
      this._overlay.setAttribute('inputmode', 'none')
      this._overlay.focus({ preventScroll: true })
      this.overlayHeld = document.activeElement === this._overlay
      if (this.overlayHeld) return true
      this._overlay.removeAttribute('inputmode')
      return false
    }

    blurOverlay() {
      this.ownBlur = true
      this._overlay.blur()
      this.ownBlur = false
    }

    // Closes the keyboard and blurs the remote's focused field too, as
    // Safari's Done does, so the next tap on the field is a new focus.
    hideKeyboard() {
      this.blurOverlay()
      this.blurRemote()
    }

    // The field's seq lets the remote drop the blur if the page was tapped
    // again before it arrived.
    blurRemote() {
      const focus = this.focusLive && this.remoteFocus!.state!.focus
      if (!this.blurUrl || !focus || !this.inputEnabled) return
      fetch(`${this.blurUrl}&seq=${focus.seq || 0}`, { method: 'POST', keepalive: true }).catch(() => {})
    }

    // iOS can dismiss its keyboard and leave the overlay focused. That is a
    // Done for the remote too, and the overlay is blurred to match.
    measureSoftKeyboard() {
      const viewport = window.visualViewport
      if (!viewport || !this.is_touch_device) return
      const change = this.softKeyboard.measure(viewport, window.innerHeight)
      this.softKeyboardUp = this.softKeyboard.up
      this.keyboardMeasurable = this.softKeyboard.measurable
      const settled = performance.now() - this.keyboardChangedAt > KEYBOARD_SETTLE_MS
      if (change === 'down' && this.keyboardOpen && document.activeElement === this._overlay && settled) {
        this.hideKeyboard()
      }
    }

    // iOS overlays the soft keyboard on the layout viewport, and some pages
    // shrink instead. Either way the video keeps its size while the keyboard is
    // open and moves up to keep the last tap visible, and the keyboard button
    // is lifted by the part of the player still covered.
    updateKeyboardView() {
      const area = (!this.fullscreen ? this._component : document.body).getBoundingClientRect()
      const viewport = window.visualViewport
      const visibleTop = viewport ? Math.max(area.top, viewport.offsetTop) : area.top
      // the extra keys row sits on the keyboard, so the video stays above both
      const visibleBottom =
        (viewport ? Math.min(area.bottom, viewport.offsetTop + viewport.height) : area.bottom) -
        (this.showTouchKeys ? TOUCH_KEYS_HEIGHT : 0)
      // the layout position, which the pan transform (mid-transition, too) does not move
      const parent = this._player.offsetParent
      const top = (parent ? parent.getBoundingClientRect().top : 0) + this._player.offsetTop
      const height = this._player.offsetHeight
      const pan =
        this.keyboardOpen && this.is_touch_device
          ? keyboardPan({
              top,
              height,
              anchor: this.keyboardAnchor,
              current: this.keyboardPan,
              visibleTop,
              visibleBottom,
            })
          : 0
      if (pan !== this.keyboardPan) {
        this.keyboardPan = pan
        this._player.style.transform = pan ? `translateY(${-pan}px)` : ''
      }
      this.keyboardInset = Math.max(0, Math.round(top + height - pan - visibleBottom))
      this.touchKeysTop = Math.round(visibleBottom - (top - pan))
    }

    toggleMobileKeyboard() {
      if (this.keyboardShown) {
        this.hideKeyboard()
      } else {
        this.focusForTyping(this.focusLive ? this.focusKeyboard.field : null)
        this.focusKeyboard.opened()
      }
    }

    get focusLive() {
      return !!this.remoteFocus && this.remoteFocus.live
    }

    resetInputSentinel() {
      if (!this.is_touch_device) return
      this.typedText.reset()
      this._overlay.value = INPUT_SENTINEL
      this._overlay.setSelectionRange(INPUT_SENTINEL.length, INPUT_SENTINEL.length)
    }

    // Soft keyboards that report keyCode 229 (Android) deliver text only through
    // input events, which the Guacamole keyboard does not handle.
    onOverlayInput(e: Event) {
      if (!this.is_touch_device) return
      if (this.keepText) {
        const kept = this.inputEnabled && this.typedText.edit(this._overlay.value)
        if (!kept && !this.textInput.composing) this.resetInputSentinel()
        return
      }
      if (this.inputEnabled) {
        const event = e as InputEvent
        // AutoFill can put the text in dataTransfer instead of data
        const data = event.data ?? (event.dataTransfer ? event.dataTransfer.getData('text/plain') : null)
        this.textInput.input(event.inputType, data)
      }
      if (!this.textInput.composing) this.resetInputSentinel()
    }

    onOverlayKey(e: KeyboardEvent) {
      this.textInput.keyEvent(e.type === 'keyup')
    }

    onCompositionStart() {
      this.textInput.compositionStart()
    }

    onCompositionEnd(e: CompositionEvent) {
      // the overlay's edits already sent a composition it keeps
      this.textInput.compositionEnd(e.data || '', this.inputEnabled && !this.keepText)
      if (!this.keepText || !this.inputEnabled || !this.typedText.edit(this._overlay.value)) this.resetInputSentinel()
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
        // on touch devices a native paste goes the same way as the Paste key
        if (this.is_touch_device) {
          const text = event.clipboardData?.getData('text/plain')
          if (text) this.pasteText(text)
          return
        }

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
      this.overlayFocused = true
      if (this.tapKeyboard.holding || this.focusKeyboard.holding) return
      this.keyboardOpen = true
      if (!this.hosting) return
      // Reading the clipboard makes iOS show its Paste callout, and hold back
      // the keyboard while it does; on touch devices the Paste control reads it.
      if (this.is_touch_device) {
        this.$accessor.remote.releaseCapsLock()
      } else {
        this.syncClipboard()
      }
    }

    onOverlayBlur() {
      // the keyboard's own Done, or another dismissal by the user; leaving the
      // page for another app or tab keeps the remote's focus
      if (!this.ownBlur && this.keyboardOpen && document.hasFocus()) this.blurRemote()
      this.overlayHeld = false
      this.overlayFocused = false
      if (this.ctrlLatched) this.textInput.latchCtrl(false)
      this.tapKeyboard.blur()
      this.focusKeyboard.blur()
      this._overlay.removeAttribute('inputmode')
      this.keyboardOpen = false
      this.resetKeyboard()
    }

    onResize() {
      const { offsetWidth, offsetHeight } = !this.fullscreen ? this._component : document.body
      const size = { width: offsetWidth, height: offsetHeight }
      if (
        this.is_touch_device &&
        keyboardShrink(this.keyboardOpen, { width: this.playerWidth, height: this.playerHeight }, size)
      ) {
        this.updateKeyboardView()
        return
      }
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
      this.updateKeyboardView()
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

    // the size kept while the keyboard was open is refitted once it closes
    @Watch('keyboardOpen')
    onKeyboardOpenChanged(open: boolean) {
      if (!this.is_touch_device) return
      if (open) {
        this.updateKeyboardView()
      } else {
        this.onResize()
      }
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
