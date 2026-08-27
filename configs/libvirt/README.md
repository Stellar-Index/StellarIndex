# Testnet + Futurenet host provisioning (libvirt/KVM)

How to build the shared Testnet+Futurenet box **from a fresh Hetzner order**,
reproducibly. This covers the layer *below* ansible: the KVM host + the two
VMs. Once the VMs exist, the archival-node ansible role deploys the stack into
them (step 5). Companion docs:
[../../docs/operations/testnet-futurenet-deployment.md](../../docs/operations/testnet-futurenet-deployment.md).

## Topology
- **One Hetzner box** (Xeon E-2176G class: 6c/12t, 64 GB ECC, 2× ~960 GB NVMe),
  Debian 12 host running **libvirt/KVM** (not Proxmox — libvirt is lighter and
  keeps the host CLI/ansible-managed).
- **Two Ubuntu 24.04 VMs** — `si-testnet` (192.168.122.10) and `si-futurenet`
  (192.168.122.20) — one per network, on libvirt's default NAT network. Ubuntu
  24.04 matches r1 so the ansible role deploys unchanged.
- **RAID0** across both NVMe (data disposable → full capacity, no mirror);
  `/boot` is auto-mirrored by installimage.
- **Networking:** VMs have private IPs; the host reverse-proxies the public
  subdomains to them ([host-Caddyfile](./host-Caddyfile)).

## 1. Host OS (Debian 12 via installimage)
1. In Hetzner **Robot**, activate **Rescue** (linux, add your SSH key) and reboot.
2. SSH into the rescue system as root.
3. Copy [installimage-host.conf](./installimage-host.conf) to `/autosetup` on the
   rescue system (adjust the `IMAGE` line to the image the rescue ships under
   `/root/images/`).
4. Run `installimage` — it auto-detects `/autosetup` and installs unattended
   (RAID0 data + mirrored `/boot` + LVM `vg0`).
5. `reboot`. The box comes up as Debian 12 with your rescue SSH key installed.
   Clear the old host key locally: `ssh-keygen -R <host-ip>`.

## 2. Deploy key
installimage installs only the Hetzner-Robot-registered key. Add the CI/deploy
key (`si_deploy`) to `/root/.ssh/authorized_keys` on the host so ansible/CI can
reach it.

## 3. VMs (libvirt + cloud-init)
On the host:
```sh
# the pubkeys to authorize for root in both VMs, one per line:
printf '%s\n' "<id_ed25519.pub>" "<si_deploy.pub>" > /root/vm_authorized_keys
bash provision-vms.sh          # copy this repo's provision-vms.sh to the host
```
This installs libvirt/KVM, pulls the Ubuntu 24.04 cloud image, carves a 600 GB
LV per VM from `vg0`, and boots both VMs with static IPs + your keys.

## 4. Reach the VMs
The VMs sit behind the host's NAT, so reach them via ProxyJump:
```sh
ssh -i ~/.ssh/si_deploy -o ProxyCommand="ssh -i ~/.ssh/si_deploy -W %h:%p root@<host-ip>" root@192.168.122.10
```
The ansible inventory (`inventory/testnet.yml`) already encodes this jump in
`ansible_ssh_common_args`.

## 5. Deploy the stack (ansible)
```sh
cd configs/ansible
cp inventory/testnet.example.yml inventory/testnet.yml      # fill host IP + CIDRs
# generate inventory/testnet.secrets.yml (DB/MinIO/CH creds) — gitignored
ansible-playbook -i inventory/testnet.yml playbooks/archival-node.yml \
  -e secrets_file=../inventory/testnet.secrets.yml
```
The role installs everything from scratch — Postgres/Timescale, **Redis**,
**ClickHouse + schema** (`run_clickhouse: true`, `clickhouse_apply_schema: true`),
**stellar-core** (galexie's captive core), galexie, MinIO, cross-compiled
binaries, migrations, and starts galexie→indexer→api. (These installs were
hand-done on r1; they're codified in the role now — see 08-clickhouse.yml,
08-redis.yml, and the stellar-core block in 07-galexie.yml. `run_clickhouse`
defaults **off** so r1 is never touched.)

Repeat for futurenet with `inventory/futurenet.yml` (Phase 2 — galexie needs an
explicit futurenet captive config; see the inventory's TODOs).

## 6. Host reverse proxy + DNS
- Install Caddy on the host and use [host-Caddyfile](./host-Caddyfile) — it
  routes the 4 subdomains to the right VM's API (`flush_interval -1` for SSE).
- DNS: point all 4 subdomains at the host (A → host IPv4, AAAA → the host's
  `/64` ::2). Keep the `api.*` records **DNS-only** on Cloudflare (its proxy
  breaks SSE streams).

## What is NOT auto-provisioned (deliberate)
- Ordering the box + activating Rescue (Hetzner Robot — manual/API).
- Installing Caddy on the host (one-liner; config is shipped here).
- DNS records (registrar/Cloudflare).
