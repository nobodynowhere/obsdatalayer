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
    // Improve chunk size warnings (moved to build level for newer Vite/Rolldown)
    chunkSizeWarningLimit: 700,
    // Optimize chunk splitting to reduce main bundle size
    rollupOptions: {
      output: {
        manualChunks: (id) => {
          // Split vendor libraries into separate chunks
          if (id.includes('node_modules')) {
            // Vue core
            if (id.includes('vue/dist/vue') || id.includes('@vue/runtime-core')) {
              return 'vue-core'
            }
            // Vue router
            if (id.includes('vue-router')) {
              return 'vue-router'
            }
            // Pinia
            if (id.includes('pinia')) {
              return 'pinia'
            }
            // PrimeVue
            if (id.includes('primevue') || id.includes('primeicons')) {
              return 'primevue-vendor'
            }
            // DDS components
            if (id.includes('@dds/components')) {
              return 'dds-vendor'
            }
            // Other node_modules
            return 'vendor'
          }
        },
      },
    },
    // Enable CSS code splitting
    cssCodeSplit: true,
    // Improve build performance
    target: 'es2020',
  },
  server: {
    port: 5173,
    proxy: {
      // Admin API endpoints during `npm run dev`. The gateway's admin listener
      // is on loopback:9091 by default.
      '^/(api|healthz|metrics)': {
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
