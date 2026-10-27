<template>
  <div class="disconnected-overlay">
    <div class="message-container">
      <span class="message">{{ sessionGone ? $t('disconnected.gone') : $t('disconnected.lost') }}</span>
      <button v-if="!sessionGone" class="reload" @click="reload">{{ $t('disconnected.reload') }}</button>
    </div>
  </div>
</template>

<style lang="scss" scoped>
  .disconnected-overlay {
    width: 100vw;
    height: 100vh;
    background: #000;
    display: flex;
    justify-content: center;
    align-items: center;

    .message-container {
      display: flex;
      flex-direction: column;
      justify-content: center;
      align-items: center;
      gap: 16px;
      padding: 20px;

      .message {
        font-size: 18px;
        color: $text-muted;
        text-align: center;
      }

      .reload {
        min-height: 44px;
        padding: 0 20px;
        border: 0;
        border-radius: 22px;
        background: rgba($color: #fff, $alpha: 0.15);
        color: #fff;
        font-size: 15px;
        font-weight: 600;
        cursor: pointer;
        touch-action: manipulation;
      }
    }
  }
</style>

<script lang="ts">
  import { Component, Vue } from 'vue-property-decorator'

  @Component({ name: 'neko-disconnected' })
  export default class extends Vue {
    // Only a session-gone close means the browser is gone. Anything else may be
    // a network change with the browser still running, so offer a reload rather
    // than claiming it shut down.
    get sessionGone() {
      return this.$accessor.sessionGone
    }

    reload() {
      location.reload()
    }
  }
</script>
