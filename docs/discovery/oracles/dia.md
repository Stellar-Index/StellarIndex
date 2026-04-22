# DIA Oracles

**Status:** 🧪 **Discovered during audit — not in our RFP proposal.**
Needs triage: include in aggregation, list as "aware", or defer.

**Source:** `stellar-docs/docs/data/oracles/oracle-providers.mdx`
(2026-04-22 clone).

## Why this matters

DIA is listed by SDF as a first-class oracle provider on Stellar,
alongside Reflector and Band. Our proposal mentions Reflector, Redstone,
Chainlink, and Band but does **not** mention DIA. We need to decide
whether that's a deliberate scope choice or an oversight.

## What DIA is

From the Stellar docs:

- Cross-chain, trustless oracle network delivering verifiable price
  feeds.
- "Sources raw trade data directly from primary markets."
- Supports 20,000+ assets across major classes.
- Custom oracle configuration available per-integrator ("tailored
  sources and methodologies").

## Deployed contracts

### Oracle contracts (from stellar-docs)

| Contract | Data source | Network |
| -------- | ----------- | ------- |
| `CAEDPEZDRCEJCF73ASC5JGNKCIJDV2QJQSW6DJ6B74MYALBNKCJ5IFP4` | External CEXs & DEXs | Testnet |

**Only a testnet contract is listed in the Stellar docs at audit time.**
That's a significant datum — if there's no mainnet deployment, DIA is a
future-looking option, not a live source we can integrate today.

### Supported assets on Stellar (from docs)

| Asset | Native chain | Address |
| ----- | ------------ | ------- |
| USDC  | Ethereum     | `0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48` |
| BTC   | Bitcoin      | `0x0000000000000000000000000000000000000000` |
| DIA   | Ethereum     | `0x84cA8bc7997272c7CfB4D0Cd3D55cd942B3c9419` |

Only three assets — narrow coverage today. Our pricing needs are much
broader (all Stellar assets + SEP-41 Soroban tokens).

## SEP-40 compliance (unconfirmed)

The docs don't explicitly say DIA's Stellar contract is SEP-40
compatible. Given SDF positions it in the same section as Reflector and
Band, it is **likely** SEP-40, but we need to read their contract to
confirm. Action captured in Open items.

## Custom-oracle path

DIA advertises a "Request a Custom Oracle" service — tailored data
sources, pricing methodology, and update triggers. Potentially useful
if a specific CTX Rates customer needs an asset or methodology we
don't otherwise cover. Not Phase 1.

## Verdict

🧪 **Evaluating.** Likely outcome: add to the "aware of, will integrate
if they ship a mainnet deployment with broad asset coverage" list.
Phase 1 integration set is Reflector (primary, on-chain, SEP-40) +
Redstone (on-chain per-symbol contracts per our proposal) + off-chain
HTTP validators (Chainlink, Band REST).

## Open items

- [ ] Fetch DIA's Stellar integration guide:
      <https://www.diadata.org/docs/guides/chain-specific-guide/stellar>
      — confirm SEP-40 compliance, contract interface, mainnet plans.
- [ ] Clone / read their Stellar oracle contract source if public.
- [ ] Monitor DIA for mainnet deployment announcement; reassess if
      they roll out with >100 assets on Stellar.
- [ ] Decide whether we expose a `custom_oracle` config knob in our
      self-hosted deployment so an operator can plug in DIA (or any
      other SEP-40 feed) without our code changes.

## References

- Stellar docs oracle providers:
  `stellar-docs/docs/data/oracles/oracle-providers.mdx`
- DIA website: <https://www.diadata.org/>
- DIA Stellar guide:
  <https://www.diadata.org/docs/guides/chain-specific-guide/stellar>
