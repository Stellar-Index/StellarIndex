# blend_emitter shape verification -- 2026-07-10

All queries run read-only against the r1 ClickHouse raw lake (HTTP
`:8123`, database `stellar`). Never MinIO / port 9000. Contract:
`CCOQM6S7ICIUWA225O5PSJWUBEMXGFSSW2PQFO6FP4DQEKMS5DASRGRR`.

## 1. Exhaustive shape check -- ALL 465 `distribute` events (not sampled)

```sql
SELECT
  count() AS total,
  countIf(length(base64Decode(data_xdr)) = 72
    AND hex(substring(base64Decode(data_xdr),1,20)) = '0000001000000001000000020000001200000001'
    AND hex(substring(base64Decode(data_xdr),53,4)) = '0000000A'
  ) AS shape_ok,
  countIf(topic_count != 1) AS bad_topic_count
FROM stellar.contract_events
WHERE contract_id = 'CCOQM6S7ICIUWA225O5PSJWUBEMXGFSSW2PQFO6FP4DQEKMS5DASRGRR'
  AND topic_0_sym = 'distribute'
```

Result: `total=465, shape_ok=465, bad_topic_count=0`. Every single
`distribute` event ever emitted -- not a sample -- decodes to
`topic=[Symbol("distribute")]`, `body=Vec[SCV_ADDRESS(contract),
SCV_I128]` (the 20-byte constant prefix through the address-type
tag, the SCV_I128 discriminant at byte 53, and the fixed 72-byte
total length are all exact-matched per row).

## 2. Orphan-topic check -- zero events outside the 4 known kinds

```sql
SELECT count() FROM stellar.contract_events
WHERE contract_id = 'CCOQM6S7ICIUWA225O5PSJWUBEMXGFSSW2PQFO6FP4DQEKMS5DASRGRR'
  AND (topic_0_sym = '' OR topic_0_sym NOT IN ('distribute','drop','q_swap','swap'))
```

Result: `0`. (Also guards against the known CH gotcha where
`topic_0_sym` can be empty for some event families -- confirmed not
the case here.) `in_successful_call`: all 469 rows are `1`.

## 3. Per-kind counts (matches events.go / README exactly)

```
q_swap      1
drop        2
swap        1
distribute  465
```

## 4. Individually decoded events -- drop (x2), q_swap (x1), swap (x1)

Decoded with a from-scratch SCVal/XDR parser (Python, no
go-stellar-sdk dependency, cross-validated against a known-good
value: ledger 51,499,546's contract-instance decode independently
reproduced comet.md's documented WASM hash
`8abc28913035c07411ed5d134e6bfeab4723d97ddd4d1a22a0605d35c94d1a36`
for the Comet backstop pool, confirming the parser is correct).

- `drop` @ ledger 51,499,914 (genesis): `Vec[Vec[Address,i128] x 13]`.
- `drop` @ ledger 57,467,292: `Vec[Vec[Address,i128] x 3]`.
- `q_swap` @ ledger 56,992,670: `Map{new_backstop: CAQQR5SW...
  (Backstop V2), new_backstop_token: CAS3FL6T... (Comet BLND/USDC
  pool), unlock_time: 1749479057}`.
- `swap` @ ledger 57,467,277: `Map{new_backstop: CAQQR5SW...,
  new_backstop_token: CAS3FL6T..., unlock_time: 1749479057}` --
  BYTE-IDENTICAL field values to the `q_swap` above, confirming
  decode.go's claim that the observed `swap` executed exactly what
  the observed `q_swap` queued.

## 5. Sampled `distribute` events across the full lifetime (first/mid/last)

First 3 (earliest): ledgers 51,524,666 / 51,546,386 / 51,555,835 --
recipient `CAO3AGAM...` (Backstop V1).
Mid 6 (spread): ledgers 52,638,893 / 52,854,815 / 55,599,139 /
58,778,993 / 60,277,837 / 61,689,188 -- recipient flips from
`CAO3AGAM...` (V1) to `CAQQR5SW...` (V2) between ledgers 55.6M and
58.8M, consistent with the V1->V2 backstop swap (`q_swap`/`swap`
above) at ~L57.47M.
Last 3 (latest): ledgers 63,321,039 / 63,350,431 / 63,380,088 --
recipient `CAQQR5SW...` (Backstop V2).

Every one of the 12 individually-inspected samples decodes to the
identical `Vec[Address,i128]` shape as the exhaustive check in §1 --
i.e. the body shape did not drift when the backstop recipient
changed, which is exactly the kind of value-level protocol event
(not a decoder-shape change) the "Soroban DeFi contracts upgrade in
place" caveat warns about being conflated with a real schema break.

## 6. WASM-upload-ledger claim -- NOT corroborated for 2 of 3

`internal/sources/blend_emitter/events.go`'s package doc claims "up
to 3 WASM uploads (ledgers 51,351,843 / 51,498,920 / 52,314,704)".
Checked against `stellar.ledger_entry_changes` (`entry_type IN
('contract_data','contract_code','ttl')`, unrestricted to any single
contract -- i.e. checking whether ANYTHING Soroban-related happened
on the whole network at these exact ledgers):

- `51,351,843` (+/-1000 ledgers): zero `contract_data` / `contract_code`
  rows network-wide. Only classic `trustline`/`offer`/`claimable_balance`
  entries.
- `51,498,920` (+/-1000 ledgers): same -- zero Soroban entry activity.
- `52,314,704`: ONE `contract_code` row exists, `change_type='state'`,
  key hash `438a5528cff17ede6fe515f095c43c5f15727af17d006971485e52462e7e7b89`
  -- the SAME hash confirmed as the Emitter's only observed WASM (see
  `evidence/blend_emitter/emitter-438a5528cff17ede.wasm`), not a
  distinct third version.

See `blend_emitter.md` "WASM timeline" for the interpretation --
this doc's own hedge ("up to 3 uploads") is not something the audit
could independently confirm from the lake, and the one ledger that
IS corroborated shows the same hash already established elsewhere,
not a new one.
