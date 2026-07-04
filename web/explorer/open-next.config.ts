// ADR-0044 Stage 1 spike — @opennextjs/cloudflare adapter config.
//
// Deliberately minimal: no R2/KV, no tag cache, no revalidation queue.
// ISR/revalidate semantics are a Stage 2 decision.
//
// incrementalCache IS required even for the spike: the build puts every
// prerendered page (all 3,832 SSG routes — including the fs-reading
// blog/research/status pages that cannot re-render inside workerd) into
// .open-next/cache, NOT into the static assets. The static-assets-backed
// cache is read-only and self-contained: `populateCache` copies the
// prerendered entries into the assets upload and the worker reads them
// from the ASSETS binding. No external service, works under wrangler dev.
import { defineCloudflareConfig } from '@opennextjs/cloudflare';
import staticAssetsIncrementalCache from '@opennextjs/cloudflare/overrides/incremental-cache/static-assets-incremental-cache';

export default defineCloudflareConfig({
  incrementalCache: staticAssetsIncrementalCache,
});
