import type { operations } from '@/api/types';

// The two account-relation boards — GET /v1/accounts/creators and
// GET /v1/accounts/sponsors — are typed ONCE here, derived from the
// generated OpenAPI contract (src/api/types.ts, `make web-generate-api`),
// and read by every panel that fetches either board: the league tables
// under /insights/{creators,sponsors} and the per-account standing panel.
//
// They are derived rather than restated because a hand-written copy
// matches the wire today and is free to drift from it the day the spec
// moves. Three panels each carried their own copy of these shapes until
// board #515; the copies agreed by luck, and nothing would have gone red
// when one of them stopped. A field renamed in the spec now fails
// `tsc` in every consumer at once.

export type CreatorsResp = NonNullable<
  operations['getAccountCreators']['responses'][200]['content']['application/json']['data']
>;
export type SponsorsResp = NonNullable<
  operations['getAccountSponsors']['responses'][200]['content']['application/json']['data']
>;
export type CreatorRow = CreatorsResp['creators'][number];
export type SponsorRow = SponsorsResp['sponsors'][number];
