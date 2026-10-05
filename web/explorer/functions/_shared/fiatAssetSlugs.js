// Fiat slugs whose one detail page is /external/assets/{slug}. Mirrors the
// /assets/{slug}/ rules in public/_redirects (which cover only the
// trailing-slash form); functions/assets-fiat-redirect.test.js keeps the
// two and src/lib/fiat-slugs.ts in lock-step.
export const FIAT_ASSET_SLUGS = new Set([
  'us-dollar',
  'euro',
  'british-pound',
  'japanese-yen',
  'swiss-franc',
  'canadian-dollar',
  'australian-dollar',
  'new-zealand-dollar',
  'chinese-yuan',
  'indian-rupee',
  'brazilian-real',
  'mexican-peso',
  'south-african-rand',
  'singapore-dollar',
  'hong-kong-dollar',
  'swedish-krona',
  'norwegian-krone',
  'danish-krone',
  'south-korean-won',
  'turkish-lira',
]);
