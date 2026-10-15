<template>
  <div class="clipboard" :class="{ touch }" v-if="opened" @click="$event.stopPropagation()">
    <p v-if="touch && hint" class="hint">{{ hint }}</p>
    <textarea ref="textarea" v-model="clipboard" @focus="$event.target.select()" />
    <div v-if="touch" class="actions">
      <button @click.stop.prevent="send">Paste into page</button>
      <button @click.stop.prevent="close">Close</button>
    </div>
  </div>
</template>

<style lang="scss" scoped>
  .clipboard {
    background-color: $background-primary;
    border-radius: 0.25rem;
    display: block;
    padding: 5px;

    position: absolute;
    bottom: 10px;
    right: 10px;

    &,
    textarea {
      max-width: 320px;
      width: 100%;
      max-height: 120px;
      height: 100%;
    }

    textarea {
      border: 0;
      color: $text-normal;
      background: none;

      &::selection {
        background: $text-normal;
      }
    }

    // on touch devices it sits at the top, clear of the soft keyboard and the controls
    &.touch {
      top: calc(56px + env(safe-area-inset-top));
      bottom: auto;
      left: 50%;
      right: auto;
      z-index: 4;
      width: calc(100% - 24px);
      height: auto;
      max-height: none;
      transform: translateX(-50%);
      box-sizing: border-box;

      textarea {
        display: block;
        height: 96px;
        font-size: 16px;
        user-select: text;
        -webkit-user-select: text;
      }

      .hint {
        margin: 0 0 4px;
        color: $text-normal;
        font-size: 13px;
      }

      .actions {
        display: flex;
        justify-content: flex-end;
        gap: 8px;
        margin-top: 4px;

        button {
          min-height: 36px;
          padding: 0 12px;
          border: 0;
          border-radius: 18px;
          background: rgba($color: #fff, $alpha: 0.15);
          color: $text-normal;
          font-weight: 600;
        }
      }
    }
  }
</style>

<script lang="ts">
  import { Component, Prop, Ref, Vue } from 'vue-property-decorator'

  @Component({
    name: 'neko-clipboard',
  })
  export default class extends Vue {
    @Ref('textarea') readonly _textarea!: HTMLTextAreaElement
    // the fallback when a touch device's clipboard cannot be read or written
    @Prop(Boolean) readonly touch!: boolean

    private opened: boolean = false
    private hint = ''
    private typing?: number

    get clipboard() {
      return this.$accessor.remote.clipboard
    }

    set clipboard(data: string) {
      this.$accessor.remote.setClipboard(data)

      if (this.typing) {
        clearTimeout(this.typing)
        this.typing = undefined
      }

      this.typing = window.setTimeout(() => this.$accessor.remote.sendClipboard(this.clipboard), 500)
    }

    open(hint = '') {
      this.opened = true
      this.hint = hint
      document.body.addEventListener('click', this.close)
      // focusing it would raise the soft keyboard over it
      if (!this.touch) window.setTimeout(() => this._textarea.focus(), 0)
    }

    // the text goes to the remote clipboard as typed; this pastes it into the page
    send() {
      if (this.typing) {
        clearTimeout(this.typing)
        this.typing = undefined
      }
      this.$emit('paste', this.clipboard)
      this.close()
    }

    close() {
      this.opened = false
      document.body.removeEventListener('click', this.close)
    }
  }
</script>
