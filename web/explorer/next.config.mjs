// ADR-0044 Stage 1 spike: OPEN_NEXT=1 switches the build from the
// static export (CF Pages, the current production path) to a normal
// server build consumed by @opennextjs/cloudflare (Workers SSR).
// The static-export path stays the default — nothing changes unless
// the env var is set. See docs/adr/0044-explorer-edge-rendering.md
// and docs/architecture/adr-0044-stage1-spike.md.
const openNext = process.env.OPEN_NEXT === '1';

/** @type {import('next').NextConfig} */
const nextConfig = {
  reactStrictMode: true,

  // React Compiler (babel-plugin-react-compiler 1.0, stable) — auto-memoizes
  // components at build time so manual useMemo/useCallback are mostly
  // unnecessary. Safe to enable now that the react-hooks/* rules (its
  // rules-of-react prerequisite) are enforced clean; the compiler bails out
  // (skips) any component it can't prove safe rather than miscompiling.
  reactCompiler: true,

  // Static-export the entire site. Deployed to Cloudflare Pages
  // (or rsync'd to r1 nginx behind Cloudflare CDN) — same vendor
  // story as api.stellarindex.io per docs/operations/cdn-setup.md.
  // Dynamic routes (/coins/[slug], /contracts/[id], etc.) use
  // client-side rendering: the build emits a shell, the page
  // hydrates and fetches data from the API based on the URL.
  // High-traffic dynamic routes can be pre-rendered at build time
  // via `generateStaticParams` (added in Phase 8).
  //
  // Under OPEN_NEXT=1 (ADR-0044 spike) we omit `output` entirely:
  // OpenNext consumes the default server build.
  ...(openNext ? {} : { output: 'export' }),

  // Static export needs explicit trailing-slash handling for
  // Cloudflare Pages routing. Trailing slash → directory-style
  // URL → directly maps to filesystem. Kept under OpenNext too so
  // canonical URLs don't change across the migration.
  trailingSlash: true,

  // No server image optimization in static-export mode. Kept off
  // under OpenNext for the spike (Workers image optimization is a
  // separate binding decision for Stage 2+).
  images: {
    unoptimized: true,
  },

  // Default 60s per page is too tight under build load — the
  // explorer pre-renders ~500 asset pages, each doing 4-6 API
  // fetches. When the API rate-limiter kicks in the cumulative
  // wait per page (each 8s timeout, optional retries) can exceed
  // 60s for slugs the build happens to hit late in the queue.
  // 180s gives headroom without papering over a real hang —
  // if a page legitimately stalls forever we still notice.
  // Tracked since 2026-05-11 deploy stuck on /assets/WGUARDIAN-...
  staticPageGenerationTimeout: 180,

  // Sourcemaps in production help when debugging from issue reports.
  productionBrowserSourceMaps: true,

  // All API access is client-side from the browser to this network's
  // api.* origin, which is already CDN-cached per cdn-setup.md. No
  // server-side fetches needed — that's the entire point of the
  // static-export architecture.
  //
  // Env contract (per-network builds, docs/operations/explorer-deployment.md):
  //   NEXT_PUBLIC_NETWORK       mainnet | testnet | futurenet (default mainnet)
  //   NEXT_PUBLIC_API_BASE_URL  OPTIONAL override of the API origin. When
  //                             unset, src/api/client.ts falls back to
  //                             CURRENT_NETWORK.apiBaseUrl, i.e. the origin
  //                             that belongs to NEXT_PUBLIC_NETWORK. CI sets
  //                             it to a stub (.invalid) so builds never
  //                             contact production.
  // NEXT_PUBLIC_* vars are inlined by Next from the build environment on
  // their own, so NEXT_PUBLIC_API_BASE_URL must NOT be listed here: an
  // `env` entry with a `?? 'https://api...'` default is inlined as a string
  // on every build that forgot the var, which makes the `??` fallback in
  // client.ts unreachable — a testnet/futurenet build then serves MAINNET
  // data under test-net chrome (the git-integrated CF Pages projects are
  // hand-configured, so "forgot the var" is a real deploy state). Pinned by
  // src/lib/next-config-env.test.ts and the mainnet-hardcode guard.
  //
  // Build identifiers (BUILD_SHA / BUILD_TIME) are surfaced in the footer
  // so an operator viewing the live site can confirm which commit it
  // reflects. CF Pages sets CF_PAGES_COMMIT_SHA automatically; for the
  // GitHub Actions manual-deploy fallback we read GITHUB_SHA. Local
  // builds get "dev".
  env: {
    NEXT_PUBLIC_BUILD_SHA:
      process.env.CF_PAGES_COMMIT_SHA ??
      process.env.GITHUB_SHA ??
      'dev',
    NEXT_PUBLIC_BUILD_TIME: new Date().toISOString(),
  },
};

export default nextConfig;
