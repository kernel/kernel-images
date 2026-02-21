const path = require('path')
const webpack = require('webpack')

module.exports = {
  productionSourceMap: false,
  css: {
    loaderOptions: {
      sass: {
        additionalData: `
          @import "@/assets/styles/_variables.scss";
        `,
      },
    },
  },
  publicPath: './',
  assetsDir: './',
  configureWebpack: {
    resolve: {
      alias: {
        vue$: 'vue/dist/vue.esm.js',
        '~': path.resolve(__dirname, 'src/'),
      },
    },
    plugins: [
      // Define environment variables for telemetry configuration (build-time only)
      // Runtime config via TELEMETRY_ENDPOINT and TELEMETRY_CAPTURE env vars is preferred
      //
      // Build-time example:
      //   VUE_APP_TELEMETRY_ENDPOINT=https://example.com/telemetry npm run build
      //   VUE_APP_TELEMETRY_CAPTURE='error_*,perf_fps' npm run build
      new webpack.DefinePlugin({
        'process.env.VUE_APP_TELEMETRY_ENDPOINT': JSON.stringify(process.env.VUE_APP_TELEMETRY_ENDPOINT || ''),
        'process.env.VUE_APP_TELEMETRY_CAPTURE': JSON.stringify(process.env.VUE_APP_TELEMETRY_CAPTURE || ''),
      }),
    ],
  },
  devServer: {
    allowedHosts: 'all',
  },
}
