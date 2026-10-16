import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Silencer 前端构建配置
// 不设 base：产物用绝对路径 /assets/…，由 Go 网关同源托管
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      '@': new URL('./src', import.meta.url).pathname,
    },
  },
  clearScreen: false,
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      '/api': {
      target: 'http://127.0.0.1:8080',
        changeOrigin: true,
        headers: {
          Origin: 'http://127.0.0.1:8080',
        },
      },
    },
  },
  build: {
    target: 'es2021',
    // Vite 8 (rolldown) 默认 minify，无需 esbuild
    sourcemap: false,
    outDir: 'dist',
    emptyOutDir: true,
  },
})
