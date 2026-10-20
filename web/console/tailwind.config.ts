import type { Config } from 'tailwindcss'

// The Hugr Lab design system's tokens as CSS variables - the same names and values as tresor-server's and kista's
// consoles, so the shell and the consoles it mounts look as one.
const v = (name: string) => `var(--${name})`

export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        surface: { DEFAULT: v('surface'), soft: v('surface-soft') },
        ink: { DEFAULT: v('ink'), muted: v('ink-muted') },
        line: v('border'),
        brand: { DEFAULT: v('brand'), strong: v('brand-strong'), on: v('on-brand') },
        navy: { DEFAULT: v('navy'), line: v('navy-line'), muted: v('navy-muted') },
        focus: v('focus'),
      },
      fontFamily: {
        sans: ['Manrope', 'system-ui', 'sans-serif'],
        mono: ['"JetBrains Mono"', 'ui-monospace', 'monospace'],
      },
      borderRadius: { sm: '8px', md: '16px', lg: '28px' },
    },
  },
} satisfies Config
