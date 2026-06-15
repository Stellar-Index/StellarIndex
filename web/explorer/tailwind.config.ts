import type { Config } from 'tailwindcss';

const config: Config = {
  // Manual toggle via `class="dark"` on <html>. The inline init
  // script in layout.tsx hydrates the class from localStorage
  // before the first paint; absent that, it falls back to the
  // OS prefers-color-scheme media query so the default behaviour
  // matches what shipped before the toggle.
  darkMode: 'class',
  content: ['./src/**/*.{js,ts,jsx,tsx,mdx}'],
  theme: {
    extend: {
      // Brand palette is intentionally minimal at v1.
      // A real design pass replaces this in Phase 7.
      colors: {
        brand: {
          50: '#f0f9ff',
          100: '#e0f2fe',
          500: '#0ea5e9',
          600: '#0284c7',
          900: '#0c4a6e',
        },
        // Semantic colours for delta strips, etc.
        up: {
          subtle: '#bbf7d0',
          DEFAULT: '#16a34a',
          strong: '#15803d',
        },
        down: {
          subtle: '#fecaca',
          DEFAULT: '#dc2626',
          strong: '#b91c1c',
        },
        // Off-tone overlay for time-machine "viewing as of" mode.
        timepin: {
          DEFAULT: '#fef3c7',
          ring: '#f59e0b',
        },
        // Severity + text scales used by DegradedBanner + incident/outage
        // surfaces and body copy. These were REFERENCED across 7 components
        // (bg-bad-50, text-warn-700, text-ink, …) but never defined here, so
        // Tailwind generated no CSS for them — the degraded/outage banner's
        // severity tint silently didn't render (audit-2026-06-14 Q3). Red =
        // bad, amber = warn (standard Tailwind red/amber), ink = body text.
        bad: {
          50: '#fef2f2',
          300: '#fca5a5',
          500: '#ef4444',
          700: '#b91c1c',
          900: '#7f1d1d',
        },
        warn: {
          50: '#fffbeb',
          300: '#fcd34d',
          500: '#f59e0b',
          700: '#b45309',
          900: '#78350f',
        },
        ink: {
          DEFAULT: '#0f172a', // slate-900 — primary body text
          faint: '#64748b',   // slate-500 — secondary / muted text
        },
      },
      fontFamily: {
        sans: ['var(--font-sans)', 'system-ui', 'sans-serif'],
        mono: ['var(--font-mono)', 'ui-monospace', 'monospace'],
      },
    },
  },
  plugins: [],
};

export default config;
