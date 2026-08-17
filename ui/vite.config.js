import { fileURLToPath, URL } from 'url'
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  // Must match ui.Prefix in internal/ui/ui.go so emitted asset URLs resolve.
  base: '/ui/',
  build: {
    // Build straight into the Go package that embeds it, so `npm run build`
    // followed by `go build` produces a single self-contained binary.
    outDir: fileURLToPath(new URL('../internal/ui/dist', import.meta.url)),
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    proxy: {
      // Admin API endpoints during `npm run dev`. The gateway's admin listener
      // is on loopback:9091 by default.
      '^/(whoami|tenants|users|roles|config|healthz|metrics)': {
        target: 'http://127.0.0.1:9091',
        changeOrigin: true,
        secure: false,
      },
    },
  },
  css: {
    postcss: {
      plugins: [
        {
          postcssPlugin: 'internal:charset-removal',
          AtRule: {
            charset: (atRule) => {
              if (atRule.name === 'charset') atRule.remove()
            },
          },
        },
      ],
    },
  },
})
