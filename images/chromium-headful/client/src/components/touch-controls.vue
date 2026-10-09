<template>
  <div class="touch-controls">
    <button
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
    transition: opacity 300ms ease;

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
  import { Component, Prop, Vue, Watch } from 'vue-property-decorator'
  import { CONTROL_SIZE, ControlMode, ControlPosition, EDGE_MARGIN, dropControl } from '~/utils/touch-controls'

  const IDLE_FADE_MS = 3000
  const DRAG_SLOP = 6
  // gap between a band control and the band edges
  const BAND_GAP = 4

  interface Point {
    x: number
    y: number
  }

  @Component({ name: 'neko-touch-controls' })
  export default class extends Vue {
    @Prop({ type: String, required: true }) readonly mode!: ControlMode
    @Prop({ type: Object, required: true }) readonly position!: ControlPosition
    @Prop(Boolean) readonly keyboardOpen!: boolean
    @Prop({ type: Number, default: 0 }) readonly areaWidth!: number
    @Prop({ type: Number, default: 0 }) readonly areaHeight!: number
    @Prop({ type: Number, default: 0 }) readonly keyboardInset!: number

    private faded = false
    private fadeTimer = 0
    private touchStart: Point | null = null
    private dragPoint: Point | null = null

    // Fractions place the control along its track without measuring it:
    // calc(margin + f * (track - 2 * margin)) shifted back by f of its own size.
    get controlStyle() {
      if (this.dragPoint) {
        return {
          left: `${this.dragPoint.x - CONTROL_SIZE / 2}px`,
          top: `${this.dragPoint.y - CONTROL_SIZE / 2}px`,
        }
      }

      const inset = this.keyboardInset
      if (this.mode === 'bottom') {
        const x = this.position.x
        return {
          left: `calc(${EDGE_MARGIN}px + ${x} * (100% - ${2 * EDGE_MARGIN}px))`,
          bottom: `calc(env(safe-area-inset-bottom) + ${BAND_GAP + inset}px)`,
          transform: `translateX(${-x * 100}%)`,
        }
      }

      const y = this.position.y
      const side = this.mode === 'overlay' ? this.position.side : this.mode
      const gap = this.mode === 'overlay' ? 6 : BAND_GAP
      return {
        [side]: `calc(env(safe-area-inset-${side}) + ${gap}px)`,
        top: `calc(${EDGE_MARGIN}px + ${y} * (100% - ${2 * EDGE_MARGIN + inset}px))`,
        transform: `translateY(${-y * 100}%)`,
      }
    }

    mounted() {
      this.wake()
    }

    beforeDestroy() {
      window.clearTimeout(this.fadeTimer)
    }

    @Watch('keyboardOpen')
    onKeyboardOpen() {
      this.wake()
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
      this.touchStart = this.localPoint(e.changedTouches[0])
    }

    onTouchMove(e: TouchEvent) {
      if (!this.touchStart) return
      const p = this.localPoint(e.changedTouches[0])
      if (!this.dragPoint && Math.hypot(p.x - this.touchStart.x, p.y - this.touchStart.y) < DRAG_SLOP) return
      this.dragPoint = p
    }

    // runs inside touchend, so a tap can still raise the soft keyboard
    onTouchEnd() {
      const drop = this.dragPoint
      this.touchStart = null
      this.dragPoint = null

      if (!drop) {
        this.toggle()
        return
      }

      const height = this.areaHeight - this.keyboardInset
      this.$emit('move', dropControl(this.mode, this.position, drop.x, drop.y, this.areaWidth, height))
      this.wake()
    }

    onTouchCancel() {
      this.touchStart = null
      this.dragPoint = null
    }
  }
</script>
