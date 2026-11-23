<template>
  <div :class="['disconnected-overlay', { reconnecting }]" role="status">
    <div class="message-container">
      <span class="message">{{ reconnecting ? $t('connection.reconnecting') : $t('disconnected.message') }}</span>
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

    // the last frame stays visible underneath while the session is restored
    &.reconnecting {
      position: fixed;
      top: 0;
      left: 0;
      z-index: 10;
      background: rgba($color: #000, $alpha: 0.6);

      .message {
        color: #fff;
      }
    }

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
  import { Component, Prop, Vue } from 'vue-property-decorator'

  @Component({ name: 'neko-disconnected' })
  export default class extends Vue {
    @Prop({ type: Boolean, default: false }) readonly reconnecting!: boolean
  }
</script>
