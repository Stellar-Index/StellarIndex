# Stellar SEPs we depend on — reference

**Status:** ✅ Primary-source verified from
`.discovery-repos/stellar-protocol/ecosystem/sep-00<NN>.md` at
clone time (2026-04-22). This closes the "linked but not read"
items from [../adversarial-audit.md §1k](../adversarial-audit.md).

Other SEPs already have their own dedicated audit docs
(SEP-40 → [../oracles/reflector.md](../oracles/reflector.md),
SEP-41 → [sep-41-token-events.md](sep-41-token-events.md)). This
doc covers the three we **reference widely but hadn't read**:
SEP-1, SEP-10, SEP-20, SEP-23.

## SEP-1 — `stellar.toml`

Already audited in dedicated doc:
[../data-sources/sep1-home-domain.md](../data-sources/sep1-home-domain.md).
Summary here:

- Standard file at `https://<home-domain>/.well-known/stellar.toml`.
- TOML format. Sections: `[[CURRENCIES]]`, `[[VALIDATORS]]`,
  `[[ACCOUNTS]]`, plus network config keys
  (`NETWORK_PASSPHRASE`, `WEB_AUTH_ENDPOINT`, `SIGNING_KEY`,
  `HORIZON_URL`, …).
- Self-asserted metadata — we treat `[[CURRENCIES]].max_supply`
  etc. as display-only (`self_declared: true`).

## SEP-10 — Stellar Web Authentication

**Spec version**: 3.4.1 (updated 2024-03-20).
**Status**: Active (production-ready, widely deployed).

### Model

Mutual challenge-response using **Stellar transactions as the
challenge / response format**. Output: a JWT representing the
authenticated session.

### Roles

- **Home Domain** — site hosting stellar.toml with
  `WEB_AUTH_ENDPOINT` (URL) and `SIGNING_KEY` (`G...`).
- **Server** — serves the `WEB_AUTH_ENDPOINT`, holds the
  `SIGNING_KEY`.
- **Client Account** — the Stellar account (`G...`) or muxed
  account (`M...`) being authenticated.
- **Client** — software holding the Client Account's secret.
- **Client Domain** (optional) — additional site verifying the
  calling application.

### Flow (condensed)

```
1. Client GETs <WEB_AUTH_ENDPOINT>?account=<client>
2. Server returns a Stellar transaction (seq=0, multi-op, signed
   by Server's SIGNING_KEY). The first op is a ManageData with
   key "<home_domain> auth" and a nonce value.
3. Client verifies the transaction structure (seq=0, signer=Server,
   first op = ManageData on Client Account, etc.).
4. Client signs the transaction with Client Account's keys.
5. Client POSTs the signed transaction to <WEB_AUTH_ENDPOINT>/token.
6. Server verifies signatures meet Client Account's threshold.
7. Server responds with a JWT.
```

### What this means for our API auth

If we want to support **"sign in with your Stellar wallet"** style
auth on the CTX Rates API — users sign a SEP-10 challenge with
their wallet keypair to get a JWT that authorises their API calls —
this is the protocol.

**Phase-1 choice**: we ship **plain API keys** (simpler, no Stellar
wallet required, consistent with industry norms for pricing APIs).

**Phase-3 optional**: offer SEP-10 as an additional auth method
alongside API keys. Users who want Stellar-native auth (no
registration, auto-account-ownership-proof) can use SEP-10 to
obtain short-lived JWTs, then use the JWT in an Authorization
header the same way an API key is used.

If we ever go multi-tenant or offer **per-Stellar-account rate
limits / tier separation**, SEP-10 becomes the right answer because
it ties quota to provable account ownership.

## SEP-20 — Self-verification of Validator Nodes

**Status**: Active.

### Mechanism

Links a running validator node to a real-world entity via:

1. Create a Stellar account `G…` for the validator node.
2. `SetOptions.home_domain = <your-website>`.
3. On that website, publish `/.well-known/stellar.toml` with a
   `[[VALIDATORS]]` table entry per validator.

### stellar.toml fields per validator

From the spec, the `[[VALIDATORS]]` entry supports:

```toml
[[VALIDATORS]]
ALIAS        = "sdf1"                                          # short identifier
DISPLAY_NAME = "SDF 1"                                         # human-facing
HOST         = "core-live-a.stellar.org:11625"                 # PEER_PORT reachable publicly
PUBLIC_KEY   = "GCGB2S2KGYARPVIA37HYZXVRM2YZUEXA6S33ZU5BUDC6THSB62LZSTYH"
HISTORY      = "http://history.stellar.org/prd/core-live/core_live_001/"
```

At minimum, `PUBLIC_KEY` is required. Filling the rest improves
discoverability and quorum-set inclusion.

### What this means for our Tier-1 aspiration

Per [../decisions.md](../decisions.md) we plan to run three Full
Validators. When we stand them up we publish a
`https://ratesengine.ctx.io/.well-known/stellar.toml` (or similar
domain we own) with three `[[VALIDATORS]]` entries — one per
validator, each linking to its own history archive URL.

Concrete todo for Phase-3 validator track:

1. Generate three validator keypairs (HSM-protected; see
   [../decisions.md](../decisions.md) §validator decision).
2. Fund each validator account with the minimum reserve.
3. Set each validator account's `home_domain = "<our-domain>"`.
4. Publish stellar.toml with three `[[VALIDATORS]]` entries per
   the table above.
5. Three distinct `HISTORY` URLs — one per validator. Our plan to
   use MinIO-behind-nginx works:
   `https://history-1.ratesengine.ctx.io/`,
   `https://history-2.ratesengine.ctx.io/`, etc.

### Security concerns (called out in spec §Security Concerns)

> "With a validator node account linked to a homedomain
> stellar.toml file like suggested, we're really relying on the
> integrity of the stellar.toml file and the server it resides
> on, making sure it only has write access by authorized users."

**Implication:** the server hosting our stellar.toml must have
hardened access control. Compromise of that server would let an
attacker redirect quorum-set-lookups to their own validator keys.

Good practice: serve stellar.toml from a separate infra path with
minimal attack surface, signed by CI pipeline, and consider
serving alongside a signature file per SEP-20's "Additional
security measures" note.

## SEP-23 — Strkeys (address encoding)

**Spec version**: 1.3.0 (updated 2025-02-07).
**Status**: Active.

### The nine strkey types

Every Stellar address / key / signer / object has a **Strkey** — a
base-32 ASCII representation. The first character of the strkey
identifies the type:

| First char | Type                     | Payload        | Notes                              |
| ---------- | ------------------------ | -------------- | ---------------------------------- |
| `G`        | `STRKEY_PUBKEY`          | 32-byte Ed25519 pub key | standard account                   |
| `M`        | `STRKEY_MUXED`           | 32-byte Ed25519 + 8-byte memo ID | muxed account (CAP-27)             |
| `S`        | `STRKEY_PRIVKEY`         | 32-byte Ed25519 secret  | never in our code / logs / DB      |
| `T`        | `STRKEY_PRE_AUTH_TX`     | SHA-256 tx hash         | pre-authorised tx signer           |
| `X`        | `STRKEY_HASH_X`          | SHA-256 preimage hash   | hash-x signer                      |
| `P`        | `STRKEY_SIGNED_PAYLOAD`  | 32-byte key + length-prefixed payload | signed-payload signer (CAP-40) |
| `C`        | `STRKEY_CONTRACT`        | 32-byte SHA-256 contract ID | Soroban contracts                  |
| `L`        | `STRKEY_LIQUIDITY_POOL`  | 32-byte pool hash           | liquidity pool ID                  |
| `B`        | `STRKEY_CLAIMABLE_BALANCE` | 33-byte (type + SHA-256) | claimable balance ID               |

### Our decoder must handle: G, M, C, L, B

The SCAddressType enum (per
[cap-67-unified-events.md](cap-67-unified-events.md)) exposes:

- `SC_ADDRESS_TYPE_ACCOUNT` → G
- `SC_ADDRESS_TYPE_MUXED_ACCOUNT` → M
- `SC_ADDRESS_TYPE_CONTRACT` → C
- `SC_ADDRESS_TYPE_CLAIMABLE_BALANCE` → B
- `SC_ADDRESS_TYPE_LIQUIDITY_POOL` → L

All five show up in post-P23 unified events as `from` / `to`
values. Our decoder must dispatch on the SCAddressType
discriminant and encode to the appropriate strkey type.

The remaining four (`S`, `T`, `X`, `P`) appear only in specific
contexts:

- `S` — never in our indexer / API. If we ever see one we've leaked
  a secret key.
- `T` / `X` — signer types on AccountEntry, not addresses. Our API
  doesn't surface them.
- `P` — signer type (CAP-40 signed payloads). Same.

### Encoding algorithm (for reference)

Per the spec §Specification:

```
1. version_byte = (type_base << 3) | algorithm_value
2. payload      = (algorithm-specific bytes: 32 for Ed25519, 32 for SHA256,
                   33 for claimable balance, etc.)
3. if muxed     : append 8-byte memo ID (network byte order)
4. if signed_payload : append 4-byte length + payload + zero-padding to 4-byte multiple
5. crc16        = CRC16(version_byte || payload || [extras])
6. strkey       = base32(version_byte || payload || [extras] || crc16)
                  RFC4648, no padding
```

**Critical decoding rule** (spec line 107):

> "a strkey's length **must not** be congruent to 1, 3, or 6 mod 8,
> and unused bits of the last symbol must be zero. Some non-padding
> base32 libraries, such as the one in the standard go library —
> `base32.StdEncoding.WithPadding(base32.NoPadding)` — do not
> enforce these requirements. Therefore, implementations of strkey
> decoding **must** check and reject such invalid inputs, or
> perform a round-trip and reject strkey inputs that do not
> re-encode to the exact same string."

**What this means for us:** if we write our own strkey decoder we
follow the round-trip validation rule. If we use
`go-stellar-sdk/strkey` (which we do, directly and transitively
via `stellar-extract`) we rely on their implementation passing the
spec's test vectors.

### Test vectors

The spec §Tests provides reference test cases. If we ever roll our
own strkey code we include those as part of our test suite.

## Summary — SEP-usage matrix

| SEP | Role in our system | Status |
| --- | ------------------ | ------ |
| SEP-1 | Home-domain metadata resolution for classic asset display / max-supply overrides | [../data-sources/sep1-home-domain.md](../data-sources/sep1-home-domain.md) |
| SEP-10 | Optional Phase-3 "sign in with Stellar" auth for our API | This doc |
| SEP-20 | Our Tier-1 validator self-verification publication | This doc |
| SEP-23 | Strkey decoding in our event parser (G, M, C, L, B) | This doc |
| SEP-40 | Oracle consumer interface — Reflector / Band / DIA | [../oracles/reflector.md](../oracles/reflector.md) |
| SEP-41 | Soroban token event parsing | [sep-41-token-events.md](sep-41-token-events.md) |

## References

- Primary sources in `.discovery-repos/stellar-protocol/ecosystem/`:
  `sep-0001.md`, `sep-0010.md`, `sep-0020.md`, `sep-0023.md`.
- Related CAPs: CAP-27 (muxed accounts), CAP-40 (signed payloads),
  CAP-67 (unified events — extends SCAddressType to 5 variants
  covered by strkey types G/M/C/B/L).
