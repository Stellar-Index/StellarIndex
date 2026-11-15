// CURATED_ASSET_ABOUT — short descriptions for the major Stellar
// assets, keyed on the asset id (`native` or `CODE-ISSUER`, the
// issuers in internal/currency/data/seed.yaml), never on the code:
// a look-alike wearing the same ticker must not be described as the
// real asset. The first sentence shows; the rest sits behind a
// <details>.
const CURATED_ASSET_ABOUT: Record<string, string> = {
  native: `Stellar Lumens (XLM) is the native asset of the Stellar network — the public blockchain that Stellar Index indexes natively. Lumens pay transaction fees, fund minimum account reserves (currently 1 XLM per base reserve unit + 0.5 per ledger entry), and serve as a convenient bridge asset for path-payments between any two issued tokens.

XLM's supply is fixed: the protocol's inflation mechanism was removed in 2019 (Protocol 12), and the SDF burned over half the original 100B that November, leaving ~50B lumens outstanding. No new lumens are created; the circulating amount reflects SDF treasury releases from that fixed pool.`,
  'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN': `USD Coin (USDC) on Stellar is issued by Circle Internet Financial — the same regulated issuer that runs the Ethereum-native USDC. Each USDC is backed 1:1 by USD-denominated reserves (cash + short-duration U.S. treasuries) held in segregated accounts and attested to monthly by Deloitte.

USDC is the dominant USD-pegged stablecoin on Stellar by volume, the canonical USD proxy for the aggregator's stablecoin-fiat policy, and the quote leg of most active on-chain DEX pairs.`,
  'EURC-GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2': `EURC is Circle's euro-denominated stablecoin. Like USDC it carries 1:1 fiat backing, monthly attestations, and Circle's regulated-issuer status. On Stellar EURC is the canonical EUR proxy and the largest non-USD stablecoin by 24h volume — most EUR cross-rates on the explorer flow through it.`,
  'AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA': `AQUA is the governance + reward token for the Aquarius DEX, a Stellar-native AMM that issues liquidity-incentive emissions to selected pools. Holders vote on emission allocations; LPs in voted-up pools earn AQUA on top of their swap fees. AQUA is one of the most-held classic credit assets on Stellar by trustline count.`,
  'yXLM-GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55': `yXLM is a yield-bearing wrapper for XLM issued by Ultra Stellar (ultrastellar.com). Holders earn variable APY funded by deploying the underlying XLM into credit markets. The peg to underlying XLM is maintained by the issuer's redemption mechanism, not by an on-chain CDP.`,
  'SHX-GDSTRSHXHGJ7ZIVRBXEYE5Q74XUVCUSEKEBR7UCHEUUEK72N7I7KJ6JH': `SHX is the native token of StellarX, an early Stellar-native trading frontend. The asset has a long history on the network and remains in the top tier by trustline count, though active trade volume is concentrated against XLM and USDC.`,
  'BLND-GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY': `BLND is the governance token for Blend Capital's Stellar lending protocol. Holders participate in protocol-level decisions; the token is not redeemable for collateral. BLND issuance is controlled by the BLND-USDC backstop pool that secures the protocol's bad-debt insurance fund.`,
};

export function AssetAbout({
  assetId,
  symbol,
}: {
  assetId: string;
  symbol: string;
}) {
  const text = Object.hasOwn(CURATED_ASSET_ABOUT, assetId)
    ? CURATED_ASSET_ABOUT[assetId]
    : undefined;
  if (!text) return null;
  return <ExpandableText title={`About ${symbol}`} body={text} />;
}

function ExpandableText({ title, body }: { title: string; body: string }) {
  const paragraphs = body.split(/\n\s*\n/).filter(Boolean);
  const sentences = paragraphs[0].split(/(?<=\.)\s+(?=[A-Z])/);
  const teaser = sentences[0];
  const more = [sentences.slice(1).join(' '), ...paragraphs.slice(1)].filter(
    Boolean,
  );
  return (
    <section aria-label={title} className="text-ink-muted text-sm">
      <p>{teaser}</p>
      {more.length > 0 && (
        <details className="text-ink-muted text-xs">
          <summary className="cursor-pointer">More</summary>
          {more.map((p, i) => (
            <p key={i} className="mt-1">
              {p}
            </p>
          ))}
        </details>
      )}
    </section>
  );
}
