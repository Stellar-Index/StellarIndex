# Chainlink

**Status:** ⚠️ Cross-check source only. Public announcement of Stellar
integration exists (Stellar joined Chainlink Scale, 2025/2026); **no
native Soroban Data Feeds contracts confirmed live on mainnet at audit
time** (2026-04-22). We use Chainlink's HTTP APIs as a reference for
anomaly detection until that lands.

## What's announced vs. what's live

### Announced (public)

Per Stellar's announcement
(<https://stellar.org/blog/foundation-news/stellar-to-join-chainlink-scale-and-adopt-data-feeds-data-streams-and-ccip-to-power-next-gen-defi-applications>):

- Stellar is joining the **Chainlink Scale** program.
- Planned integrations: **Data Feeds, Data Streams, and CCIP**
  (Cross-Chain Interoperability Protocol).
- CCIP v1.5 Mainnet Launch targeted for 2026.
- Stated goal: institutional-grade tokenization + cross-chain DeFi.

### Confirmed live at audit time

- ❓ No Stellar Soroban contract addresses for Chainlink Data Feeds
  confirmed in `stellar-docs/docs/data/oracles/oracle-providers.mdx`
  (Chainlink is not listed there, unlike Reflector / Band / DIA).
- ✅ Chainlink's public **HTTP price feeds** (the REST endpoints
  serving their Data Feeds values) are available for off-chain
  cross-check use.

## Our proposal said

> "Chainlink does not have a native Stellar or Soroban deployment.
> Integration will be via Chainlink's HTTP price feed API, providing
> reference prices for major pairs such as BTC/USD and ETH/USD. This
> data is used exclusively as a cross-check signal for anomaly
> detection and is not treated as a primary pricing input.
> Divergence beyond configured thresholds triggers investigation and
> potential source exclusion rather than directly influencing
> aggregated output.
>
> Stellar is part of Chainlink Scale and will be integrating
> Chainlink's Data Feeds, Data Streams, and the Cross-Chain
> Interoperability Protocol (CCIP). Once this has developed further
> we will be well positioned to extend support to cover this new
> functionality."

**Assessment:** this is accurate as of 2026-04. No change required.

## Our integration plan

### Phase 1 (now)

1. **Off-chain HTTP polling** of Chainlink's public Data Feeds API
   (e.g. the `eth_usd` / `btc_usd` aggregator endpoints).
2. **Role: divergence detector.** If our aggregated price for BTC/USD
   (from Soroban DEXes + CEX + Reflector) diverges from Chainlink's
   BTC/USD feed by > threshold, we:
   - Log with full source breakdown.
   - Flag affected API responses with `divergence: true`.
   - Alert ops via our status endpoint.
3. **Do NOT** contribute Chainlink values to VWAP/TWAP aggregation.

### Post-CCIP / Data Streams mainnet launch

Once Chainlink's Soroban Data Feeds are live:

1. Switch from HTTP polling to on-chain contract reads.
2. Subscribe to update events for push-mode consumption.
3. Optionally expose a SEP-40 wrapper over Chainlink feeds for
   consumers who want a unified interface.
4. Re-assess whether Chainlink becomes a VWAP-contributing source or
   stays as cross-check only.

## What we need when Chainlink ships

- Data Feeds contract addresses on pubnet (per-symbol aggregator
  addresses).
- Contract interface (Rust trait, method names, return types —
  likely similar to EVM's `latestRoundData() -> (roundId, answer,
  startedAt, updatedAt, answeredInRound)`).
- Decimals convention per feed.
- Update cadence per feed (EVM is typically 1-hour heartbeat +
  deviation-triggered; Stellar deployment may differ).
- Whether events are emitted on update.

## Cross-check methodology

Per our proposal — divergence bounds trigger **investigation + source
exclusion**, not silent acceptance. Specific thresholds:

- 1% divergence: log only.
- 2% divergence: include `sources_diverging: [...]` in API response.
- 5% divergence: drop divergent source from current aggregation
  window; alert.

These numbers are placeholders — to be tuned with live data. The
mechanism is the firm decision.

## i128 considerations

When Chainlink's Soroban feeds ship, we expect their on-chain answer
type to be `i128` or `i256` (matching EVM `int256`). Same [i128
invariant](../decisions.md) applies; same `stellar-extract`-style
parsing.

## Open items

- [ ] Identify the exact Chainlink HTTP API endpoints we need for
      Phase 1 (BTC/USD, ETH/USD, USDC/USD). Authentication model,
      rate limits, SLA.
- [ ] Set up Chainlink-HTTP polling in our divergence detector
      subsystem.
- [ ] Monitor Chainlink Stellar announcement channels for the actual
      Data Feeds go-live date.
- [ ] Design our SEP-40 wrapper pattern so both Reflector + (future)
      Chainlink feeds can serve the same interface to our consumers.
- [ ] Track **CCIP** separately — it's not a price oracle but a
      cross-chain messaging protocol. If Stellar gains CCIP
      connectivity it affects our asset coverage (e.g. Ethereum-only
      assets bridging in) but doesn't directly affect pricing infra.

## References

- Stellar × Chainlink Scale announcement:
  <https://stellar.org/blog/foundation-news/stellar-to-join-chainlink-scale-and-adopt-data-feeds-data-streams-and-ccip-to-power-next-gen-defi-applications>
- Related: [reflector.md](reflector.md), [redstone.md](redstone.md),
  [band.md](band.md).
