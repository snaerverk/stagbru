# Current state (private cluster, shared networking namespace)

This is a self-contained extraction of every fact the stagbru implementation
depends on, pulled from the private infra monorepo (`<INFRA_REPO>`,
`infra/kluster/<CLUSTER>/flux/networking/{<GATEWAY_POD>,<TAILSCALE_POD>}`,
`infra/kluster/<CLUSTER>/_wg.tf`, `infra/kluster/<CLUSTER>/flux/apps/headscale/tofu/policy`)
as of 2026-10-01. Treat this file as the source of truth for the plan in
`plan.md` — do not re-derive these values from memory, and re-verify against
the live manifests before cutover in case they've drifted.

> **This repo is public.** Everything below has had company-identifying
> values (public IP, internal hostnames/domain, admin emails, source org/repo
> name) replaced with `<PLACEHOLDER>` tokens. CIDRs, secret/field *names*,
> and other structural facts are kept because they're needed for a correct
> implementation and aren't identifying on their own. If you're filling this
> plan in against the real infra, substitute the real values locally — don't
> commit them back here.

## Placeholders used below, and what each thing's job actually is

Cluster/site/pod **names** are internal codenames too (even though they
don't look like secrets, they identify this company's infra layout as
directly as a hostname would), so they're replaced with role-based
placeholders throughout this file and `plan.md`. What each one actually
*does* matters more than its name — that's what the rest of this plan is
built on:

| Placeholder | What it is | Its job |
|---|---|---|
| `<CLUSTER>` | The Kubernetes cluster stagbru runs on. | Hosts every pod this plan touches; has no public IP of its own on its internal node network. |
| `<NAMESPACE>` | The one shared `wg-eyev`-style namespace. | Holds everything in this plan: both current pods, both their Secrets, and (post-cutover) the single stagbru pod. Pod Security is `privileged` here, nowhere else. |
| `<MESH>` | The site-to-site WireGuard mesh. | Encrypts traffic between `<CLUSTER>` and other remote sites' nodes directly, node-to-node, over the internet/DMZ. `<CLUSTER>`'s own nodes have no public IPs, so they're only reachable *through* this mesh. |
| `<GATEWAY_POD>` | The current WireGuard-gateway Deployment/Service. | **The public entry point.** It's the one thing on `<CLUSTER>` that remote mesh peers actually dial into (`LoadBalancer`, UDP 51820) — it terminates the tunnel and forwards traffic on to `<CLUSTER>`'s internal networks. Every other node on `<CLUSTER>` is only mesh-routable, not internet-routable. |
| `<TAILSCALE_POD>` | The current Tailscale subnet-router Deployment. | Joins a separate overlay network (a managed Tailscale/headscale tailnet) as a **spoke**, and advertises routes to `<CLUSTER>`'s own networks *and* to two remote sites' subnets that are only reachable through `<GATEWAY_POD>`'s mesh tunnel — so tailnet devices (laptops, phones) can reach those networks too, not just `<CLUSTER>`'s own. |
| `<REMOTE_SITE_A>`, `<REMOTE_SITE_B>` | Two other clusters that are `<MESH>` peers of `<CLUSTER>`. | Each runs its own nodes that dial into `<GATEWAY_POD>`; their node subnets are among the routes `<TAILSCALE_POD>` advertises. |
| `site-c`, `site-d` (ACL tags) | Two more networks reachable only via `<GATEWAY_POD>`'s tunnel. | Same role as the two remote sites above — included in `TS_ROUTES` and auto-approved by the same ACL mechanism. |
| `<DMZ_NET>` / `<DMZ_SITE>` | The DMZ network `<CLUSTER>` itself lives in, and that network's name as used for endpoint/ACL-tag preference. | Preferred low-latency path for mesh peers that are reachable over it; also the network whose edge WAF exposes `<GATEWAY_POD>`'s public IP. |
| `<VAULT_STORE>` | The internal Vault instance that stores `<MESH>` node public keys and the headscale admin API key. | Source of truth for the peer list `<GATEWAY_POD>` reads today, and for the credential `<TAILSCALE_POD>`'s join-key Terraform authenticates with. |
| `<WG_NODE_NAME>` | This gateway's node name *within the WireGuard mesh* (distinct from its tailnet hostname below). | Identifies it as a peer to every other `<MESH>` node; used to exclude "self" when building the peer list. |
| `<TS_HOSTNAME>` | This pod's hostname *within the tailnet* (distinct from `<WG_NODE_NAME>` above — two different identity systems, two different names). | Must stay byte-for-byte identical across the cutover, or the tailnet treats stagbru as a brand-new node (new IP, routes need re-approval, everyone's `--accept-routes`/bookmarks break). |

### Why this collapses to one pod

`<GATEWAY_POD>` and `<TAILSCALE_POD>` are two halves of one egress path, but
they run as **separate pods today only because of a routing workaround**:
`<TAILSCALE_POD>` needs to send traffic for `<REMOTE_SITE_A>`/`<REMOTE_SITE_B>`/`site-c`/`site-d`
through `<GATEWAY_POD>`'s `wg0`, but a pod can't reach into another pod's
network namespace — so today that detour is built out of a headless Service
(to resolve a real, routable pod IP instead of a virtual ClusterIP), an
`ip route ... onlink` hack (needed because the kernel won't accept that pod
IP as a route next-hop without one), and a **required** pod affinity pinning
both pods to the same node (because `onlink` only ARP-resolves over the same
node's L2 bridge — confirmed live: the identical route broke the moment a
redeploy landed the two pods on different nodes).

None of that is solving a real requirement — it's entirely working around
"these are two different network namespaces." Putting both WireGuard and
Tailscale in **one pod** means `wg0` and `tailscale0` share one namespace,
so the kernel's own routing table already does the job: traffic headed for
any of those remote subnets just routes straight into `wg0`, no next-hop
resolution, no `onlink`, no affinity, no headless Service, no risk of the
two landing on different nodes after a redeploy. It also removes an entire
class of previously-silent failure (the affinity drifting or a Service DNS
lookup racing pod startup) and collapses the two pods' NAT/forwarding rule
sets into one nftables table that's always consistent with itself, instead
of two `iptables`/firewall configs that each have to assume the other
exists.

## Namespace

`<NAMESPACE>`, created by `infra/kluster/<CLUSTER>/_wg.tf`'s
`kubernetes_namespace_v1.<MESH>`, Pod Security labels `enforce/audit/warn:
privileged` on all three.

GitOps is **Flux** (`kustomization.yaml` per app dir, `namespace: <NAMESPACE>` set
at the kustomize level) with `tofu-controller`'s `Terraform`
(`infra.contrib.fluxcd.io/v1alpha2`) CRD running in-cluster OpenTofu applies.
No ArgoCD anywhere in this cluster.

## WireGuard identity (do not regenerate)

- Keypair: `wireguard_asymmetric_key.gateway` in
  `infra/kluster/<CLUSTER>/_wg.tf`. **Never recreate this resource** — the
  private key is referenced only via the `kubernetes_secret_v1` resource
  backing `<GATEWAY_POD>-self`.
- Node: `name = "<WG_NODE_NAME>"`, `ip = "100.192.8.5"`,
  `addresses = ["100.192.8.0/24", "172.16.6.0/32"]`.
- Endpoints published under this node: `global` → `<GATEWAY_PUBLIC_IP>:51820`,
  `<DMZ_NET>` → `172.16.6.168:51820`.
- `ListenPort = 51820`.
- Secret `<GATEWAY_POD>-self` (created directly by `_wg.tf`, namespace
  `<NAMESPACE>`) holds two keys consumed via `envFrom`: `PRIVATE_KEY`, `IP`
  (`100.192.8.5`, no CIDR suffix — the `/10` was added by the old
  `wg-quick` template, not stored in the Secret itself).
- The public half is exposed by <CLUSTER>'s `wireguard_nodes` output for
  `infra/vault/<VAULT_STORE>` to publish to `network/<MESH>/nodes/<WG_NODE_NAME>`
  in Vault (`<VAULT_URL>`, KV v2 mount `infra`). stagbru never touches this
  publishing path.

## Peers (today: ExternalSecret + wg-quick template)

`ClusterSecretStore` `<VAULT_STORE>-infra` (dedicated, not the shared `<VAULT_STORE>`
store — needs `provider.vault.path: infra` set for `dataFrom.find` to work).

`ExternalSecret` `wg0-conf`, `refreshInterval: 5m`:
- `dataFrom.find` lists every key under `network/<MESH>/nodes/` (Vault LIST),
  returning one JSON blob per node as a string value, keyed
  `network_<MESH>_nodes__<name>`.
- Template excludes any key whose name has suffix `<WG_NODE_NAME>` (self), and
  any entry with no `public_key` (defensive against stale/malformed test
  entries that can't be deleted — Vault policy only grants create/update/read
  on `infra/*`).
- For each remaining peer: `PersistentKeepalive = 25`. `Endpoint` picked by
  preferring network `<DMZ_NET>`, falling back to `global`; peers with
  neither get no `Endpoint` (can still dial in). `AllowedIPs` = the peer's
  `addresses`, **with the chosen endpoint host's own `/32` filtered out** —
  critical detail: leaving a peer's transport endpoint inside its own
  `AllowedIPs` creates a self-referencing routing loop (outbound
  handshake/keepalive UDP gets routed back into `wg0`), which manifests as
  "sent" bytes climbing on `wg show` while "received" stays 0 forever.
- Target Secret key: `peers.conf` (wg-quick fragment text), **not** JSON.
  stagbru's peer informer needs the `wg0-conf` ExternalSecret's template
  changed to emit a `peers.json` key instead (see `plan.md` manifest changes)
  — this is new work, not something that already exists.

Sync today: a `wg-reload` sidecar container polls the mounted Secret every
15s (sha256 of the rendered conf), and on change runs
`wg syncconf wg0 <(wg-quick strip wg0.conf)`. This is exactly the behaviour
stagbru's peer reconciler must replace (informer-driven, not polling, but
same no-op/no-disruption guarantee).

## Current <GATEWAY_POD> Deployment

- `initContainers`:
  - `enable-ip-forward` (privileged busybox): `sysctl -w net.ipv4.ip_forward=1`.
  - `render-wg0-conf` (busybox, `envFrom: <GATEWAY_POD>-self`): concatenates
    the `[Interface]` block with `peers.conf`, writes
    `/config/wg_confs/wg0.conf`, `chmod 600`.
- `wireguard` container: `lscr.io/linuxserver/wireguard:latest`, `NET_ADMIN`,
  `containerPort: 51820/udp`. Its `PostUp`/`PostDown` (today, **iptables** —
  to be replaced by stagbru's own `table inet stagbru`):
  ```
  PostUp   = iptables -A FORWARD -i %i -j ACCEPT; iptables -A FORWARD -o %i -j ACCEPT; iptables -t nat -A POSTROUTING -o %i -j MASQUERADE
  PostDown = iptables -D FORWARD -i %i -j ACCEPT; iptables -D FORWARD -o %i -j ACCEPT; iptables -t nat -D POSTROUTING -o %i -j MASQUERADE
  ```
  MASQUERADE is required because remote <MESH> peers (<REMOTE_SITE_A>, <REMOTE_SITE_B>)
  only accept packets whose *source* IP is one of this gateway's own
  published `AllowedIPs` (WireGuard cryptokey routing silently drops
  anything else); traffic forwarded on behalf of tailscale's pod network
  source IP would otherwise be dropped on arrival at the remote peer.
- `wg-reload` sidecar: see above.

### Service — ⚠️ plan/spec correction needed

The original spec's "hard constraints" says `externalTrafficPolicy: Cluster`.
**The live manifest actually uses `externalTrafficPolicy: Local`**, with a
detailed, confirmed-live comment explaining why: `Cluster` was corrupting the
source IP of return WireGuard traffic via a cross-node flannel VXLAN hop
before it reached `wg0` (`wg showconf` peers were observed roaming to
`10.244.x.x` flannel addresses instead of the real `172.16.6.x` one, right
after a genuine handshake). `Local` is only safe because there is exactly one
replica (`Recreate` strategy) — this still holds for stagbru's single pod.

**stagbru's Service must keep `externalTrafficPolicy: Local`, not `Cluster`.**
Copy the Service verbatim except the selector. This is the one place the
original spec's text and the real manifest disagree; the live manifest wins.

Other Service facts (these do match the spec): `LoadBalancer`, UDP `51820`,
`metallb.io/loadBalancerIPs: 172.16.6.168`, `metallb.io/allow-shared-ip:
dmz-public` (same shared DMZ IP as the apiserver and Traefik — the DMZ's edge
WAF already DNATs `<GATEWAY_PUBLIC_IP>` → `172.16.6.168` wholesale, no extra
per-port NAT needed).

### Headless Service `<GATEWAY_POD>-pod`

`clusterIP: None`, exists purely so `<TAILSCALE_POD>`'s `start.sh` (and the
standalone test Job below) can resolve a real routable pod IP as an
`ip route ... onlink` next-hop — a ClusterIP can't be used as a route
next-hop. **Disappears entirely in stagbru**: wg0 and tailscale0 live in the
same netns, so there's no cross-pod next-hop to resolve.

## Current <TAILSCALE_POD> Deployment

- `tailscale/tailscale:latest` — **not version-pinned today**. There is no
  existing pinned version to inherit; Phase 3 must pick one explicitly (see
  `plan.md` open questions).
- Does **not** run `containerboot` directly: `command: ["/scripts/start.sh"]`
  (ConfigMap `<TAILSCALE_POD>-start`), which sets up `onlink` routes to the
  gateway pod, builds `TS_EXTRA_ARGS`, then `exec`s `containerboot`.
- Env (`_deploy.yaml`):
  - `TS_AUTHKEY` ← Secret `tailscale-authkey` key `TS_AUTHKEY`.
  - `TS_STATE_DIR=/var/lib/tailscale`.
  - `TS_HOSTNAME=<TS_HOSTNAME>` — **exact value to preserve**. (Not
    `<WG_NODE_NAME>` — that's the *WireGuard* node name; the tailnet hostname
    is different. Getting this wrong re-registers a new tailnet node.)
  - `TS_KUBE_SECRET=""` — disables containerboot's redundant attempt to also
    persist state via a Kubernetes Secret (would need extra RBAC); state is
    already persisted on the PVC. Not directly applicable once stagbru
    supervises `tailscaled` itself rather than running `containerboot`, but
    the reasoning (don't let tailscaled touch a k8s Secret for state) still
    applies — make sure nothing re-introduces that RBAC need.
  - `TS_DEBUG_FIREWALL_MODE=nftables` — Talos has no legacy iptables kernel
    modules; tailscale's default firewall mode fails with `can't initialize
    iptables table 'filter': Table does not exist`, which *silently* skips
    the forwarding/filtering rules subnet routing depends on (tailscale
    itself still connects fine, only forwarding breaks). stagbru must set
    this (or the tailscaled equivalent flag) for its embedded/supervised
    tailscaled.
  - `TS_USERSPACE=false` + `NET_ADMIN`/`NET_RAW` + `/dev/net/tun` hostPath —
    kernel TUN mode, required for real subnet-router packet forwarding.
  - `ROUTES=100.244.64.0/18,100.244.128.0/18,10.44.64.0/20,10.44.128.0/20`
    — **four subnets, not two**. This is wider than naive reading of the
    READMEs suggests: besides <REMOTE_SITE_B>'s and <REMOTE_SITE_A>'s <MESH> node
    subnets, it also includes `10.44.64.0/20` (`site-c`) and
    `10.44.128.0/20` (`site-d`) — both only reachable through
    `<GATEWAY_POD>`'s wg0, same as the other two.
  - `GATEWAY_HOST=<GATEWAY_POD>-pod`, `TS_LOGIN_SERVER=<HEADSCALE_URL>`.
- `start.sh`'s own hardcoded `LOCAL_ROUTES=172.16.6.0/24,100.192.8.0/24`
  (this cluster's own DMZ + mesh node subnet — reachable directly, no gateway
  detour) is unconditionally appended to `ROUTES` to form
  `--advertise-routes`.

  **Full current `--advertise-routes` value, and therefore stagbru's
  `TS_ROUTES` default, is the union of all of the above:**
  ```
  172.16.6.0/24,100.192.8.0/24,100.244.64.0/18,100.244.128.0/18,10.44.64.0/20,10.44.128.0/20
  ```
  This supersedes the shorter 4-CIDR list in the original spec's config
  table.
- `--reset` is passed on every `tailscale up` (state persists across
  restarts on the PVC, but `tailscale up` otherwise requires re-stating every
  non-default flag or it fails outright — confirmed live, crash-loops
  without it). `--reset` only resets *settings*, never the persisted
  node identity/registration.
- Deliberately **no** `--accept-routes` (this pod is itself the subnet
  router for these CIDRs; accepting another peer's route for the same
  space hijacked traffic in testing) and **no** `--advertise-exit-node`.
- Required pod affinity onto `<GATEWAY_POD>`'s node
  (`kubernetes.io/hostname` topology) — load-bearing today because the
  `onlink` route only ARP-resolves on the same node's `cni0` L2 domain.
  **Disappears entirely in stagbru** (no cross-pod next-hop at all).
- PVC `tailscale-state`: `longhorn`, `ReadWriteOnce`, `1Gi`. **Reuse this
  exact PVC** — it's what lets a restart keep the same tailnet node identity
  instead of re-registering.

## Tailscale auth / ACL (headscale at `<HEADSCALE_URL>`)

- In-cluster Terraform (`tofu-controller`, `destroyResourcesOnDeletion:
  false` — never flip this) creates a **reusable** pre-auth key
  (`headscale_pre_auth_key.this`), tied to the existing `system` user
  (imported resource id `"2"`, not created fresh).
- `acl_tags = ["tag:<DMZ_SITE>", "tag:<NAMESPACE>", "tag:site-c", "tag:site-d"]` —
  **four tags**, not the two the old README's "Why routes don't need manual
  approval" section mentions. All four matter: they're what make all of
  `TS_ROUTES` above auto-approve.
- Key written to Secret `tailscale-authkey`, key `TS_AUTHKEY`
  (`writeOutputsToSecret`).
- Auth to headscale's admin API uses Secret `headscale-api-key` (separate
  `ExternalSecret` copy of the same Vault path `secret/headscale/<INSTANCE>`,
  property `apiKey`, via the *shared* `<VAULT_STORE>` ClusterSecretStore — not
  `<VAULT_STORE>-infra`).
- Headscale ACL (`flux/apps/headscale/tofu/policy/main.tf`):
  - `tagOwners`: `tag:exit`, `tag:<DMZ_SITE>`, `tag:<NAMESPACE>`, `tag:office`,
    `tag:site-c`, `tag:site-d`, all owned by `group:admin`.
  - `autoApprovers.routes`:
    `172.16.6.0/24` → `tag:<DMZ_SITE>`,
    **`100.192.0.0/10`** → `tag:<NAMESPACE>` (note: `/10`, a much bigger supernet
    than `/16` or `/24` — covers `100.192.8.0/24` with huge headroom),
    `10.0.128.0/24` → `tag:office`,
    `10.44.128.0/20` → `tag:site-d`,
    `10.44.64.0/20` → `tag:site-c`.
  - One ACL rule: `accept *:* from group:admin, tag:exit, tag:office,
    tag:<DMZ_SITE>, tag:<NAMESPACE>, tag:site-c, tag:site-d`.
  - `group:admin` includes several `<ADMIN_EMAIL>` entries (real addresses
    redacted — not needed for the implementation, only confirms the ACL
    grants admin-group access).
- None of this ACL/tofu needs to change for the stagbru migration — only the
  pod consuming `tailscale-authkey` and advertising routes changes.

## RBAC / supporting objects

- `<TAILSCALE_POD>/_rbac.yaml`: per-namespace `ServiceAccount`/`RoleBinding`
  `tf-runner` → ClusterRole `tf-runner-role`, required by `tofu-controller`
  for any `Terraform` object in this namespace. Keep as-is; it's unrelated to
  the pod architecture change.
- `<TAILSCALE_POD>/_tofu.yaml`: Flux `Terraform` object, `path:
  ./infra/kluster/<CLUSTER>/flux/networking/<TAILSCALE_POD>/tofu`, `interval:
  30m`, `approvePlan: auto`. Default plan: leave this tofu in place
  (confirmed: moving/renaming the Terraform object counts as a state change
  and needs explicit sign-off).

## Things referencing the pieces slated for deletion

Besides `start.sh`'s own use of `<GATEWAY_POD>-pod` / the pod affinity,
one more manifest depends on the current two-pod architecture:

- `<GATEWAY_POD>/tests/connectivity-test.yaml` — a standalone Job (not
  Flux-managed; a human/agent `kubectl apply`s it by hand before pushing a
  change). It requires `requiredDuringSchedulingIgnoredDuringExecution` pod
  affinity onto `<GATEWAY_POD>`, resolves
  `<GATEWAY_POD>-pod.<NAMESPACE>.svc.cluster.local`, and adds the same
  `onlink` routes as `<TAILSCALE_POD>/start.sh`, then pings known <MESH>
  peers. **This test is architecture-specific and must be rewritten (or
  dropped) alongside the cutover** — it was missing from the original spec's
  list of things to check/delete. A stagbru-era equivalent would just exec
  into the single pod and `ping`/`wg show` directly; no onlink routes or
  affinity needed.

## Observability

No `ServiceMonitor` or `PodMonitor` exists anywhere in this infra repo today
(`grep -r` came back empty) — there is no Prometheus Operator wired up in
this cluster to discover one. Skip shipping one in Phase 4; expose
`/metrics` and let it be scraped however this cluster already does it (or
not at all, until asked for).

## CI / registry

No GitHub Actions workflows, no Harbor/ghcr references anywhere in
`<INFRA_REPO>`. There's no existing pinned registry/image path to
inherit — Phase 3's registry choice is a genuinely open decision, not a
"read it from the current Deployment" one. See `plan.md`'s open questions.

## This repository (`github.com/snaerverk/stagbru`) today

- `go.mod`: `module github.com/snaerverk/stagbru`, `go 1.27.1`.
- `cmd/main.go`: empty placeholder (`package main; func main() {}`).
- No other Go code, no CI, no README yet.
