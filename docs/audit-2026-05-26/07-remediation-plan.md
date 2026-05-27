# Remediation Plan

Maps every `open` finding from
[05-findings-register.md](05-findings-register.md) to a wave + owner.

The audit doesn't dictate WHO fixes things; it dictates WHAT must
be fixed and by WHEN (relative to the public flip).

## Wave taxonomy

| Wave | Window | Closure rule |
| --- | --- | --- |
| **W0** | Before public flip | Critical + every High that blocks launch-day commitments |
| **W1** | Within 7 days of public flip | Remaining High + every Medium that lands an in-flight customer commitment |
| **W2** | Within 30 days of public flip | Remaining Medium + Low that affect observability or ergonomics |
| **W3** | Backlog | Low + Note; closed opportunistically |

## Wave assignment table

Updated as findings land.

| Wave | Finding | Title | Owner | Closure target |
| --- | --- | --- | --- | --- |
| **W0 — LAUNCH-BLOCKING (the cascade + critical/high impact)** | | | | |
| W0 | F-0001 | r1 root partition 100% full | ops | immediate |
| W0 | F-0039 | Redis MISCONF stop-writes-on-bgsave-error | ops | immediate (depends on F-0001) |
| W0 | F-0041 | Redis persistence dir on root partition | ops + platform | move to data partition |
| W0 | F-0053 | Prometheus TSDB on root partition | ops + platform | move to data partition |
| W0 | F-0080 | aggregator_silent alert unguarded `rate()==0` | platform | add `OR absent_over_time(...)` per F-0081 template |
| W0 | F-0085 | `redis_writes_blocked` alert blind because redis_exporter DOWN | ops | restart exporter + add exporter-down alert |
| W0 | F-0036 | Counters tied to cascade (revised) | platform | covered by F-0080 fix |
| W0 | F-0027 | Silent alerts cluster | platform | covered by F-0080 |
| W0 | F-0086 | `/v1/oracle/latest` HTTP 500 under cascade | platform | depends on F-0090 + F-0039 |
| W0 | F-0087 | `/v1/lending/pools` HTTP 500 under cascade | platform | depends on F-0090 + F-0039 |
| W0 | F-0089 | `/v1/vwap` + 3× /v1/oracle/* HTTP 500 under cascade | platform | depends on F-0090 + F-0039 |
| W0 | F-0049 | Signup IP throttle fails OPEN on Redis error | platform | fail-CLOSED with 503 |
| W0 | F-0050 | Global rate-limit fails OPEN on Redis error | platform | fail-CLOSED with 503 |
| W0 | F-0055 | `/v1/status` reports `ok` while every signal is `unknown` | platform | self-consistency check |
| W0 | F-0045 | redis_exporter DOWN | ops | restart + alert |
| W0 | F-0046 | postgres_exporter DOWN | ops | restart + alert |
| W0 | F-0047 | pgbackrest_exporter DOWN | ops | restart + alert |
| W0 | F-0048 | minio scrape 403 | ops | fix scrape auth |
| **W1 — Public-flip + 7 days** | | | | |
| W1 | F-0003 | Migration deploy operator-manual | ops + platform | before next migration |
| W1 | F-0028 | Diagnostics/cursors public exposure (F-0034) | platform | gate behind auth |
| W1 | F-0056 | GH Actions tag refs not SHA-pinned | platform | SHA-pin via Dependabot |
| W1 | F-0058 | dependabot.yml missing github-actions ecosystem | platform | add 4-line block |
| W1 | F-0070 | TolerateTrailingMissing opt-in inconsistent | platform | helper-ify in single place |
| W1 | F-0071 | `/v1/ohlc` single-bar (CG/CMC parity gap) | platform | implement multi-bar series |
| W1 | F-0074 | `/v1/twap`+`/v1/ohlc` lack LKG fallback chain | platform | mirror price's priceFallback |
| W1 | F-0090 | Routes return 500 not 503 on Redis MISCONF | platform | infrastructure-error → 503 mapping |
| W1 | F-0040 | PHO supply discrepancy (med) | platform | investigate |
| W1 | F-0083 | R2/R3 inventories example-only | ops | provision when geo-redundancy promised |
| **W2 — Public-flip + 30 days** | | | | |
| W2 | F-0002 | Memory entry obsolete | claude/ops | maintenance |
| W2 | F-0007 | btmp brute-force noise | ops | fail2ban / sshguard |
| W2 | F-0011 | API main.go 3K LOC (refactor only) | platform | extract themes (F-0069) |
| W2 | F-0029 | … | platform | (per F-0029 detail) |
| W2 | F-0034 | /v1/diagnostics public | platform | covered by F-0028 |
| W2 | F-0051 | No TLS cert expiry alert | ops | add cert_exporter + alert |
| W2 | F-0057 | govulncheck in CI but not make verify | platform | add make vuln target |
| W2 | F-0061 | `/v1/price` vs `/v1/twap`/`/v1/ohlc` param inconsistency | platform | unify shape (also F-0068, F-0073, F-0091) |
| W2 | F-0062 | `/v1/changes` not a price-change endpoint | platform | document + add `/v1/price/change` |
| W2 | F-0065 | `/v1/markets last_trade_at` is bucket-end | platform | relabel field |
| W2 | F-0072 | `/v1/twap` no window= (reframed) | platform | bundled with F-0061 |
| W2 | F-0069 | API main.go 3K LOC | platform | refactor |
| **W3 — Backlog / opportunistic** | | | | |
| W3 | F-0008 | (minor) | — | — |
| W3 | F-0009 | (minor) | — | — |
| W3 | F-0010 | (minor) | — | — |
| W3 | F-0015 | (minor) | — | — |
| W3 | F-0026 | (minor) | — | — |
| W3 | F-0063 | `/v1/markets` XLM ranking nit (downgraded) | platform | XLM section |
| **POSITIVE — no action required (preserved for context)** | | | | |
| — | F-0042 | `/v1/readyz` honest about Redis degraded | — | accepted POSITIVE |
| — | F-0059 | WASM-audit coverage complete | — | accepted POSITIVE |
| — | F-0067 | Live ledger ingestion healthy | — | accepted POSITIVE |
| — | F-0076 | SEP-10 JWT validation done right | — | accepted POSITIVE |
| — | F-0077 | API key entropy + storage sound | — | accepted POSITIVE |
| — | F-0078 | Migrations 0042-0045 NUMERIC honoured | — | accepted POSITIVE |
| — | F-0079 | ADR-0029 SQL backfill design shipped | — | accepted POSITIVE |
| — | F-0081 | ingestion_source_stopped guards via label_replace | — | accepted POSITIVE |
| — | F-0082 | supply-* uses absent_over_time correctly | — | accepted POSITIVE |
| — | F-0084 | cross-region-check tooling ships | — | accepted POSITIVE |
| — | F-0088 | `/v1/pools` strong granular Stellar surface | — | accepted POSITIVE |
| — | F-0092 | `/v1/sac-wrappers` strong CG/CMC differentiator | — | accepted POSITIVE |

## Cascade-fix sequence (Wave 0 ordering)

The Wave 0 findings cannot be fixed independently — they cascade.
Operator-execution order:

1. **F-0001 (root disk)** — free disk space (rotate logs, clear journal,
   move stuck artefacts). After this, F-0039 should auto-recover the
   next BGSAVE cycle. Watch:
   ```
   redis-cli config set stop-writes-on-bgsave-error no   # emergency
   redis-cli bgsave                                       # force a save
   redis-cli info persistence | grep last_bgsave_status
   ```
   then reset `stop-writes-on-bgsave-error yes` once disk pressure clears.
2. **F-0041 + F-0053** — schedule a move of `/var/lib/redis` and
   `/var/lib/prometheus` to the ZFS data pool (out of root). Reduces
   amplifier risk for any future root-disk pressure.
3. **F-0045..F-0048** — restart the down exporters; add `up{job=...}
   == 0` rules for each.
4. **F-0080** — rewrite aggregator_silent alert with the F-0081
   `label_replace(vector(1), ...)` template OR
   `absent_over_time(...)` guard.
5. **F-0085** — add the meta-alert `redis_exporter_down` so future
   cascades surface even if the exporter dies first.
6. **F-0049 + F-0050** — change rate-limit + signup-throttle to
   fail-CLOSED with HTTP 503 on Redis errors (NOT open).
7. **F-0086, F-0087, F-0089** — route handlers return 503 not 500
   when Redis errors (F-0090).
8. **F-0055** — fix `/v1/status` self-consistency: if every signal
   is `unknown` + zero-time, `overall` must NOT be `ok`.

After these 8 steps, the cascade can no longer go silent, the
public surfaces degrade with proper RFC-7807 + 503 semantics, and
the abuse-defence layer can't be bypassed by knocking out Redis.

## Wave 0 effort estimate

Conservative operator-time + code-change estimates for the
14-step Wave 0 sequence. Steps 1-5 + 9-12 are operator-side
(SSH + Ansible re-runs). Steps 6-8 + 14 require PRs + CI +
deploy.

| Step | Action | Operator | Code | Total wall-clock |
| --- | --- | --- | --- | --- |
| 1 | Free root disk (journalctl vacuum + log rotate + clear wasm-history stderr) | 5-10min | — | 10 min |
| 2 | Reset Redis bgsave + clear MISCONF | 2 min | — | 2 min |
| 3 | Restart 4 down exporters + fix minio scrape auth | 10 min | — | 10 min |
| 4 | Edit aggregator.yml expr + promtool check + Prometheus reload | 5 min | — | 5 min |
| 5 | Add exporter-down meta-alerts (4 rules) | 10 min | — | 10 min |
| 6 | F-0049/F-0050 fail-CLOSED — PR + CI + deploy | — | 1-2h (code + tests + 2 file changes) | 3h incl. deploy |
| 7 | F-0086/0087/0089/0090 — 503 + Retry-After on cascade-affected routes | — | 1-2h (4 handlers + tests + middleware) | 3h incl. deploy |
| 8 | F-0055 — /v1/status self-consistency | — | 30min (single handler) | 1h incl. deploy |
| **Op-side subtotal** | — | **~30 min** | — | **30 min** |
| **Code-side subtotal** | — | — | **~3-5h dev** | **~7h incl. CI + deploy** |
| 9 | F-0137 — wire install.sh into Ansible | 30 min | — | 30 min |
| 10 | F-0134 — re-run install.sh on r1 | 2 min | — | 2 min |
| 11 | F-0135 — SCP missing rule file + reload | 5 min | — | 5 min |
| 12 | F-0139 — fix alertmanager path mismatch + sync | 30 min | — | 30 min |
| 13 | F-0140 — audit prometheus_pair role target | 1-2h investigation | — | 2h |
| 14 | F-0142 — `make verify-r1-sync` target | — | 1-2h | 2h |
| **Drift-side subtotal** | — | **~1h ops** | **~1-2h code** | **~3h** |
| **TOTAL** | — | **~1.5h ops** | **~4-7h dev** | **8-10h end-to-end** |

The op-side fixes (steps 1-5 + 9-12) **unblock the cascade
within ~30 min of work.** Code-side fixes (6-8 + 14) close the
structural holes but can lag the operational recovery by hours
to days without re-risking the cascade.

**Recommended execution order:**

1. **Hour 1:** Steps 1-3 (free disk, reset Redis, restart exporters)
2. **Hour 2:** Steps 4-5 + 9-11 (guard alerts, restart smoke install)
3. **Day 1 PR cycle:** Step 12 (alertmanager) + Step 13 (role investigation) + Step 8 (status fix)
4. **Day 1-2 PR cycle:** Steps 6-7 (fail-CLOSED rate-limit + 503 mapping)
5. **Day 3:** Step 14 (drift-sync verification — preventive)

## Closure rules per wave

### W0 — Launch-blocking

A W0 finding is closed only when:
1. Code/docs/infra change merged (PR linked).
2. Verify rerun green.
3. Post-change R1 probe shows the fix in place.
4. Finding's row in 05-findings-register.md updated to
   `closed-by-PR-####`.

### W1 — Public-flip + 7 days

Same closure rule as W0. The 7-day window assumes no major
customer commitment hangs on this finding.

### W2 — Public-flip + 30 days

Closure rule relaxes step 3: a verifiable code change is
sufficient; R1 probe required only when the finding touches a
runtime invariant.

### W3 — Backlog

Closed opportunistically. No SLA.

## Templates

### Code-change finding

```
- finding: F-####
- pr: <link>
- verify: <verify.sh output ref>
- post-change probe: <r1-probe ref if relevant>
- closed at: YYYY-MM-DD
```

### Doc-change finding

```
- finding: F-####
- pr: <link>
- docs-lint: <ci output ref>
- closed at: YYYY-MM-DD
```

### Infra-change finding

```
- finding: F-####
- change: ansible playbook / systemd unit / terraform if any
- pre-state ref: <r1-probe before>
- post-state ref: <r1-probe after>
- closed at: YYYY-MM-DD
```

### Accepted-risk finding

```
- finding: F-####
- reason: <why accepting>
- operator: <who agreed>
- review date: <when to re-evaluate>
- mitigations in place: <if any>
```
