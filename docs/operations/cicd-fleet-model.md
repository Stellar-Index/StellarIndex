# CI/CD fleet model (design proposal)

Status: **proposal** — needs its own plan→audit before implementation. The
immediate testnet/futurenet capability is already shipped as a minimal
change (deploy.yml now offers `testnet` / `futurenet` targets); this doc is
the holistic redesign for when the fleet grows past a couple of hosts.

## Problem

`deploy.yml` deploys **one region per manual `workflow_dispatch` run**
(region choice → `{REGION}_HOST` secret → inventory). That was right for a
single r1. The fleet is now growing on two axes:

- **Regions (Mainnet HA):** r1 + future r2/r3.
- **Networks:** Mainnet + Testnet + Futurenet.

At full spread that is ~5 targets, so a release becomes 5 manual dispatches
— toil that scales with the fleet and invites "forgot to deploy r3" drift.
And the two axes are **not symmetric**: Mainnet is money (gated, staged,
HA); test nets are low-stakes pre-prod canaries.

## Model

### 1. Fleet manifest — `deploy/fleet.yml`

One declarative list; every target is a row:

```yaml
targets:
  - name: r1
    network: mainnet
    tier: prod
    inventory: r1
    binaries: [indexer, aggregator, api, sla-probe]
    auto_deploy: false          # gated + staged
  - name: testnet
    network: testnet
    tier: test
    inventory: testnet
    binaries: [indexer, api, ops]   # NO aggregator
    auto_deploy: true           # continuous, on merge to main
    core_version: v27.0.0       # per-network stack (protocol pipeline)
  - name: futurenet
    network: futurenet
    tier: test
    inventory: futurenet
    binaries: [indexer, api, ops]
    auto_deploy: true
    core_version: <bleeding-edge>   # ahead of mainnet
```

### 2. Tiered CD

- **Test tier (Testnet + Futurenet) = CONTINUOUS deploy on merge to main.**
  A live pre-prod canary: Futurenet catches upcoming-protocol breakage,
  Testnet catches functional regressions — *before* a Mainnet release. More
  targets = more safety, not more toil. Low-stakes ⇒ auto-deploy is fine
  (no approval gate; the current minimal change already gives test-net
  targets no `environment` protection).
- **Mainnet tier = GATED, STAGED.** tag → canary region (r2/r3) → verify →
  promote to the rest. Keep the approval gate + config-apply gate +
  migrations. Unchanged from today.

### 3. Protocol-upgrade pipeline (per-network stack version)

Stellar rolls out protocol/core **Futurenet → Testnet → Mainnet**, so
`core_version` / `galexie_version` / `go-stellar-sdk` version are
**per-target**, not global. Futurenet runs bleeding-edge on the unreleased
protocol; running our indexer there surfaces "new op/event type we don't
decode yet" months before Mainnet. The fleet manifest carries the per-target
versions; the test tier deliberately runs ahead-of-Mainnet software.

### 4. Mechanics (stay Ansible — no K8s/ArgoCD re-platform)

- `deploy.yml` gains a manifest-driven **matrix**: "version X → target-set Y";
  each matrix entry runs the existing per-target ansible unchanged.
- Push-to-main drives the `tier: test` targets (auto_deploy).
- A `tag` drives the staged Mainnet rollout.
- **Fleet visibility:** a "what version is where" report that polls each
  target's `/v1/version`.

### 5. Sequencing

Build Testnet + Futurenet as the **first continuous-deploy targets**. That
validates the tiered-CD model, gives the canary immediately, and de-risks
the later Mainnet multi-region (r2/r3) rollout — which reuses the same
manifest + matrix.

## Not doing (and why)

- **K8s / ArgoCD / a GitOps re-platform** — the stack is bare-metal Ansible;
  the win here is orchestration, not a container migration.
- **Auto-deploy to Mainnet** — money network stays human-gated and staged.

## Related

- `.github/workflows/deploy.yml` (today: per-target manual; testnet/futurenet
  targets added as the minimal first step).
- [testnet-futurenet-deployment.md](./testnet-futurenet-deployment.md).
