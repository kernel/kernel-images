<template>
  <div class="connect">
    <div class="window">
      <div class="loader" v-if="connecting">
        <div class="ocm-orb" aria-hidden="true">
          <div class="ocm-orb__primary"></div>
          <div class="ocm-orb__bloom"></div>
          <svg class="ocm-orb__grain" xmlns="http://www.w3.org/2000/svg">
            <filter id="ocm-orb-noise-connect">
              <feTurbulence type="fractalNoise" baseFrequency="0.65" numOctaves="3" stitchTiles="stitch" />
            </filter>
            <rect width="100%" height="100%" filter="url(#ocm-orb-noise-connect)" />
          </svg>
        </div>
      </div>
    </div>
  </div>
</template>

<style lang="scss" scoped>
  .connect {
    position: fixed;
    top: 0;
    left: 0;
    right: 0;
    bottom: 0;
    background: rgba($color: $background-floating, $alpha: 0.8);

    display: flex;
    justify-content: center;
    align-items: center;

    .window {
      .logo {
        width: 100%;
        display: flex;
        flex-direction: row;
        justify-content: center;
        align-items: center;
        cursor: pointer;

        img {
          height: 90px;
          margin-right: 10px;
        }

        span {
          font-size: 30px;
          line-height: 56px;

          b {
            font-weight: 900;
          }
        }
      }

      // OCM orange orb — ported from frontend/src/components/hero/OrbBackground.tsx.
      // The original is React-driven for a hero-sized organic drift; here we
      // collapse it into a pure-CSS pop-in + breathe loop for the 140px loading
      // slot. Two blurred radial-gradient divs compose the orb (primary + bloom)
      // with an SVG feTurbulence grain overlay.
      .loader {
        width: 140px;
        height: 140px;
        position: relative;
        margin: 0 auto;
        display: flex;
        justify-content: center;
        align-items: center;
        overflow: hidden; // clip the blur halo to the loading slot

        .ocm-orb {
          position: relative;
          width: 100%;
          height: 100%;
          animation: ocm-orb-popin 0.9s cubic-bezier(0.34, 1.56, 0.64, 1) both;
        }

        .ocm-orb__primary,
        .ocm-orb__bloom {
          position: absolute;
          top: 50%;
          left: 50%;
          border-radius: 50%;
          will-change: transform, opacity;
        }

        .ocm-orb__primary {
          width: 78%;
          height: 78%;
          filter: blur(16px);
          background: radial-gradient(circle, #fb923c 0%, #f97316 40%, transparent 70%);
          animation: ocm-orb-breathe-primary 3.6s ease-in-out infinite;
        }

        .ocm-orb__bloom {
          width: 56%;
          height: 56%;
          filter: blur(12px);
          background: radial-gradient(circle, #fbbf24 0%, #f59e0b 50%, transparent 70%);
          animation: ocm-orb-breathe-bloom 3.6s ease-in-out infinite;
        }

        .ocm-orb__grain {
          position: absolute;
          inset: 0;
          width: 100%;
          height: 100%;
          opacity: 0.06;
          pointer-events: none;
        }
      }
    }
  }

  @keyframes ocm-orb-popin {
    0% {
      transform: scale(0.05);
      opacity: 0;
    }
    70% {
      transform: scale(1.06);
      opacity: 1;
    }
    100% {
      transform: scale(1);
      opacity: 1;
    }
  }

  @keyframes ocm-orb-breathe-primary {
    0%, 100% {
      transform: translate(-50%, -50%) scale(0.92);
      opacity: 0.75;
    }
    50% {
      transform: translate(-50%, -50%) scale(1.08);
      opacity: 1;
    }
  }

  @keyframes ocm-orb-breathe-bloom {
    0%, 100% {
      transform: translate(-50%, -50%) scale(1.05);
      opacity: 0.5;
    }
    50% {
      transform: translate(-50%, -50%) scale(0.9);
      opacity: 0.7;
    }
  }
</style>

<script lang="ts">
  import { Component, Vue } from 'vue-property-decorator'

  @Component({ name: 'neko-connect' })
  export default class extends Vue {
    private autoPassword: string | null = new URL(location.href).searchParams.get('pwd')

    private displayname: string = ''
    private password: string = ''

    mounted() {
      // auto-password fill
      let password = this.$accessor.password
      if (this.autoPassword !== null) {
        this.removeUrlParam('pwd')
        password = this.autoPassword
      }

      // auto-user fill
      let displayname = this.$accessor.displayname
      const usr = new URL(location.href).searchParams.get('usr')
      if (usr) {
        this.removeUrlParam('usr')
        displayname = this.$accessor.displayname || usr
      }

      // KERNEL: auto-login
      this.$accessor.login({ displayname: 'kernel', password: 'admin' })
      this.autoPassword = null
    }

    get connecting() {
      return this.$accessor.connecting
    }

    removeUrlParam(param: string) {
      let url = document.location.href
      let urlparts = url.split('?')

      if (urlparts.length >= 2) {
        let urlBase = urlparts.shift()
        let queryString = urlparts.join('?')

        let prefix = encodeURIComponent(param) + '='
        let pars = queryString.split(/[&;]/g)
        for (let i = pars.length; i-- > 0; ) {
          if (pars[i].lastIndexOf(prefix, 0) !== -1) {
            pars.splice(i, 1)
          }
        }

        url = urlBase + (pars.length > 0 ? '?' + pars.join('&') : '')
        window.history.pushState('', document.title, url)
      }
    }

    login() {
      let password = this.password
      if (this.autoPassword !== null) {
        password = this.autoPassword
      }

      if (this.displayname == '') {
        this.$swal({
          title: this.$t('connect.error') as string,
          text: this.$t('connect.empty_displayname') as string,
          icon: 'error',
        })
        return
      }

      this.$accessor.login({ displayname: this.displayname, password })
      this.autoPassword = null
    }

    about() {
      this.$accessor.client.toggleAbout()
    }
  }
</script>
