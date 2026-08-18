# Phoenix — contract & event verification

> **For the Phoenix team:** this is the complete set of Phoenix contracts
> and events Stellar Index ingests. Please confirm the factory, multihop,
> and pool list are correct and complete — **especially any pool not
> listed here**, since we can't enumerate them from on-chain events (see
> below) and rely on this list being complete.
>
> - **Enumeration method:** RPC view — the factory's `query_pools()` plus
>   the multihop contract (Phoenix pools were created before our lake's
>   earliest ledger, 50.46M, so there are **no `create` events in the lake**
>   to enumerate from).
> - **Last verified:** 2026-08-18 (r1 lake event activity; the original
>   pool list was the 2026-05-01 WASM-history walk — see the 2026-08-18
>   completeness-gap update below).
> - **Gate status:** ✅ Gated code-side (2026-07-02, ADR-0040 §1 mechanism 2
>   — curated-set registry: the **12 pools + 16 stake contracts** below are
>   the in-code seed `phoenix.MainnetGatedSet`; factory creation events
>   predate the lake so the seed is the trust root). Operator rollout
>   remaining per ADR-0040 §2: deploy, lake re-derive, one green verdict
>   cycle. An unlisted pool/stake contract fail-closes into a recognition
>   gap.
>
> **2026-08-18 completeness-gap update:** a `-ch` projection reconcile
> found the 2026-05-01 snapshot was INCOMPLETE — 1 pool
> (`CAZ6W4WH…`, 455 served `phoenix_liquidity` rows) and 13 per-pool
> stake contracts (2,513 served `phoenix_stake_events` rows, several
> STILL emitting to ledger ~64.0M) were gated out and scored
> `expected=0`. All 14 were VERIFIED genuine against the r1 lake
> (`stellar.contract_events`) — the pool + 11 stakes co-occur in their
> phoenix-factory pool-create transactions; the 2 pre-lake stakes
> (`CDOXQONPND…`, `CDEQYRWFU…`) are driven by the phoenix reward keeper
> `CBZ7M5B3…` that also drives the seeded stakes and emit the phoenix
> stake-v1.1 migration events — and added to the seed. A served-tier
> re-projection of the `[51,019,036 … tip]` gated-but-unseeded window is
> the required operator follow-up to backfill the still-active
> contracts' rows.

## Factory & multihop

| Role | Contract | Lake events | Notes |
|---|---|---|---|
| Factory | `CB4SVAWJA6TSRNOJZ7W2AWFW46D5VR4ZMFZKDIKXEINZCZEGZCJZCKMI` | none | Emits `("create","liquidity_pool")`, but only before our lake. Pools enumerated via its `query_pools()` view. |
| Multihop | `CCLZRD4E72T7JCZCN3P7KNPYNXFYKQCL64ECLX7WP5GNVYPYJGU2IO2G` | `initialize` ×1 | **Emits no `swap` events** — it relays to pools, so a pool-only gate loses no trades. |

## Pools (12)

Pools that have emitted `swap` in the lake are marked **active** with their
swap count; the rest are in the factory's `query_pools()` but have no swap
activity in our window.

| Pool | Lake activity |
|---|---|
| `CAZ6W4WHVGQBGURYTUOLCUOOHW6VQGAAPSPCD72VEDZMBBPY7H43AYEC` | **active** — 23,672 swap (+ provide/withdraw_liquidity, initialize); factory-created @51,572,101, swap activity ended ~54.5M. Added 2026-08-18 (completeness-gap; snapshot missed it). |
| `CBHCRSVX3ZZ7EGTSYMKPEFGZNWRVCSESQR3UABET4MIW52N4EVU6BIZX` | **active** — 43,918 swap (+ provide/withdraw_liquidity) |
| `CBCZGGNOEUZG4CAAE7TGTQQHETZMKUT4OIPFHHPKEUX46U4KXBBZ3GLH` | **active** — 4,233 swap (+ provide/withdraw_liquidity) |
| `CD5XNKK3B6BEF2N7ULNHHGAMOKZ7P6456BFNIHRF4WNTEDKBRWAE7IAA` | **active** — 2,872 swap |
| `CBISULYO5ZGS32WTNCBMEFCNKNSLFXCQ4Z3XHVDP4X4FLPSEALGSY3PS` | **active** — 1,736 swap |
| `CDMXKSLG5GITGFYERUW2MRYOBUQCMRT2QE5Y4PU3QZ53EBFWUXAXUTBC` | **active** — 48 swap |
| `CB5QUVK5GS3IU23TMFZQ3P5J24YBBZP5PHUQAEJ2SP5K55PFTJRUQG2L` | **active** — 25 swap |
| `CC6MJZN3HFOJKXN42ANTSCLRFOMHLFXHWPNAX64DQNUEBDMUYMPHASAV` | **active** — 8 swap |
| `CBW5G5SO5SDYUGQVU7RMZ2KJ34POM3AMODOBIV2RQYG4KJDUUBVC3P2T` | no lake events |
| `CCKOC2LJTPDBKDHTL3M5UO7HFZ2WFIHSOKCELMKQP3TLCIVUBKOQL4HB` | no lake events |
| `CCUCE5H5CKW3S7JBESGCES6ZGDMWLNRY3HOFET3OH33MXZWKXNJTKSM3` | no lake events |
| `CDQLKNH3725BUP4HPKQKMM7OO62FDVXVTO7RCYPID527MZHJG2F3QBJW` | no lake events |

## Stake contracts (16 — separate from the pools)

`bond` / `unbond` events come from per-pool **stake contracts**, which are
distinct addresses **not** returned by `query_pools()`. Original 3 (found
active in the 2026-05-01 walk):

```
CBRGNWGAC25CPLMOAMR7WBPOF5QTFA5RYXQH4DEJ4K65G2QFLTLMW7RO   bond ×24
CAF3UJ45ZQJP6USFUIMVMGOUETUTXEC35R2247VJYIVQBGKTKBZKNBJ3   unbond ×21
CBBUVHCEML7UE46XXZXLTMGKFMKX7KOC2XAKI3TW6WBQBKWMSARMU3YM   bond ×10
```

13 more added 2026-08-18 (completeness-gap; all VERIFIED genuine — see the
update note at the top). The first 11 co-occur in their pool's
phoenix-factory pool-create transaction (paired 1:1 with a curated pool);
the last two pre-date the lake and are driven by the phoenix reward keeper
`CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32`:

```
CABWEFVXUB3XWYPTWFETEGJR2WRGE2ZKYYLZDLV3EBUVFMOU4ENK4DJC   ↔ pool CBHCRSVX (factory @51,572,026)
CAIR3UPW2PEP27QZWX4XGMO65W6LJ3XCRA3F5G7Z3D52MNOVF5K5YZ56   ↔ pool CBCZGGNO (factory @51,572,030)
CDP6DT2YU75ZMOPTTCQ563H2XZDDWHPWKRQ6N2W5LNVE5HHRSB4MMRNQ   ↔ pool CAZ6W4WH (factory @51,572,101)
CB2S5X4H6ZMMCDQV4DNKEO2SBSW7T2YXVN5A7G2BBSN3VM73CQYIIZ3C   ↔ pool CBISULYO (factory @51,927,948)
CCP653KENMYCAYQ3PHJDT6PITMG4XYKVWV3OEDDCOAOS6Z4GOMXGYH3Z   ↔ pool CDQLKNH3 (factory @53,853,219)
CCIWIW6ESCCCFMEI5QOSUHDKTMBEMRJ22F7GPYNRKM2UI2FH6WYUKOUU   ↔ pool CBW5G5SO (factory @53,853,220)
CBULEXIMZ5C4CSUPZ4E5LXATWDZNS6MDM2A57DAUD5GXSUG4IWKLOSOC   ↔ pool CDMXKSLG (factory @53,955,603)
CD2YKNPX3JPTGDANJRPEJS42MPQLEVUVVRZKJYLLUSPJKQJA7LUANBO4   ↔ pool CC6MJZN3 (factory @54,953,243)
CDBMVFP7KJXW3YEFSLOU5GYUQHHJJI7QPZJPCSPDK6HHBCBZAMCHS2QY   ↔ pool CB5QUVK5 (factory @54,953,245)
CDH6JILIADIC5SKE6OZJAYV3GM62RTR4O54OMVNP4ZOK4HH4J2JWJPVW   ↔ pool CCKOC2LJ (factory @54,953,247)
CBDCTYZSZIOWCK5IGCQZNFUOJ53KMPYG2MG7GMVGE3A2LEYCFTDYYZ3S   ↔ pool CCUCE5H5 (factory @54,953,248)
CDOXQONPND365K6MHR3QBSVVTC3MKR44ORK6TI2GQXUXGGAS5SNDAYRI   pre-lake; 260 shared tx w/ curated pools + keeper (still emitting → ~64.0M)
CDEQYRWFU3IHPRR6H6VOQRUU3JFS6DTUYUL4YAQSD3ALB5IPBTEOZUFM   pre-lake; keeper + phoenix stake-v1.1 migration events
```

There may be still more stake contracts (one per pool) that haven't emitted
bond/unbond yet. **Please send the complete pool → stake-contract mapping.**

**Note on completeness:** the `swap` topic is emitted by 49 distinct
contracts in our lake (most are other AMMs), and `withdraw_liquidity` by
75 — so we **cannot** reverse-derive or verify the complete Phoenix pool
set from event topics, and Phoenix's pool-creation events predate our lake,
so we have no live signal for new pools. The pool list above is the
factory's `query_pools()` snapshot (2026-05-01); a gate built on it would
**silently drop** any pool or stake contract not on the list. This is why
we need the team to confirm completeness (or a `query_pools()` we can
re-poll) **before** enforcing the gate. **If Phoenix has deployed pools or
stake contracts since 2026-05-01, please send the additions.**

## Events decoded

Verified against `phoenix-contracts` `pool/src/contract.rs`. Each Phoenix
action emits **multiple field-named events** (e.g. a swap emits 8) that we
correlate by `(ledger, tx_hash, op_index)` into one trade.

| Action (topic[0]) | Where it lands |
|---|---|
| `swap` | `trades` (source=phoenix) |
| `provide_liquidity`, `withdraw_liquidity` | `phoenix_liquidity` |
| `bond`, `unbond`, `withdraw_rewards`, `distribute_rewards` | `phoenix_stake_events` |

## Rewards topics — HANDLED (ROADMAP #89, 2026-07-10)

A topic census found `withdraw_rewards` (40 events) and
`distribute_rewards` (18 events) — the stake contract's reward-claim
surface, distinct from `bond`/`unbond`. Real-lake-bytes verified
(ledgers 53588319 / 53587626): `withdraw_rewards` is a 2-field-event
action (`user`, `reward_token`); `distribute_rewards` is a single-
field, pool-wide announcement (`asset`, no user). Neither carries an
amount — the paid-out amount surfaces on the reward token's own
SEP-41 `transfer` event in the same op, not correlated here (would
need a cross-decoder join against `sep41_transfers`). Both land in
`phoenix_stake_events` (migration 0098 made `user_addr`/`amount`
nullable and repurposed `lp_token` to carry the reward-token /
distributed-asset address for these two actions). See
`internal/sources/phoenix/README.md` + `events.go`'s "Reward actions"
doc + `rewards_test.go` for the evidence trail.
