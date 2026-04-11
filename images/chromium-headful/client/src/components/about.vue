<template>
  <div class="about" @click="toggle">
    <div class="window">
      <div class="loading" v-if="loading">
        <div class="loader">
          <div class="ocm-wordmark" aria-hidden="true">
            <span class="ocm-wordmark__open">OpenClaw</span><span class="ocm-wordmark__machines">Machines</span>
          </div>
        </div>
      </div>

      <div class="markdown-body" v-if="!loading" v-html="about"></div>
    </div>
  </div>
</template>

<style lang="scss" scoped>
  .about {
    position: absolute;
    top: 0;
    left: 0;
    right: 0;
    bottom: 0;
    background: rgba($color: $background-floating, $alpha: 0.8);

    display: flex;
    justify-content: center;
    align-items: center;

    .window {
      &::-webkit-scrollbar {
        width: 8px;
      }

      &::-webkit-scrollbar-track {
        background-color: transparent;
      }

      &::-webkit-scrollbar-thumb {
        background-color: $background-tertiary;
        border: 2px solid $background-primary;
        border-radius: 4px;
      }

      &::-webkit-scrollbar-thumb:hover {
        background-color: $background-floating;
      }

      .loading {
        display: flex;
        flex-direction: column;
        justify-content: center;
        align-items: center;

        .logo {
          display: flex;
          flex-direction: row;
          justify-content: center;
          align-items: center;
          margin: 40px 80px 0 80px;

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

        // OCM wordmark — see connect.vue for design notes.
        .loader {
          width: auto;
          height: auto;
          position: relative;
          margin: 0 auto 20px auto;
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
            animation:
              ocm-wordmark-popin 2.5s cubic-bezier(0.34, 1.56, 0.64, 1) both,
              ocm-wordmark-breathe 5s ease-in-out 2.5s infinite;
          }

          .ocm-wordmark__open {
            color: #ef4444;
          }

          .ocm-wordmark__machines {
            color: #2dd4bf;
          }
        }
      }

      .markdown-body {
        margin: 50px 200px;
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

  @Component({ name: 'neko-about' })
  export default class extends Vue {
    loading = false

    get about() {
      return this.$accessor.client.about_page
    }

    async Load() {
      this.loading = true

      try {
        const res = await this.$http.get<string>('https://raw.githubusercontent.com/m1k1o/neko/master/README.md')
        const res2 = await this.$http.post('https://api.github.com/markdown', {
          text: res.data,
          mode: 'gfm',
          context: 'github/gollum',
        })
        this.$accessor.client.setAbout(res2.data)
      } catch (err: any) {
        console.error(err)
      } finally {
        this.loading = false
      }
    }

    mounted() {
      if (this.about === '') {
        this.Load()
      }
    }

    toggle(event: { target?: HTMLElement }) {
      if (event.target && event.target.classList.contains('about')) {
        this.$accessor.client.toggleAbout()
      }
    }
  }
</script>
