<template>
  <div class="connect">
    <div class="window">
      <div class="loader" v-if="connecting">
        <img src="../assets/images/logo.svg" alt="loading" aria-hidden="true" class="kernel-logo" />
        <div class="loading-bar">
          <div class="loading-bar-fill"></div>
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

      .loader {
        position: relative;
        margin: 0 auto;
        display: flex;
        flex-direction: column;
        justify-content: center;
        align-items: center;
        gap: 20px;

        .kernel-logo {
          width: 90px;
          height: 90px;
        }

        .loading-bar {
          width: 128px;
          height: 1px;
          background: rgba(255, 255, 255, 0.12);
          overflow: hidden;
        }

        .loading-bar-fill {
          width: 40%;
          height: 100%;
          background: rgba(255, 255, 255, 0.85);
          animation: kernel-bar-slide 1.2s ease-in-out infinite;
        }
      }
    }
  }

  @keyframes kernel-bar-slide {
    0% {
      transform: translateX(-100%);
    }
    100% {
      transform: translateX(350%);
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
