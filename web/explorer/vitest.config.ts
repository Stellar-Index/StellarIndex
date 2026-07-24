import { fileURLToPath } from 'node:url';
import { defineConfig } from 'vitest/config';

const abs = (p: string) => fileURLToPath(new URL(p, import.meta.url));

// Component/unit test config for the explorer. Deliberately does NOT pull in
// @vitejs/plugin-react (its peer range wants a newer Vite than vitest bundles);
// esbuild's automatic JSX runtime is all the React 19 components need to render.
// next/link + next/navigation are aliased to hermetic stubs so primitives that
// use <Link> render without the Next app-router runtime.
export default defineConfig({
  resolve: {
    alias: [
      { find: /^@\/(.*)$/, replacement: `${abs('./src')}/$1` },
      { find: /^next\/link$/, replacement: abs('./test/stubs/next-link.tsx') },
      { find: /^next\/navigation$/, replacement: abs('./test/stubs/next-navigation.ts') },
    ],
  },
  esbuild: {
    jsx: 'automatic',
    jsxImportSource: 'react',
  },
  test: {
    globals: true,
    environment: 'jsdom',
    setupFiles: ['./vitest.setup.ts'],
    // `functions/**` (Cloudflare Pages edge functions) is plain JS with its
    // own small test suite colocated the same way `src/**` component tests
    // are — no jsdom DOM APIs needed, but the shared config/setup applies.
    include: ['src/**/*.{test,spec}.{ts,tsx}', 'functions/**/*.{test,spec}.js'],
    css: false,
    restoreMocks: true,
  },
});
