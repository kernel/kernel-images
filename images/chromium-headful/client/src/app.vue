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
            :showDomOverlay="showDomOverlay"
            :domSyncTypes="domSyncTypes"
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
      overflow-y: auto !important;
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
  import { DomSyncPayload, DomWebSocketMessage, DomElementType, DOM_ELEMENT_TYPES } from '~/neko/dom-types'

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
    private domWebSocket: WebSocket | null = null
    private domReconnectTimeout: number | null = null

    // dom_sync: enables WebSocket connection for DOM element syncing
    // Values: false (default), true (inputs only), or comma-separated types (e.g., "inputs,buttons,links")
    get isDomSyncEnabled(): boolean {
      const params = new URL(location.href).searchParams
      const param = params.get('dom_sync') || params.get('domSync')
      if (!param || param === 'false' || param === '0') return false
      return true
    }

    // Get enabled DOM element types from query param
    // ?dom_sync=true -> ['inputs'] (default, backwards compatible)
    // ?dom_sync=inputs,buttons,links -> ['inputs', 'buttons', 'links']
    get domSyncTypes(): DomElementType[] {
      const params = new URL(location.href).searchParams
      const param = params.get('dom_sync') || params.get('domSync')
      if (!param || param === 'false' || param === '0') return []
      // If true/1, default to inputs only (backwards compatible)
      if (param === 'true' || param === '1') return ['inputs']
      // Parse comma-separated list and filter to valid types
      const types = param.split(',').map((t) => t.trim().toLowerCase()) as DomElementType[]
      return types.filter((t) => DOM_ELEMENT_TYPES.includes(t))
    }

    // dom_overlay: shows purple overlay rectangles when dom_sync is enabled (default: true)
    get showDomOverlay() {
      if (!this.isDomSyncEnabled) return false
      const params = new URL(location.href).searchParams
      const param = params.get('dom_overlay') || params.get('domOverlay')
      // Default to true if not specified, false only if explicitly set to 'false' or '0'
      if (param === null) return true
      return param !== 'false' && param !== '0'
    }

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
        // Connect to DOM sync if enabled
        if (this.isDomSyncEnabled) {
          this.connectDomSync()
        }
        try {
          if (window.parent !== window) {
            window.parent.postMessage({ type: 'KERNEL_CONNECTED', connected: true }, this.parentOrigin)
          }
        } catch (e) {
          console.error('Failed to post message to parent', e)
        }
      } else {
        // Disconnect DOM sync when main connection is lost
        this.disconnectDomSync()
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

      // Handle readOnly query param (e.g., ?readOnly=true or ?readonly=1)
      const readOnlyParam = params.get('readOnly') || params.get('readonly') || params.get('ro')
      const readOnly = typeof readOnlyParam === 'string' && ['1', 'true', 'yes'].includes(readOnlyParam.toLowerCase())
      if (readOnly) {
        // Disable implicit hosting so the user doesn't automatically gain control
        this.$accessor.remote.setImplicitHosting(false)
        // Lock the session locally to block any input even if hosting is later requested
        this.$accessor.remote.setLocked(true)
      }
    }

    // KERNEL: end custom resolution, frame rate, and readOnly control via query params

    // KERNEL: DOM Sync - connects to kernel-images API WebSocket for bounding box overlay
    private domRetryCount = 0
    private readonly domMaxRetries = 10

    private connectDomSync() {
      if (!this.isDomSyncEnabled) return
      if (this.domWebSocket && this.domWebSocket.readyState === WebSocket.OPEN) return

      const params = new URL(location.href).searchParams
      const domPort = params.get('dom_port') || '444'
      const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
      const domUrl = `${protocol}//${location.hostname}:${domPort}/dom-sync`

      console.log(`[dom-sync] Connecting to ${domUrl} (attempt ${this.domRetryCount + 1})`)

      try {
        this.domWebSocket = new WebSocket(domUrl)

        this.domWebSocket.onopen = () => {
          console.log('[dom-sync] Connected')
          this.domRetryCount = 0 // Reset retry count on success
          this.$accessor.dom.setEnabled(true)
          this.$accessor.dom.setConnected(true)
        }

        this.domWebSocket.onmessage = (event) => {
          try {
            const message: DomWebSocketMessage = JSON.parse(event.data)
            if (message.event === 'dom/sync' && message.data) {
              this.$accessor.dom.applySync(message.data)
            }
          } catch (e) {
            console.error('[dom-sync] Failed to parse message:', e)
          }
        }

        this.domWebSocket.onclose = () => {
          console.log('[dom-sync] Disconnected')
          this.$accessor.dom.setConnected(false)
          this.scheduleDomReconnect()
        }

        this.domWebSocket.onerror = (error) => {
          console.error('[dom-sync] WebSocket error:', error)
          // Error will trigger onclose, which handles reconnect
        }
      } catch (e) {
        console.error('[dom-sync] Failed to connect:', e)
        this.scheduleDomReconnect()
      }
    }

    private scheduleDomReconnect() {
      if (!this.isDomSyncEnabled || !this.connected) return
      if (this.domRetryCount >= this.domMaxRetries) {
        console.log('[dom-sync] Max retries reached, giving up')
        return
      }
      // Exponential backoff: 500ms, 1s, 2s, 4s... capped at 5s
      const delay = Math.min(500 * Math.pow(2, this.domRetryCount), 5000)
      this.domRetryCount++
      console.log(`[dom-sync] Reconnecting in ${delay}ms`)
      this.domReconnectTimeout = window.setTimeout(() => {
        this.connectDomSync()
      }, delay)
    }

    private disconnectDomSync() {
      if (this.domReconnectTimeout) {
        clearTimeout(this.domReconnectTimeout)
        this.domReconnectTimeout = null
      }
      if (this.domWebSocket) {
        this.domWebSocket.close()
        this.domWebSocket = null
      }
      this.domRetryCount = 0
      this.$accessor.dom.setEnabled(false)
      this.$accessor.dom.setConnected(false)
      this.$accessor.dom.reset()
    }

    beforeDestroy() {
      this.disconnectDomSync()
    }

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
