# Exclusions Register

Any scope explicitly excluded from this audit must be recorded
here. The point: future auditors can see what we chose not to
examine and exactly what evidence would re-include it.

## Format

| ID | Excluded thing | Reason | Temp/Perm | Re-entry trigger |
| --- | --- | --- | --- | --- |

## Initial exclusions

| ID | Excluded thing | Reason | Temp/Perm | Re-entry trigger |
| --- | --- | --- | --- | --- |
| X-0001 | R2 + R3 live-state probes (multi-region) | R2 and R3 are not deployed today; only R1 is live | Temp | If R2 or R3 is provisioned, audit them under W21 + W23 |
| X-0002 | Stripe live-mode webhook signature verification | We're not in production-live billing yet; verify the test-mode key path only | Temp | When stripe live-mode keys roll out, re-audit signature path |
| X-0003 | k6 weekly load run live-result analysis | k6 cron runs but results not yet aggregated; this audit reads the workflow + scenarios only, not the latest report | Temp | When the weekly report archive lands, audit results vs SLA expectations |
| X-0004 | Chaos drill live execution | We won't run chaos drills against the running R1 during the audit window; review the scenarios + reports only | Temp | After the next intentional chaos drill |
| X-0005 | External vendor account state | Audit verifies our code's vendor handling but does NOT log into vendor portals (Stripe dashboard, CMC console, etc.) to verify their side | Perm | n/a |
| X-0006 | NEW: Performance comparison against actual CoinGecko / CoinMarketCap API latencies | We benchmark our own latencies via SLA probe; the parity matrix in 08-cgcmc-parity-matrix.md grades feature coverage, not latency | Perm | If launch positioning requires "we're faster than CG/CMC", add a comparative benchmark workstream |
| X-0007 | NEW: Soroban WASM bytecode-level analysis | We trust the contract project's audit + our wasm-history walks (W24). We do NOT disassemble WASM bytecode for backdoors | Perm | If a contract project is found compromised upstream, re-audit that specific contract |
| X-0008 | NEW: Long-duration fuzz testing of the dispatcher / decoders | Time-bounded audit. We fuzz happy + obvious-malformed paths; we do not run multi-week AFL campaigns | Temp | If a decoder is exploited in production, add fuzz-coverage workstream |
| X-0009 | NEW: Live SSH probes against Galexie binary internals | We treat Galexie + stellar-core captive as upstream we depend on. Health probes + metrics only; no code-level audit of Galexie itself | Perm | n/a |

## Re-entry Procedure

To remove an exclusion mid-audit:
1. Add a row to `evidence/log.md` recording the re-entry trigger
   and resulting scope expansion.
2. Open a new workstream or extend an existing one to cover the
   newly-included scope.
3. Update the tracker.

To add an exclusion mid-audit:
1. Add a row above with `X-####` ID, reason, and re-entry
   trigger.
2. Note in the affected workstream that this scope is now
   excluded.
3. The remediation plan should not contain items for excluded
   scope.
