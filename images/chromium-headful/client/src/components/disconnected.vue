<template>
  <div class="disconnected-overlay">
    <div class="message-container">
      <span class="message">{{ sessionGone ? $t('disconnected.gone') : $t('disconnected.lost') }}</span>
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
      justify-content: center;
      align-items: center;
      padding: 20px;

      .message {
        font-size: 18px;
        color: $text-muted;
        text-align: center;
      }
    }
  }
</style>

<script lang="ts">
  import { Component, Vue } from 'vue-property-decorator'

  @Component({ name: 'neko-disconnected' })
  export default class extends Vue {
    // Only a session-gone close means the browser is gone. Anything else may be
    // a network change with the browser still running.
    get sessionGone() {
      return this.$accessor.sessionGone
    }
  }
</script>
