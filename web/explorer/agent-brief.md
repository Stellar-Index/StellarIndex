# CLAUDE.md — web/explorer (design & frontend brief)

Auto-loaded when working under `web/explorer`. If you're doing a **design
refresh or restructure**, read this first, then the two references at the
bottom. The goal of a redesign here is **on-system, mergeable output — reuse the
system, don't reinvent it.**

## What this is

The public Stellar Index explorer + showcase (`stellarindex.io`). **Next 16**
(app router, `output: 'export'` static) · **React 19** · **Tailwind 4** ·
TanStack Query. Customer account at `/account/*`. ~70 routes.

## Hard rules (these are what keep a redesign mergeable)

1. **Reuse the design system.** Build from `@/components/ui` (Button, Card,
   Badge, Stat, Table, Tabs, Page/Container/Section, Input, Feedback, Mono) and
   the domain atoms in `@/components/primitives` (Sparkline, DirectionPill,
   RankBadge, …). If a pattern recurs, promote it to a primitive — don't inline
   a bespoke one. **This is a hand-rolled system — NOT shadcn/ui.**
2. **Tokens, not hex.** Colors / type scale / radius / shadow are semantic
   tokens in the `@theme` block of `src/app/globals.css` (Tailwind 4 — there is
   **no `tailwind.config.ts`**). Restyle by editing tokens. Compose class
   strings with `cn()` (`@/lib/cn`, last-wins merge) — never concatenate.
3. **Dark only.** The semantic tokens are defined dark (neutral near-black
   canvas `#0a0b0d`); there is one mode — don't add `dark:` variants or a
   light/dark toggle. Depth comes from surface layering + hairline borders, not
   shadows. Type: **Fraunces serif** for display/titles (`h1`, `.font-display`),
   **Inter** body, **JetBrains Mono** for all data/numerics/eyebrows. Charts
   theme from tokens via `components/charts/chartTheme.ts`. See
   `docs/frontend/dark-redesign-direction.md`.
4. **Never hand-edit `src/api/types.ts`.** It's generated from the OpenAPI spec
   (`pnpm generate:api`) and CI drift-gated. Data flows through `api/client.ts`
   (`API_BASE_URL` + `apiGet` — import it, don't re-derive the base), build-time
   `lib/buildFetch.ts` (fail-hard), and `api/hooks.ts` (TanStack Query). Restyle
   freely; don't rewire data plumbing.
5. **Decompose-first for monoliths.** Some routes still inline their sections in
   one large `page.tsx`/`*View.tsx` — extract them to co-located `*.tsx` files
   *before* reordering. `app/assets/[slug]/AssetFAQ.tsx` is the worked pattern
   (component + data-builder + a render test). Watch for shared callers (a
   helper may also feed a JSON-LD schema, etc.) — **export, don't just move**;
   `pnpm typecheck` catches that coupling instantly.

## Gate every change

`pnpm test && pnpm typecheck && pnpm lint` — all must be green. (A full
`pnpm build` hits the live API via the fail-hard build fetch, so prefer the
three above locally.) Add a render test next to any new or extracted section —
assert **semantics** (text/role/element), not Tailwind classes, so a restyle
doesn't fight the test. See the `*.test.tsx` beside the primitives and
`AssetFAQ`.

## References

- **Design language →** `docs/architecture/design-system.md` — tokens,
  principles, primitive list, do / don't.
- **Structure & redesign playbook →** `docs/frontend/redesign-readiness.md` —
  per-page decomposition status, ranked extract-first list, and how to
  restyle / reorder / restructure safely.
- **Living rendered reference →** the `/dev/styleguide` and `/dev/primitives`
  routes (every token + component, on screen).
