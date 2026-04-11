<template>
  <div class="about" @click="toggle">
    <div class="window">
      <div class="loading" v-if="loading">
        <div class="loader">
          <div class="ocm-orb" aria-hidden="true">
            <div class="ocm-orb__primary"></div>
            <div class="ocm-orb__bloom"></div>
            <svg class="ocm-orb__grain" xmlns="http://www.w3.org/2000/svg">
              <filter id="ocm-orb-noise-about">
                <feTurbulence type="fractalNoise" baseFrequency="0.65" numOctaves="3" stitchTiles="stitch" />
              </filter>
              <rect width="100%" height="100%" filter="url(#ocm-orb-noise-about)" />
            </svg>
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

        // OCM orange orb — see connect.vue for design notes.
        .loader {
          width: 140px;
          height: 140px;
          position: relative;
          margin: 0 auto 20px auto;
          display: flex;
          justify-content: center;
          align-items: center;
          overflow: hidden;

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

      .markdown-body {
        margin: 50px 200px;
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
