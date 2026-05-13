# Ansible role — `haproxy`

Deploy the api-tier load balancer fronting `ratesengine-api`.
Implements the topology pinned in
[`docs/architecture/ha-plan.md §3.1`](../../../../docs/architecture/ha-plan.md):

- 2 HAProxy hosts (`lb-01` / `lb-02`) sharing a VIP via keepalived
  VRRP.
- HAProxy backends are the 3 `ratesengine-api` pods, health-checked
  via `/v1/readyz`.
- TLS termination at the edge; HSTS header on every response.
- Built-in Prometheus exporter on the loopback stats endpoint.

Pairs with the `patroni` and `redis-sentinel` roles to complete
the launch-critical HA topology. Design rationale lives in
[`docs/architecture/haproxy-ansible-role-design-note.md`](../../../../docs/architecture/haproxy-ansible-role-design-note.md).

## Prerequisites

- Two LB hosts named per inventory (`lb-01` / `lb-02` by default).
  Each needs:
  - Ubuntu 24.04 LTS (or 22.04).
  - Network reachability to the `ratesengine_api` host group on
    the api port (default 3000).
  - VRRP traffic (multicast `224.0.0.18` on private VLAN, OR
    unicast peering — see "Cloud VRRP gotchas" below).
  - A combined cert+key file dropped at
    `/etc/haproxy/certs/api.pem` (cert-bot integration is a
    separate concern; this role just expects the file to exist).

- Vault contents:
  - `haproxy_vip` — the floating VIP (e.g. `10.0.0.30`).
  - `keepalived_vrrp_password` — VRRP auth password.
    **Note**: keepalived silently truncates `auth_pass` beyond
    8 bytes; preflight warns if longer.

## Inventory model

Set in your `inventory/<region>.yml`:

```yaml
all:
  children:
    haproxy_lb:
      hosts:
        lb-01: { ansible_host: 10.0.0.31, keepalived_priority: 100, keepalived_initial_state: MASTER }
        lb-02: { ansible_host: 10.0.0.32, keepalived_priority: 99,  keepalived_initial_state: BACKUP }
      vars:
        haproxy_vip: 10.0.0.30
        keepalived_iface: eth0
        keepalived_vip_prefix_length: 24
        keepalived_vrid: 51
    ratesengine_api:
      hosts:
        api-01: { ansible_host: 10.0.0.41 }
        api-02: { ansible_host: 10.0.0.42 }
        api-03: { ansible_host: 10.0.0.43 }
      vars:
        ratesengine_api_port: 3000
```

Per-host `keepalived_priority` + `keepalived_initial_state` only
affect the **initial** election. Once running, keepalived
re-elects on health-check failure regardless of original priority.

## Running

F-1266 (2026-05-13): `playbooks/haproxy.yml` is on the L4
cutover backlog and hasn't landed yet; the role is applied
ad-hoc via tag-filtered plays today. When the playbook lands,
restore the original commands below.

```sh
cd configs/ansible
# Standard bring-up — playbook TBD:
# ansible-playbook -i inventory/r1.yml playbooks/haproxy.yml --tags haproxy

# Re-render config without restarting (cert rotation, backend
# pool change) — playbook TBD:
# ansible-playbook -i inventory/r1.yml playbooks/haproxy.yml --tags haproxy,config
```

The `03-haproxy-configure` task validates the rendered config via
`haproxy -c -f` before reload — a malformed template never lands
in production.

## Health-check semantics

- **Path**: `/v1/readyz` (the deep ready probe — passes only
  when Timescale + Redis are both reachable).
- **Cadence**: 5s interval, 3 fails before drain, 2 successes
  before re-add → 15s detection latency.
- **Slowstart**: 10s ramp prevents cold pods from getting
  hammered after recovery.

A pod that's *running* but can't read from Timescale shouldn't
receive traffic — `/v1/healthz` would mark it healthy
(process-alive); `/v1/readyz` correctly marks it unready.

## Failover scenarios

| Scenario | Detection | RTO |
|---|---|---|
| 1 api pod dies | HAProxy after 15s (3 × 5s `inter`) | 0 (others serve) |
| 1 HAProxy host dies | keepalived VRRP within 1-3s | ≤3s |
| HAProxy process dies but host alive | `chk_haproxy` script after 2-4s | 1-4s |

## Stats + monitoring

- **Stats endpoint**: `http://127.0.0.1:8404/` — loopback-only.
  SSH-tunnel for remote access; never expose 8404 publicly.
- **Prometheus metrics**: `http://127.0.0.1:8404/metrics` —
  HAProxy 2.4+'s built-in exporter.
- **Keepalived state**: per-host textfile metric
  `ratesengine_haproxy_vip_owner{instance=...}` — sums to 1
  across hosts in steady state. Emitted every 30s by the role's
  textfile scraper.

## Cloud VRRP gotchas

- **Hetzner**: VRRP multicast works on private VLANs (`vSwitch`).
  Default config in this role assumes that.
- **AWS**: VRRP multicast is **blocked** on EC2 by default. Use
  unicast VRRP — set `unicast_peer { <peer-ip> }` per peer
  manually in `keepalived.conf` (or extend this role with a
  `unicast_peers` list var).
- **VIP-as-secondary-IP routing**: Some clouds require the VIP
  be registered as a secondary IP on a NIC; gratuitous-ARP
  notification on failover may not propagate without that.

## TLS cert rotation

This role does **not** manage TLS certs. Drop a combined
`fullchain+privkey` PEM file at
`/etc/haproxy/certs/api.pem` (override path via
`haproxy_tls_cert_dir` / `haproxy_tls_cert_filename`).

A `systemctl reload haproxy` picks up new certs without restart.
A separate cert-bot role can be added to automate this; out of
scope here.

## Rolling password rotation

`keepalived_vrrp_password` is set from vault — rotating it is a
2-host roll:

1. Update the password in `inventory/<region>.secrets.yml`.
2. Re-apply this role to ONE LB host first (`--limit lb-02`).
3. Run `ip a | grep <vip>` on lb-01 to confirm it still owns
   the VIP (i.e. the rotation hasn't disrupted membership).
4. Re-apply to lb-01.
5. `ip a | grep <vip>` on both — should sum to 1 owner.

Until both hosts have the new password, VRRP authentication will
fail between them and they may briefly both think they're MASTER
(harmless — both bind the VIP, upstream receives both copies of
gratuitous ARP, settles within seconds).

## Companion runbook

[`docs/operations/runbooks/api-down.md`](../../../../docs/operations/runbooks/api-down.md)
covers the SEV-1 "every API pod is down" case. The HAProxy-
specific single-pod-eject scenario (HAProxy's 15-second health-
check window flips one backend OUT but the other backends keep
serving) is partial-degradation rather than a SEV-1 — the
mitigation is "wait for HAProxy's 15s health-check window, then
investigate the pod itself." Add a §"single-pod-ejected" section
to `api-down.md` (or a sibling `api-partial-degradation.md`
runbook) when the multi-pod HAProxy topology actually lands; the
earlier text named a nonexistent `api-pod-down.md` (F-1274,
2026-05-13).
