/// <reference types="vitest" />
import { existsSync, readdirSync, rmSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { defineConfig, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'

// dist/ emptied before a build but for its .gitkeep (the Go embed needs the directory): an earlier build's bundles are
// never embedded with this one's
const dist = fileURLToPath(new URL('./dist', import.meta.url))
const cleanDist: Plugin = {
  name: 'clean-dist',
  apply: 'build',
  buildStart() {
    if (!existsSync(dist)) return
    for (const entry of readdirSync(dist)) {
      if (entry !== '.gitkeep') rmSync(`${dist}/${entry}`, { recursive: true, force: true })
    }
  },
}

// The console (spec 003), served by the operator at its root: no inline script (the module-preload polyfill is one)
// and no data: fonts, for the CSP. The build lands where the Go package embeds it.
export default defineConfig({
  plugins: [react(), cleanDist],
  base: '/',
  build: {
    outDir: 'dist',
    emptyOutDir: false, // cleanDist does it, keeping dist/.gitkeep
    assetsInlineLimit: 0,
    modulePreload: { polyfill: false },
    rollupOptions: {
      output: {
        // DuckDB-wasm loads an extension only from a file named <name>.duckdb_extension.wasm (spec 007): its hash goes
        // into the directory, so the file keeps its name and assets/ stays content-named
        assetFileNames: (asset) =>
          (asset.names ?? []).some((n) => n.endsWith('.duckdb_extension.wasm'))
            ? 'assets/[hash]/[name][extname]'
            : 'assets/[name]-[hash][extname]',
      },
    },
  },
  server: {
    // npm run dev against an operator's console (CONSOLE_URL, default https://localhost:8444)
    proxy: { '/api': { target: process.env.CONSOLE_URL ?? 'https://localhost:8444', secure: false } },
  },
  test: { environment: 'jsdom', include: ['src/**/*.test.{ts,tsx}'] }, // e2e/ is Playwright's
})
