<template>
  <div class="touch-controls">
    <button
      ref="control"
      class="keyboard-control"
      data-role="keyboard-toggle"
      :class="[`mode-${mode}`, { dragging: !!dragPoint, faded, open: keyboardOpen }]"
      :style="controlStyle"
      :aria-label="keyboardOpen ? 'Hide keyboard' : 'Keyboard'"
      @touchstart.stop.prevent="onTouchStart"
      @touchmove.stop.prevent="onTouchMove"
      @touchend.stop.prevent="onTouchEnd"
      @touchcancel.stop.prevent="onTouchCancel"
      @click.stop.prevent="toggle"
    >
      <i class="fas fa-keyboard" />
      <span v-if="mode === 'bottom'">{{ keyboardOpen ? 'Hide keyboard' : 'Keyboard' }}</span>
      <span v-else-if="keyboardOpen" class="short">Hide</span>
    </button>
  </div>
</template>

<style lang="scss" scoped>
  .touch-controls {
    position: absolute;
    top: 0;
    right: 0;
    bottom: 0;
    left: 0;
    z-index: 2;
    pointer-events: none;
  }

  .keyboard-control {
    position: absolute;
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 8px;
    min-width: 44px;
    min-height: 44px;
    padding: 0 12px;
    border: 0;
    border-radius: 22px;
    background: rgba($color: #000, $alpha: 0.7);
    color: #fff;
    font-size: 15px;
    font-weight: 600;
    white-space: nowrap;
    cursor: pointer;
    pointer-events: auto;
    touch-action: none;
    -webkit-tap-highlight-color: transparent;
    transition: opacity 300ms ease, left 200ms ease-out, top 200ms ease-out;

    &.dragging {
      transition: opacity 300ms ease;
    }

    // in a side strip the button is only as wide as the strip
    &.mode-left,
    &.mode-right {
      flex-direction: column;
      gap: 2px;
      padding: 6px 0;

      .short {
        font-size: 10px;
      }
    }

    &.open {
      background: #81b300;
      color: #000;
    }

    &.faded {
      opacity: 0.3;
    }

    // over the stream a faded control lets taps through; any touch on the view wakes it
    &.mode-overlay.faded {
      pointer-events: none;
    }
  }
</style>

<script lang="ts">
  import { Component, Prop, Ref, Vue, Watch } from 'vue-property-decorator'
  import {
    ControlFrame,
    ControlMode,
    ControlPosition,
    SafeArea,
    Size,
    dropControl,
    placeControl,
  } from '~/utils/touch-controls'

  const IDLE_FADE_MS = 3000
  const DRAG_SLOP = 6

  interface Point {
    x: number
    y: number
  }

  @Component({ name: 'neko-touch-controls' })
  export default class extends Vue {
    @Ref('control') readonly _control!: HTMLElement

    @Prop({ type: String, required: true }) readonly mode!: ControlMode
    @Prop({ type: Object, required: true }) readonly position!: ControlPosition
    @Prop({ type: Object, required: true }) readonly safeArea!: SafeArea
    @Prop(Boolean) readonly keyboardOpen!: boolean
    @Prop({ type: Number, default: 0 }) readonly areaWidth!: number
    @Prop({ type: Number, default: 0 }) readonly areaHeight!: number
    @Prop({ type: Number, default: 0 }) readonly keyboardInset!: number

    private size: Size = { width: 44, height: 44 }
    private faded = false
    private fadeTimer = 0
    private touchStart: Point | null = null
    // finger offset from the control's top-left corner, so a drag does not jump
    private grab: Point = { x: 0, y: 0 }
    private dragPoint: Point | null = null

    get frame(): ControlFrame {
      return {
        area: { width: this.areaWidth, height: this.areaHeight },
        control: this.size,
        safe: this.safeArea,
        keyboardInset: this.keyboardInset,
      }
    }

    get corner(): Point {
      if (this.dragPoint) {
        return { x: this.dragPoint.x - this.grab.x, y: this.dragPoint.y - this.grab.y }
      }
      return placeControl(this.mode, this.position, this.frame)
    }

    get controlStyle() {
      return { left: `${this.corner.x}px`, top: `${this.corner.y}px` }
    }

    mounted() {
      this.measure()
      this.wake()
    }

    updated() {
      this.measure()
    }

    beforeDestroy() {
      window.clearTimeout(this.fadeTimer)
    }

    @Watch('keyboardOpen')
    onKeyboardOpen() {
      this.wake()
    }

    // the label changes the control's size, which its placement depends on
    measure() {
      const { offsetWidth: width, offsetHeight: height } = this._control
      if (width !== this.size.width || height !== this.size.height) {
        this.size = { width, height }
      }
    }

    // restore full opacity and restart the idle timer
    wake() {
      this.faded = false
      window.clearTimeout(this.fadeTimer)
      this.fadeTimer = window.setTimeout(() => {
        this.faded = !this.keyboardOpen && !this.dragPoint
      }, IDLE_FADE_MS)
    }

    toggle() {
      this.wake()
      this.$emit('toggle')
    }

    localPoint(t: Touch): Point {
      const rect = this.$el.getBoundingClientRect()
      return { x: t.clientX - rect.left, y: t.clientY - rect.top }
    }

    onTouchStart(e: TouchEvent) {
      this.wake()
      const p = this.localPoint(e.changedTouches[0])
      const corner = this.corner
      this.touchStart = p
      this.grab = { x: p.x - corner.x, y: p.y - corner.y }
    }

    onTouchMove(e: TouchEvent) {
      if (!this.touchStart) return
      const p = this.localPoint(e.changedTouches[0])
      if (!this.dragPoint && Math.hypot(p.x - this.touchStart.x, p.y - this.touchStart.y) < DRAG_SLOP) return
      this.dragPoint = p
    }

    // runs inside touchend, so a tap can still raise the soft keyboard
    onTouchEnd() {
      if (!this.dragPoint) {
        this.touchStart = null
        this.toggle()
        return
      }

      const corner = this.corner
      this.touchStart = null
      this.dragPoint = null
      this.$emit('move', dropControl(this.mode, this.position, corner.x, corner.y, this.frame))
      this.wake()
    }

    onTouchCancel() {
      this.touchStart = null
      this.dragPoint = null
    }
  }
</script>
