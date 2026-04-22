# withObsrvr — flowctl

**Status:** ⚠️ Reference only. Good patterns; wrong scope for us.

**Repo:** <https://github.com/withObsrvr/flowctl>
**Verified against:** `README.md`, `CLAUDE.md`, `cmd/` and `internal/`
directory listings at clone time (2026-04-22). I did not deep-read the
Go source — the architecture is clear enough from the layout and
CLAUDE.md.

## What flowctl is

A **pipeline orchestrator**. Composes external components (sources,
processors, sinks — each running as its own process or container) into
a pipeline, validated with CUE schemas, coordinated via an embedded or
standalone gRPC control plane, with a TUI dashboard and health
monitoring.

Top-level layout:

```
cmd/            dashboard, init, list, pipelines, processors,
                run, server, status, translate, validate, version
internal/       api, components, config, controlplane, core,
                drivers, generator, orchestrator, runner, sandbox,
                source, sink, translator, ...
proto/          gRPC proto definitions
schemas/cue/    CUE schema for pipeline YAML validation
```

## Key design choices (from `CLAUDE.md`, verbatim where quoted)

- **Kubernetes-style YAML** — `apiVersion`, `kind`, `metadata`, `spec`.
- **CUE schema validation** during config load and translate.
- **Components run as separate containers/processes**, not in-process.
- **gRPC control plane** for component registration + heartbeats.
- **Multi-orchestrator translation**: Docker Compose today,
  Kubernetes/Nomad planned. `flowctl translate -f p.yaml -o
  docker-compose` emits a deployable artifact.
- **Sandbox command** (`flowctl sandbox start`) — local development
  environment.

## Why it's overkill for us

1. We are **one service** (CTX Rates API). flowctl is designed for
   operators who run many heterogeneous pipelines side-by-side and want
   per-pipeline lifecycle management. That's not our shape.
2. Out-of-process components mean inter-process gRPC on every event —
   wasteful latency at the volumes Stellar produces (pubnet currently
   closes a ledger every ~5 s with hundreds of operations).
3. Our control plane needs are small (one pipeline, one lifecycle, one
   health endpoint). We don't need a full gRPC server just for that.
4. We want **one deployable unit** for the ingest + aggregation +
   serving stack so our co-lo operators can reason about it as a
   single process. Adding flowctl adds a second operational surface.

## Useful patterns to borrow (but not the code)

- **CUE validation** for pipeline configs — nice if we expose a
  customer-tweakable config; probably not needed for our internal
  config.
- **Component heartbeat + registration** — if we ever horizontally
  split our ingest workers, something similar inside our own
  control plane is sensible.
- **Translator pattern** (`flowctl translate -o docker-compose`) — if
  we ever ship a self-hosted deployment kit, we'd want a similar
  "generate deployment artifact from config" story.
- **TUI dashboard** (`flowctl dashboard`) — attractive pattern for
  operator introspection. Post-MVP.

## Verdict

- Do not adopt as a runtime dependency.
- Do not build our pipeline inside flowctl.
- Re-read `internal/core/` if we ever have to split our pipeline across
  machines — there are likely useful bits around gRPC topology and
  backpressure. For Phase 1–5, we use a single-process in-memory
  channel topology.

## Open items

- [ ] Re-audit if we ever want to ship a public "self-hosted CTX
      Rates" kit. flowctl's translate-to-compose is then directly
      useful.
- [ ] Check whether the Stellar-specific processors it knows about
      (from `registry.yaml`) include Reflector, Soroswap, Blend
      decoders — if yes, those are individual withObsrvr processor
      repos we should audit next.

## References

- Repo: <https://github.com/withObsrvr/flowctl>
- Related: [withobsrvr-nebu.md](withobsrvr-nebu.md) — nebu's processor
  contract is what flowctl orchestrates against.
