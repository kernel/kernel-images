<template>
  <div id="neko" :class="[!videoOnly && side ? 'expanded' : '']">
    <template v-if="!$client.supported">
      <neko-unsupported />
    </template>
    <template v-else>
      <main class="neko-main">
        <div v-if="!videoOnly" class="header-container">
          <neko-header />
        </div>
        <div class="video-container">
          <neko-video
            ref="video"
            :hideControls="hideControls"
            :extraControls="isEmbedMode"
            :readOnly="isReadOnlyMode"
            @control-attempt="controlAttempt"
          />
        </div>
        <div v-if="!videoOnly" class="room-container">
          <neko-members />
          <div class="room-menu">
            <div class="settings">
              <neko-menu />
            </div>
            <div class="controls">
              <neko-controls :shakeKbd="shakeKbd" />
            </div>
            <div class="emotes">
              <neko-emotes />
            </div>
          </div>
        </div>
      </main>
      <neko-side v-if="!videoOnly && side" />
      <neko-connect v-if="!connected && !wasConnected" />
      <neko-disconnected v-if="!connected && wasConnected" />
      <neko-about v-if="about" />
      <notifications
        v-if="!videoOnly"
        group="neko"
        position="top left"
        style="top: 50px; pointer-events: none"
        :ignoreDuplicates="true"
      />
    </template>
  </div>
</template>

<style lang="scss">
  #neko {
    position: absolute;
    top: 0;
    left: 0;
    right: 0;
    bottom: 0;
    max-width: 100vw;
    max-height: 100vh;
    flex-direction: row;
    display: flex;

    .neko-main {
      min-width: 360px;
      max-width: 100%;
      flex-grow: 1;
      flex-direction: column;
      display: flex;
      overflow: auto;

      .header-container {
        background: $background-tertiary;
        height: $menu-height;
        flex-shrink: 0;
        // KERNEL: hide it
        // display: flex;
        display: none;
      }

      .video-container {
        background: rgba($color: #000, $alpha: 0.4);
        max-width: 100%;
        flex-grow: 1;
        display: flex;
      }

      .room-container {
        background: $background-tertiary;
        height: $controls-height;
        max-width: 100%;
        flex-shrink: 0;
        flex-direction: column;
        // KERNEL: hide it
        // display: flex;
        display: none;

        .room-menu {
          max-width: 100%;
          flex: 1;
          display: flex;

          .settings {
            margin-left: 10px;
            flex: 1;
            justify-content: flex-start;
            align-items: center;
            display: flex;
          }

          .controls {
            flex: 1;
            justify-content: center;
            align-items: center;
            display: flex;
          }

          .emotes {
            margin-right: 10px;
            flex: 1;
            justify-content: flex-end;
            align-items: center;
            display: flex;
          }
        }
      }
    }
  }

  @media only screen and (max-width: 1024px) {
    html,
    body {
      overflow: hidden !important;
      width: auto !important;
      height: auto !important;
    }

    body > p {
      display: none;
    }

    #neko {
      position: relative;
      flex-direction: column;
      max-height: initial !important;

      .neko-main {
        height: 100vh;
      }

      .neko-menu {
        height: 100vh;
        width: 100% !important;
      }
    }
  }

  @media only screen and (max-width: 1024px) and (orientation: portrait) {
    #neko {
      &.expanded .neko-main {
        height: 40vh;
      }

      &.expanded .neko-menu {
        height: 60vh;
        width: 100% !important;
      }
    }
  }

  @media only screen and (max-width: 768px) {
    #neko .neko-main .room-container {
      display: none;
    }
  }
</style>

<script lang="ts">
  import { Vue, Component, Ref, Watch } from 'vue-property-decorator'

  import Connect from '~/components/connect.vue'
  import Disconnected from '~/components/disconnected.vue'
  import Video from '~/components/video.vue'
  import Menu from '~/components/menu.vue'
  import Side from '~/components/side.vue'
  import Controls from '~/components/controls.vue'
  import Members from '~/components/members.vue'
  import Emotes from '~/components/emotes.vue'
  import About from '~/components/about.vue'
  import Header from '~/components/header.vue'
  import Unsupported from '~/components/unsupported.vue'

  @Component({
    name: 'neko',
    components: {
      'neko-connect': Connect,
      'neko-disconnected': Disconnected,
      'neko-video': Video,
      // 'neko-menu': Menu,
      //'neko-side': Side,
      // 'neko-controls': Controls,
      //'neko-members': Members,
      //'neko-emotes': Emotes,
      //'neko-about': About,
      //'neko-header': Header,
      //'neko-unsupported': Unsupported,
    },
  })
  export default class extends Vue {
    @Ref('video') video!: Video

    shakeKbd = false
    wasConnected = false

    get volume() {
      const numberParam = parseFloat(new URL(location.href).searchParams.get('volume') || '1.0')
      return Math.max(0.0, Math.min(!isNaN(numberParam) ? numberParam * 100 : 100, 100))
    }

    get isCastMode() {
      return !!new URL(location.href).searchParams.get('cast')
    }

    get isEmbedMode() {
      return !!new URL(location.href).searchParams.get('embed')
    }

    get isReadOnlyMode() {
      const params = new URL(location.href).searchParams
      const value = params.get('readOnly') || params.get('readonly') || params.get('ro')
      return typeof value === 'string' && ['1', 'true', 'yes'].includes(value.toLowerCase())
    }

    get hideControls() {
      return this.isCastMode
    }

    get videoOnly() {
      return this.isCastMode || this.isEmbedMode
    }

    @Watch('volume', { immediate: true })
    onVolume(volume: number) {
      if (new URL(location.href).searchParams.has('volume')) {
        this.$accessor.video.setVolume(volume)
      }
    }

    get parentOrigin() {
      try {
        if (document.referrer) {
          return new URL(document.referrer).origin
        }
      } catch (e) {
        // fallback if referrer is not a valid URL
      }
      return '*'
    }

    @Watch('hideControls', { immediate: true })
    onHideControls(enabled: boolean) {
      if (enabled) {
        this.$accessor.video.setMuted(false)
        this.$accessor.settings.setSound(false)
      }
    }

    @Watch('side')
    onSide(side: boolean) {
      if (side) {
        console.log('side enabled')
        // scroll to the side
        this.$nextTick(() => {
          const side = document.querySelector('aside')
          if (side) {
            side.scrollIntoView({ behavior: 'smooth', block: 'start' })
          }
        })
      }
    }

    // KERNEL: begin custom resolution, frame rate, and readOnly control via query params

    // Add a watcher so that when we are connected we can set the resolution from query params
    @Watch('connected', { immediate: true })
    onConnected(value: boolean) {
      if (value) {
        this.wasConnected = true
        this.applyQueryResolution()
        try {
          if (window.parent !== window) {
            window.parent.postMessage({ type: 'KERNEL_CONNECTED', connected: true }, this.parentOrigin)
          }
        } catch (e) {
          console.error('Failed to post message to parent', e)
        }
      }
    }

    // Read ?width=, ?height=, and optional ?rate= (or their short aliases w/h/r) from the URL
    // and set the resolution accordingly. If the current user is an admin we also request the
    // server to switch to that resolution.
    private applyQueryResolution() {
      const params = new URL(location.href).searchParams

      // Helper to parse integer query parameters and return `undefined` when the value is not a valid number.
      const parseIntSafe = (keys: string[], fallback?: number): number | undefined => {
        for (const key of keys) {
          const value = params.get(key)
          if (value !== null) {
            const num = parseInt(value, 10)
            if (!isNaN(num)) return num
          }
        }
        return fallback
      }

      const width = parseIntSafe(['width', 'w'])
      const height = parseIntSafe(['height', 'h'])
      const rate = parseIntSafe(['rate', 'r'], 30) as number

      if (width !== undefined && height !== undefined) {
        const resolution = { width, height, rate }
        this.$accessor.video.setResolution(resolution)
        if (this.$accessor.user && this.$accessor.user.admin) {
          this.$accessor.video.screenSet(resolution)
        }
      }

      if (this.isReadOnlyMode) {
        // Disable implicit hosting so the user doesn't automatically gain control
        this.$accessor.remote.setImplicitHosting(false)
        // Lock the session locally to block any input even if hosting is later requested
        this.$accessor.remote.setLocked(true)
      }
    }

    // KERNEL: end custom resolution, frame rate, and readOnly control via query params

    controlAttempt() {
      if (this.shakeKbd || this.$accessor.remote.hosted) return

      this.shakeKbd = true
      window.setTimeout(() => (this.shakeKbd = false), 5000)
    }

    get about() {
      return this.$accessor.client.about
    }

    get side() {
      return this.$accessor.client.side
    }

    get connected() {
      return this.$accessor.connected
    }

    get playing() {
      return this.$accessor.video.playing
    }

    @Watch('playing')
    onPlaying(value: boolean) {
      try {
        if (window.parent === window) return

        if (value) {
          window.parent.postMessage({ type: 'KERNEL_PLAYING', playing: true }, this.parentOrigin)
        } else {
          window.parent.postMessage({ type: 'KERNEL_PAUSED', playing: false }, this.parentOrigin)
        }
      } catch (e) {
        console.error('Failed to post message to parent', e)
      }
    }

    @Watch('connected', { immediate: true })
    onConnectedChange(connected: boolean) {
      if (connected) {
        this.wasConnected = true
      }
    }
  }
</script>
