# `test/chaos/` — failure-mode chaos suite

Per [Task #75 design note](../../docs/architecture/chaos-suite-design-note.md).

Forced failure of one stack component at a time, asserting the API
either **stays up degraded** or **fails loudly with a structured
envelope** — never silently serves bad data.

The Wave 1 scope here is **smoke against the docker-compose dev
stack**: kill / partition one container at a time, verify behaviour.
Wave 2 (post-launch) extends to the staging baremetal stack with
Patroni replica promotion, Sentinel failover, and HAProxy VIP
flips — those need infrastructure beyond `make dev`.

## Wave 1 scenarios (this PR)

| File | Stresses | Pass criteria |
|---|---|---|
| `scenarios/01-redis-down.sh` | rate-limit fail-open + Redis-fed read paths | API `/v1/healthz` returns 200/503 throughout; recovers within 30s |
| `scenarios/02-timescale-down.sh` | DB-backed read paths fail loudly | `/v1/markets` 5xx OR Redis-cached body; never silent-empty 200 |
| `scenarios/03-redis-network-partition.sh` | go-redis cold-conn timeout vs connection-refused | ≤ 1 transient sample failure during 30s partition; clean recovery |

## Wave 2 scenarios (deferred)

These need staging baremetal with the production HA topology
(`configs/ansible/`-deployed). Tracked separately from Task #75:

- Postgres primary kill → Patroni replica promotion within 30 s.
- Redis master kill → Sentinel failover; client reconnects via
  go-redis `FailoverClient`.
- HAProxy node kill → keepalived VRRP VIP flips to peer.
- Galexie / MinIO node failure → erasure-coding continues serving.
- API pod mid-stream kill → SSE client reconnects with cursor.
- Aggregator tick stall → cached values keep serving until TTL +
  the `aggregator-silent` alert fires.

## Running

Pre-flight: bring up the dev stack and run the API against it:

```sh
make dev                      # Postgres + Redis + MinIO containers
make db-migrate-up
go build -o bin/ratesengine-api ./cmd/ratesengine-api
./bin/ratesengine-api -config configs/dev.yaml &
```

Run the suite:

```sh
# All Wave 1 scenarios:
./test/chaos/run.sh

# A subset by prefix:
./test/chaos/run.sh 01 03

# Override target (default http://localhost:8080):
CHAOS_TARGET=http://staging.internal:8080 ./test/chaos/run.sh
```

## Production safety

Every scenario AND the runner refuse to execute when `CHAOS_TARGET`
matches `*production*`, `*api.ratesengine.net*`, or `*prod.*`. Chaos
in production is opt-in via the SEV playbook's quarterly drill, NOT
this suite.

## Output

Each run appends rows to `reports/chaos-run-<UTC-timestamp>.md`. The
file is created on first scenario start and reused by subsequent
scenarios in the same run. Per-row columns: scenario name, outcome
(✅/❌), duration, free-form notes.

`reports/` is gitignored (see `.gitignore`); the `.gitkeep` ensures
the directory exists for `run.sh` to write into.

## Tooling

Scenarios prefer **pumba** (Docker chaos sidecar) for network
manipulation when available — `pumba pause`, `pumba netem`, etc.
When pumba isn't installed, the harness falls back to
`docker network disconnect` + `docker stop`. Both paths exercise
the same go-client-side code branches in the Rates Engine.

Install pumba:

```sh
brew install pumba             # macOS
# or
go install github.com/alexei-led/pumba@latest
```

## Limitations (Wave 1)

- **Single-host**. The dev stack runs everything on one Docker host.
  Multi-host partition behaviour (e.g. cross-region split-brain)
  isn't exercised here.
- **No Patroni / Sentinel HA**. The dev stack is single-node
  Postgres + single-node Redis. Replica-promotion timing is a
  Wave 2 (staging) concern.
- **Behavioural assertions only**. We're checking "does the API
  return a sane response" — not measuring p95 latency under
  failure. Latency under chaos is the SLA-proof report's surface
  ([Task #77](../load/README.md)).

## See also

- [`docs/architecture/chaos-suite-design-note.md`](../../docs/architecture/chaos-suite-design-note.md) — Wave 1 / Wave 2 scoping + scenario matrix rationale.
- [`docs/operations/sev-playbook.md`](../../docs/operations/sev-playbook.md) §"Quarterly live chaos" — production drill cadence.
- [`docs/operations/runbooks/redis-master-down.md`](../../docs/operations/runbooks/redis-master-down.md), [`timescale-primary-down.md`](../../docs/operations/runbooks/timescale-primary-down.md) — runbooks Wave 2 will exercise.
- [Coverage matrix L5.5](../../docs/architecture/launch-readiness-backlog.md) — launch-readiness checklist row this satisfies.
