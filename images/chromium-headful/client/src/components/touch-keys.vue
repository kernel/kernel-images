<template>
  <div class="touch-keys" :style="{ top: `${top}px` }" @touchstart.stop.prevent @mousedown.prevent>
    <button
      v-for="key in keys"
      :key="key.label"
      class="touch-key"
      :class="{ latched: key.ctrl && ctrlLatched }"
      :aria-label="key.label"
      :aria-pressed="key.ctrl ? String(ctrlLatched) : undefined"
      @touchend.stop.prevent="press(key)"
      @click.stop.prevent="press(key)"
    >
      <i v-if="key.icon" :class="['fas', key.icon]" />
      <span v-else>{{ key.label }}</span>
    </button>
  </div>
</template>

<style lang="scss" scoped>
  .touch-keys {
    position: absolute;
    left: 0;
    right: 0;
    z-index: 3;
    display: flex;
    gap: 4px;
    height: 44px;
    padding: 0 calc(4px + env(safe-area-inset-right)) 0 calc(4px + env(safe-area-inset-left));
    box-sizing: border-box;
    background: rgba($color: #000, $alpha: 0.8);
    touch-action: none;
  }

  .touch-key {
    flex: 1;
    min-width: 0;
    margin: 4px 0;
    border: 0;
    border-radius: 6px;
    background: rgba($color: #fff, $alpha: 0.15);
    color: #fff;
    font-size: 14px;
    font-weight: 600;
    -webkit-tap-highlight-color: transparent;

    &.latched {
      background: #81b300;
      color: #000;
    }
  }
</style>

<script lang="ts">
  import { Component, Prop, Vue } from 'vue-property-decorator'

  export interface TouchKey {
    label: string
    icon?: string
    keysym?: number
    ctrl?: boolean
    paste?: boolean
  }

  export const TOUCH_KEYS_HEIGHT = 44

  export const TOUCH_KEYS: TouchKey[] = [
    { label: 'Esc', keysym: 0xff1b },
    { label: 'Tab', keysym: 0xff09 },
    { label: 'Ctrl', ctrl: true },
    { label: 'Left', icon: 'fa-arrow-left', keysym: 0xff51 },
    { label: 'Up', icon: 'fa-arrow-up', keysym: 0xff52 },
    { label: 'Down', icon: 'fa-arrow-down', keysym: 0xff54 },
    { label: 'Right', icon: 'fa-arrow-right', keysym: 0xff53 },
    { label: 'Paste', icon: 'fa-paste', paste: true },
  ]

  // A row of keys soft keyboards lack, shown above the soft keyboard. Every key
  // acts on touchend, inside the tap, which reading the clipboard for Paste
  // needs, and never takes focus, so the keyboard stays open.
  @Component({ name: 'neko-touch-keys' })
  export default class extends Vue {
    @Prop({ type: Number, required: true }) readonly top!: number
    @Prop(Boolean) readonly ctrlLatched!: boolean

    readonly keys = TOUCH_KEYS

    press(key: TouchKey) {
      if (key.paste) {
        this.$emit('paste')
      } else if (key.ctrl) {
        this.$emit('ctrl', !this.ctrlLatched)
      } else {
        this.$emit('key', key.keysym)
      }
    }
  }
</script>
