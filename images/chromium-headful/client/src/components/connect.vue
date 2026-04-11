<template>
  <div class="connect">
    <div class="window">
      <div class="loader" v-if="connecting">
        <div class="ocm-wordmark" aria-hidden="true">
          <span class="ocm-wordmark__open">OpenClaw</span><span class="ocm-wordmark__machines">Machines</span>
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

      // OCM wordmark loading indicator. Pops in small-to-big with an
      // overshoot (same cubic-bezier + duration as the prior orb) and then
      // gently breathes. Two-tone brand colors match the OCM product
      // marks (red "OpenClaw" + teal "Machines"), identical to Layout.tsx
      // in the frontend.
      .loader {
        width: auto;
        height: auto;
        position: relative;
        margin: 0 auto;
        display: flex;
        justify-content: center;
        align-items: center;

        .ocm-wordmark {
          display: inline-flex;
          align-items: baseline;
          gap: 0.3em;
          font-family: 'Whitney', 'Segoe UI', system-ui, sans-serif;
          font-size: 44px;
          font-weight: 900;
          letter-spacing: -0.01em;
          white-space: nowrap;
          line-height: 1;
          // Pop-in on mount (same style as the orb), then continuous breathe.
          animation:
            ocm-wordmark-popin 0.9s cubic-bezier(0.34, 1.56, 0.64, 1) both,
            ocm-wordmark-breathe 3.6s ease-in-out 0.9s infinite;
        }

        .ocm-wordmark__open {
          color: #ef4444; // red-500 — matches Layout.tsx brand red
        }

        .ocm-wordmark__machines {
          color: #2dd4bf; // teal-400 — matches Layout.tsx brand teal
        }
      }
    }
  }

  @keyframes ocm-wordmark-popin {
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

  @keyframes ocm-wordmark-breathe {
    0%, 100% {
      transform: scale(0.98);
      opacity: 0.85;
    }
    50% {
      transform: scale(1.02);
      opacity: 1;
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
