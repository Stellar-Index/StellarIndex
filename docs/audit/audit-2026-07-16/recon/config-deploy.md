# Recon: config surface + deploy/infra + startup/shutdown (HEAD f84e2d0b)

## Config (internal/config, 215 toml fields, struct-tag driven, strict unknown-key reject)
- **ENV OVERRIDE TRAP**: `STELLARINDEX_POSTGRES_DSN` silently replaces `storage.postgres_dsn` (load.go:69). Ansible writes it to `/etc/default/stellarindex` (EnvironmentFile), so the on-disk TOML DSN is decorative in prod. Also overridden: REDIS_PASSWORD, CLICKHOUSE_SERVING_PASSWORD, several vendor keys, STRIPE_WEBHOOK_SECRET.
- **TWO CONFLICTING `*_env` CONVENTIONS** (footgun/bug class): `S3AccessKeyEnv`/`S3SecretKeyEnv` hold the NAME of an env var (deliberately no `env:` tag after audit A16-01); `RedisPassword`/`ClickHouseServingPassword`/`SEP10.SeedEnv`/`JWTSecretEnv`/`ResendAPIKeyEnv` hold the VALUE despite `*_env` toml names. Mixed semantics on similarly-named fields.
- Indexer reads COINGECKO_API_KEY/DEMO directly at connector build, bypassing config (indexer main.go:923).

### Dangerous / behavior-changing defaults
- `ingestion.enabled_sources` = `["soroswap","aquarius","phoenix"]` (NOT empty).
- `storage.clickhouse_live_sink`=true, `clickhouse_projector_source`=true.
- `hashdb.enabled`=false (opt-in).
- `aggregate.min_usd_volume`=10000, `outlier_sigma_threshold`=4 (0 disables), `interval_seconds`=30.
- rate limits: anon 60/min, key 1000/min; ops upgrade-key ladder 1000/10000/50000.
- **`api.listen_addr`=`0.0.0.0:3000` + `trusted_proxy_cidrs`=`[]` + `allowed_origins`=`["*"]`** — exactly what warnUnsafeBind/warnOpenCORS warn about, but **warn-only, never fatal** (except allow_credentials+`*` panics). Prod ansible sets trusted_proxy_cidrs=["127.0.0.1/32"].
- `api.auth_mode`=none default; `auth_backend`=redis; postgres backend disables /v1/account/keys self-service (split-brain guard).
- `signup_require_email_verification`=true; `signup_reaper.enabled`=true.
- `region.id`=`"r1"` default — **a mis-deployed region silently claims r1's identity**.
- Feature flags OFF by default: anomaly, price_alerts, divergence.supply, supply.aggregator_refresh, all external.*, divergence.chainlink. ON: signup_reaper, divergence.{coingecko,reflector,redstone,band}, triangulation, cdn.
- Cross-validation at load: SDF reserve accts need balance entries; refresh ≥30s; fully_wrapped_sacs ⊆ sac_wrappers; USD-pegs must be classic 7-dp (10^7 safety); trusted CIDRs validated.

### Secrets
- Never in TOML (convention). Prod in ansible-vault `inventory/r1.secrets.yml` (gitignored, verified). Rendered to /etc/default/stellarindex mode 0640.
- `storage.postgres_dsn` default = passwordless + `sslmode=disable` (relies on env override in prod). Chainlink RPC URL embeds Alchemy key (treated as secret via env). No other committed secret defaults.

## Deploy / infra
- **deploy.yml** manual-dispatch only, region enum [r1] only. Hardened: env-passed input validation (F-1298 shell-injection), SHA256SUMS verify, pinned known_hosts hard-fail (F-1297), pinned ansible. One deploy/region concurrency.
- **deploy-binary.yml**: migrations `up` BEFORE binary swap (F-1220), per-binary stage→backup(.prev-<tag>)→atomic rename→restart→health probe→rollback binary; **migrations NEVER rolled back (CS-099)**. `migrations_skip=true` is an operator bypass.
- **run-heavy-job.sh**: transient scope MemoryMax=20G, MemorySwapMax=0, CPU/IO weight 50, root-disk watchdog stops <2GiB free. Galexie MemoryLow=16G + weight 500.
- Caddy TLS (LE), trusts CF-Connecting-IP only from CF static ranges (hardcoded, quarterly manual refresh); API trusts only Caddy (127.0.0.1/32).

## Startup / shutdown
- All 3 daemons: signal.NotifyContext, 30s drain, cancel-registered-LAST so workers unwind before store/redis close (F-1350). Indexer: ordered teardown avoids send-on-closed panic; cursor advanced only on successful upsert (kill mid-ledger resumes at cursor+1, CS-029).
- **Deploy-restart-mid-backfill**: ops backfill jobs are NOT systemd services (deploy restarts only indexer/aggregator/api), so a Phase-0 backfill under run-heavy-job.sh SURVIVES a deploy but shares host memory/IO with the restart. backfill/ch-backfill/projected-rebuild resumable+idempotent. **This is why the deploy freeze matters: the swap of the ops binary + service restart contends with Phase 0.**
- migrate: advisory-lock serialised, `up` idempotent, `force` manual dirty escape.

## Config/deploy INVARIANT TIERS (recipe §5/§6 seed)
| Invariant | Enforcement | Tier |
|---|---|---|
| r1 config codified in ansible | weekly ansible-drift --check --diff, fails changed>13 | **automated-weekly, but ≤13-change allowance + weekly cadence = drift window** |
| One heavy job at a time | run-heavy-job.sh caps resources; unit name `heavy-<name>-$$` — **NO lock, concurrency NOT prevented** | **CONVENTION-ONLY** (prose in CLAUDE.md:437) ← weak link |
| Heavy jobs can't starve galexie | MemoryMax + galexie MemoryLow + root watchdog | automated (cgroups) |
| Migrations before swap, never rolled back | deploy playbook + CS-099 | automated on workflow path; migrations_skip bypass |
| Migrate needs explicit DSN | hard fail no default | automated |
| Two migrate runners serialise | PG advisory lock | automated (DB) |
| Main protected / required checks / signed | GitHub-side setting | **NOT verifiable from repo** — checks exist, "required" is a GH config claim |
| XFF only from trusted proxies | Caddy CF ranges + API middleware; empty list=ignore XFF | automated runtime; CF IP list freshness = convention (quarterly manual) |
| Unsafe bind / wildcard CORS+auth | boot WARN only (1 hard panic case) | warn-only |
| Unknown config keys / defaults match tags | loader hard error + TestDefault_MatchesStructTags | automated |
| Destructive archive trim safety | dry-run default, --commit, upstream verify, max-files cap | automated CLI guards |
| One writer per projected range | projected-rebuild refuses live-cursor overlap unless -allow-live-overlap | automated + bypass flag |

## Additional leads
- **Two divergent copies of systemd units**: repo `deploy/systemd/*` (EnvironmentFile=/etc/default/stellarindex-ops) vs ansible `templates/systemd/*.j2` (/etc/default/stellarindex). Only ansible copies drift-checked → repo copies can rot.
- ops `mint-key` emits plaintext API key once to stdout. `classic-movements-backfill` hard-clamps below P23 (58762517). `reconcile-balances` exit code = mismatch count (cap 255).
- ansible-drift.yml uses live ssh-keyscan (:38) unlike deploy.yml's pinned known_hosts — MITM window in the drift check.
