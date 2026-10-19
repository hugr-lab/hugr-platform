/// <reference types="vitest" />
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The console (spec 003), served by the operator at its root: no inline script (the module-preload polyfill is one)
// and no data: fonts, for the CSP. The build lands where the Go package embeds it.
export default defineConfig({
  plugins: [react()],
  base: '/',
  build: {
    outDir: 'dist',
    emptyOutDir: false, // dist/.gitkeep stays: the Go embed needs the directory
    assetsInlineLimit: 0,
    modulePreload: { polyfill: false },
  },
  server: {
    // npm run dev against an operator's console (CONSOLE_URL, default https://localhost:8444)
    proxy: { '/api': { target: process.env.CONSOLE_URL ?? 'https://localhost:8444', secure: false } },
  },
  test: { environment: 'jsdom', include: ['src/**/*.test.{ts,tsx}'] }, // e2e/ is Playwright's
})
