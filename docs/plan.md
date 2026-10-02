# stagbru — implementation plan

> Codename in earlier discussion was "bifrost"; the project is **stagbru**
> (this repo). Binary name, nftables table name, env var prefix, and all
> identifiers below use `stagbru`, not `bifrost`.

## What stagbru does

stagbru is a single Go binary, running as one pod, that does two things:

1. **Runs one or more WireGuard nodes inside the Kubernetes cluster.**
2. **Runs a managed Tailscale client that bridges those WireGuard networks
   onto a tailnet** — joining it as a subnet router and advertising routes
   to the networks reachable over WireGuard, so tailnet devices can reach
   them without being WireGuard peers themselves.

Both run in the same pod, sharing one network namespace, which is what lets
the second capability work at all (see "How" below).

> This repo is public; `<CLUSTER>`, `<NAMESPACE>`, `<MESH>` and the other
> angle-bracket tokens throughout this file are internal codenames replaced
> with role-based placeholders. [`current-state.md`](./current-state.md)'s
> placeholder table explains what each one actually is, and also records
> the concrete facts of the one real WireGuard network and tailnet
> connection this plan is implemented against today.

### 1. A WireGuard node, running in the cluster

**What**: for each configured network, stagbru brings up a real kernel
WireGuard interface with its own identity (private key, address, listen
port) and keeps its peer list — the other nodes it should be able to reach,
and what's routable through each — continuously in sync with a desired
state read from Kubernetes.

**How**: the desired peer list comes from a Kubernetes Secret (JSON,
watched via an informer — see `internal/peers`), not a static config file,
because the mesh's membership changes independently of stagbru's own
lifecycle (nodes join, leave, get new endpoints) and the running interface
has to pick that up without a restart. `internal/wg` diffs that desired
state against the interface's actual state (via `wgctrl`) and applies only
the delta — added peers get added, changed peers get updated, removed peers
get removed, and anything unchanged is left alone so its live handshake
state survives. For every peer's allowed address range, a route is
installed pointing at that interface, so the kernel's own routing table —
not stagbru itself — does the work of getting a packet to the right peer.

**Why**: WireGuard's own protocol (cryptokey routing) only tells the kernel
*which peer* a given destination belongs to; it doesn't manage interface
state, keep peers in sync with an external source of truth, or install
routes — something has to own that continuously, and it has to run
somewhere with real kernel networking access (not every node in the mesh
necessarily has that, or a public IP to be dialed on), which is why this
runs as its own pod with elevated network capabilities rather than being
pushed out to every workload.

### 2. A Tailscale client that bridges WireGuard networks onto the tailnet

**What**: stagbru also runs (supervises, configures) `tailscaled` as a
member of an existing Tailscale/headscale network, acting as a subnet
router: it advertises the CIDR ranges that live behind its WireGuard
node(s) — plus any other locally reachable ranges — so that tailnet-only
devices (a laptop, a phone, anything that's never heard of WireGuard) can
reach hosts on those networks as if they were directly connected.

**How**: `tailscaled` runs in **kernel TUN mode**, not its default userspace
mode — userspace networking can only terminate traffic addressed to the
process itself, it can't forward packets on to some other host, which is
exactly what subnet routing requires. stagbru starts `tailscaled` as a
supervised child process (restarting it with backoff if it exits) and
drives it entirely through its LocalAPI: setting its login server and
hostname, telling it which routes to advertise, and handling the one-time
auth-key login. Because WireGuard (`wg0`, or `wg1`, …) and Tailscale
(`tailscale0`) are interfaces inside the *same* network namespace, a
tailnet packet addressed to a WireGuard-side network simply has a normal
kernel route pointing at the right WireGuard interface — no second hop
through another pod, no next-hop resolution, nothing stagbru has to
arrange beyond installing that route (see part 1). The reverse direction —
WireGuard peers reaching tailnet-sourced traffic — needs one more thing:
those peers only accept packets whose *source* address is one of this
node's own WireGuard addresses (that's the whole point of cryptokey
routing), so traffic actually originating from the Tailscale side has to be
masqueraded to look like it came from the WireGuard node itself on the way
out — one nftables rule, part of the same table described below.

**Why**: this is what actually connects "a user's laptop on the tailnet"
to "a host that's only reachable over WireGuard" — without it, the tailnet
and the WireGuard mesh would be two separate islands that happen to run on
adjacent infrastructure. Doing the bridging in the *same pod* as the
WireGuard node (rather than a separate pod routing to it) removes an entire
class of problem that only exists because of that separation in the first
place — resolving another pod's real IP as a route next-hop, the kernel
refusing to treat it as on-link without extra flags, and that whole path
only working if both pods happen to land on the same cluster node. None of
that is a requirement of subnet routing itself; it's purely a consequence
of the two capabilities living in different network namespaces, which
sharing one pod eliminates outright.

### Shared plumbing: one nftables table for both

**What**: stagbru owns exactly one nftables table, `table inet stagbru`,
covering both capabilities: it accepts forwarded traffic between the
WireGuard interface(s) and `tailscale0`, and masquerades traffic leaving
over each WireGuard interface that needs it (see part 2's "Why").

**How**: the whole table is rebuilt from the current configuration and
peer/route state and applied as one atomic replace — never incrementally
patched — so there is no window where only some of the expected rules are
in place. No `iptables` anywhere: the environment this runs in has no
legacy iptables kernel modules, so both WireGuard's forwarding/NAT and
Tailscale's own firewall mode have to be nftables-native.

**Why**: one table, atomically applied, owned exclusively by stagbru,
means its contents are always a pure function of "what networks and peers
are configured right now" — never a snapshot of some rules applied at
startup plus whatever incremental changes happened to land since. That's
what makes it safe to change on every reconcile, not just once at boot.

## Hard constraints (do not violate)

- **Do not change any WireGuard identity, for any configured network.** For
  today's one real network: private key still comes from the
  `<GATEWAY_POD>-self` Secret, address stays `100.192.8.5`, addresses
  `100.192.8.0/24` + `172.16.6.0/32`, listen port `51820`, node name
  `<WG_NODE_NAME>`. Never touch `wireguard_asymmetric_key.gateway` or the
  Vault publishing in `infra/vault/<VAULT_STORE>`. The same rule applies to
  any future second network — its private key Secret, address, and node
  name become equally load-bearing identity the moment it's added, not
  something to be casually regenerated during development.
- **Do not change the Tailscale identity.** Reuse the existing Longhorn
  `tailscale-state` PVC. Reuse the `tailscale-authkey` Secret and the
  in-cluster tofu (`destroyResourcesOnDeletion: false` stays false). Keep
  `TS_HOSTNAME=<TS_HOSTNAME>` **exactly** (see current-state.md — this is
  not the same string as the WireGuard node name). Keep tags `tag:<DMZ_SITE>`,
  `tag:<NAMESPACE>`, `tag:site-c`, `tag:site-d` so all routes stay
  auto-approved.
- **Keep the exposure exactly as it is today — including the one place the
  original spec and reality disagree:**
  - a `LoadBalancer` on UDP `51820`, pinned to `172.16.6.168`,
  - `metallb.io/allow-shared-ip: dmz-public`,
  - **`externalTrafficPolicy: Local`** (not `Cluster` — `Cluster` was tried
    and reverted; see current-state.md for the confirmed-live failure mode:
    cross-node flannel hop corrupting WireGuard's return-traffic source IP),
  - a single replica with `strategy: Recreate`.
- **No iptables anywhere.** Talos has no legacy iptables kernel modules. Use
  nftables only, through `github.com/google/nftables`. Set
  `TS_DEBUG_FIREWALL_MODE=nftables` (or the tailscaled-native equivalent) on
  the supervised `tailscaled`.
- **stagbru owns its nft table exclusively** (`table inet stagbru`).
  Reconciling it means atomically replacing that table. Never touch
  flannel's or tailscale's own tables.
- **Never run `tofu apply`, never push to main, and never apply to the
  cluster** without explicit human approval. Produce plans and diffs, then
  stop.

## Architectural decisions beyond the original spec

Two choices were specifically requested for this plan: use the
`tailscale/tailscale` Go module directly rather than only shelling out to a
prebuilt image, and use `koanf` for config so stagbru itself can be
configured from a file (YAML or TOML) as well as env vars/flags, not only
env vars.

### Use `tailscale.com` as a real Go dependency, not just a sibling binary

`tsnet` (tailscale's embeddable-node package) is not an option here: it
always runs the userspace gVisor netstack internally and is built for
*serving/dialing from inside the Go process*, not for being a kernel-mode
subnet router that forwards third-party traffic between real interfaces.
The hard constraint above (`TS_USERSPACE=false`, real `wg0`↔`tailscale0`
kernel forwarding) rules it out.

What *is* valuable, and is what "use the tailscale repo" should mean here:

1. **Build the bundled `tailscaled` from the same `tailscale.com` module
   version stagbru imports**, instead of copying a binary out of
   `tailscale/tailscale:latest` (today's image, which isn't even pinned —
   see current-state.md). Concretely: `go.mod` has `require tailscale.com
   vX.Y.Z`; the Dockerfile's build stage does
   `go build -o /tailscaled tailscale.com/cmd/tailscaled` using that same
   `go.sum`-locked version, and separately builds `stagbru` itself, which
   imports `tailscale.com/client/local`, `tailscale.com/ipn`,
   `tailscale.com/types/key` at that identical version. One `go.mod` entry
   is then the single place version drift between the supervisor and the
   supervised daemon becomes structurally impossible — `go mod tidy` /
   `go build` fail loudly if they ever disagree, instead of silently
   drifting the way two separately-tagged container images could.
2. Use `tailscale.com/ipn.Prefs` (and related types under
   `tailscale.com/ipn`, `tailscale.com/tailcfg`) directly when calling
   LocalAPI through `tailscale.com/client/local`, instead of hand-rolling
   JSON structs for prefs. This is what `internal/tailscale` already implied
   in the original spec ("pinned to the same version as the bundled
   tailscaled") — this plan just makes the version pin automatic via the
   build rather than a manually-maintained constant.
3. **Phase 2 explicitly re-checks**, before committing to this: whether the
   pinned `tailscale.com` version's `cmd/tailscaled` builds cleanly with
   `CGO_ENABLED=0` for the target platform, and whether any
   platform-specific build tags (e.g. for nftables support, or the
   `TS_DEBUG_FIREWALL_MODE=nftables` codepath) need extra `go build` flags.
   Tailscale does ship this as a normal buildable Go command, but verify
   against the exact pinned tag rather than assuming.

### Use `koanf` for stagbru's own configuration

Today's two pods are configured entirely through env vars baked into the
Kubernetes manifests (see current-state.md's env tables). stagbru keeps env
vars as the primary path in-cluster (12-factor, no manifest changes needed
beyond the ones in "Manifest changes" below) but adds a config file as an
optional lower-priority layer, specifically to make local development and
the Phase 1/2 integration tests usable without a full k8s Secret/env setup.

Use `github.com/knadh/koanf/v2` with this provider order (later overrides
earlier):

1. `confmap` provider — compiled-in defaults (interface name `wg0`, listen
   port `51820`, `HTTP_ADDR :9090`, etc. — see the config table below).
2. `file` provider, **optional**, path from `--config` flag or
   `STAGBRU_CONFIG` env var, parsed with koanf's `yaml` parser if the file
   ends `.yaml`/`.yml`, or its `toml` parser if it ends `.toml`. Not finding
   this file is not an error — it's how the in-cluster pod runs today
   (pure env vars), and it's an optional convenience for local dev.
3. `env` provider, prefix `STAGBRU_`, so e.g. `STAGBRU_WG_INTERFACE` sets
   `wg_interface`. The existing env var names in the original spec's config
   table (`WG_INTERFACE`, `TS_HOSTNAME`, ...) become the **unprefixed**
   koanf keys; keep the manifests setting the exact same bare names
   (`WG_INTERFACE=wg0`) by also registering an env-var alias table in
   `internal/config` — don't force a rename of every manifest env var to add
   a `STAGBRU_` prefix purely for koanf's sake.
4. `posflag` provider (koanf's `pflag`/`flag` integration) for CLI flags,
   highest priority, for one-off overrides when running `stagbru` by hand
   (debugging, the integration tests).

`internal/config` unmarshals the merged koanf tree into a single typed
struct and validates it (required fields present, CIDRs/durations parse,
etc.) — same validation surface the original spec already asked for, just
sourced from koanf instead of raw `os.Getenv`.

This does **not** replace the peer list format. `peers.json` (from the
`wg0-conf` Secret, see "Manifest changes") stays a separate, hot-reloaded
input handled by `internal/peers`, not config — it changes on its own
schedule (Vault refresh), is watched by an informer, and must never block on
or get re-merged with the static config tree.

### Generalize `internal/wg` to manage multiple WireGuard networks, not one

Today there is exactly one WireGuard network (the `<MESH>` mesh, interface
`wg0`). This plan builds `internal/wg` to manage **a list of independent
networks from day one**, rather than hardcoding a single interface and
retrofitting a list later — retrofitting would mean changing every
assumption in `internal/wg`/`internal/peers`/`internal/nft` about there
being exactly one device, one listen port, one peers Secret, after they're
already written and tested against that assumption. Building the list model
first and expressing today's one real network as a one-element list costs
nothing extra up front and avoids a second migration later.

**Config shape** (`wg.networks`, a list — this is exactly the kind of
structure a flat env-var scheme can't express cleanly, which is the other
reason `koanf`'s file provider matters here, not just for local-dev
convenience):

```yaml
wg:
  networks:
    - name: default            # logical key: metrics label, log prefix, peers-secret default
      interface: wg0           # Linux IFNAMSIZ limit applies: max 15 chars, validated at startup
      listen_port: 51820
      self_secret: gateway-self   # Secret name; envFrom keys PRIVATE_KEY, IP (same shape as today)
      self_name: wg-node-name     # excluded from this network's own peer list
      peers_secret: wg0-conf      # Secret name, key `peers.json`
      masquerade: true             # install `oifname <interface> masquerade`?
      advertise_to_tailscale: true # union this network's peer AllowedIPs into TS_ROUTES?
    # - name: second-network
    #   interface: wg1
    #   listen_port: 51821
    #   ...
```

- **Backward compatible with zero config changes to the current cluster**:
  if `wg.networks` is empty/unset, `internal/config` synthesizes a single
  network named `default` from the existing flat env vars
  (`WG_INTERFACE`, `WG_LISTEN_PORT`, `WG_SELF_SECRET`, `WG_PEERS_SECRET`,
  `WG_SELF_NAME`) — this is exactly today's real deployment, see
  current-state.md. The flat env vars are sugar for a one-network config,
  not a second, parallel mechanism to keep in sync.
- **Validation at startup, across the whole list**: every `interface` is
  unique and ≤ 15 bytes; every `listen_port` is unique; every `name` is
  unique (used as the metrics/log label and the default `peers_secret`
  prefix); and no two networks' peer `allowed_ips` overlap (an exact overlap
  between two different networks' peers is ambiguous to route and gets
  rejected at validation, not silently resolved by "whichever reconciled
  last wins").
- **`internal/wg` owns one `wgctrl` device + one reconcile loop per
  network**, keyed by `name`. A failure in one network's informer or
  reconcile (e.g. its peers Secret is temporarily malformed) must not stop
  other networks from reconciling — isolate failures per network, not
  globally, and surface which network failed in both the log line and the
  `reconcile_total{result,network}` metric.
- **`internal/peers` runs one informer per distinct `peers_secret`** (two
  networks may share a `peers_secret` if their peers genuinely live in the
  same Secret/namespace — don't assume 1:1 with networks).
- **`internal/nft`'s `table inet stagbru` generalizes to the whole
  configured set**: `iifname`/`oifname` sets for the forward-accept rule
  become `{wg0, wg1, ..., tailscale0}` (every configured interface plus
  tailscale0), and the postrouting rule becomes one `oifname <interface>
  masquerade` line per network with `masquerade: true` — not every network
  necessarily needs it (a network that's purely peer-to-peer and never
  forwards tailscale-sourced traffic doesn't need MASQUERADE at all; see
  current-state.md for exactly why today's single network does). Still one
  atomic table replace for the whole set, same as the single-network design
  — never a per-network table, which would reopen a window where only some
  networks' rules exist.
- **Tailscale route advertisement becomes derived, not hand-maintained,
  where possible**: for every network with `advertise_to_tailscale: true`,
  union its peers' `allowed_ips` into the advertised-routes set, instead of
  keeping a second, manually-updated CIDR list (today's `TS_ROUTES`/`ROUTES`
  env var, which already drifted once from the READMEs — see
  current-state.md). `TS_ROUTES` becomes the *extra*, locally-reachable
  CIDRs that aren't behind any WireGuard network (today's hardcoded
  `LOCAL_ROUTES`, e.g. the DMZ subnet) rather than the whole list. This
  means adding a peer, or a whole new network, to the WireGuard side
  automatically widens what's advertised to the tailnet — no second place
  to remember to update. Keep `TS_ROUTES` itself as an explicit override
  escape hatch (union'd in regardless of any network), not removed.
- **Metrics and logs gain a `network` label** everywhere they currently have
  a bare `peer` label (see "Probes and metrics" below) — a metric with only
  `{peer="..."}` and no network becomes ambiguous the moment a second
  network exists, since nothing guarantees public keys are unique *across*
  networks.
- **Startup/shutdown iterate the list**: "create or adopt `wg0`" (startup
  step 2) and "delete `wg0`" (shutdown step 4) both become "for each
  configured network, in config order" — all must succeed before the pod
  reports ready; on shutdown, best-effort delete all of them even if one
  fails, rather than stopping at the first error.

This is a real capability addition, not just future-proofing for its own
sake: it lets `stagbru` eventually gateway a second site-to-site mesh (or a
second mesh with a different trust boundary, e.g. a partner network that
shouldn't share routes with `<MESH>`) from the same pod, without a second
binary or a second Deployment. Nothing about the initial cutover (Phase 5)
requires a second network to exist — it ships as a one-element list against
the real infra today, see the open-questions entry on this below.

## Repository layout

```
cmd/stagbru/main.go           flags/env/config wiring, signal handling
internal/config/              koanf setup, validation
internal/peers/                Secret informer → []Peer (JSON decode, validation)
internal/wg/                   N networks: link/address mgmt, peer diff+apply, route sync
internal/nft/                  build + atomically replace `table inet stagbru` for all networks
internal/tailscale/            tailscaled supervisor + LocalAPI prefs
internal/health/                /healthz, /readyz, /metrics
Dockerfile                     multi-stage; builds stagbru + tailscaled from the same pinned tailscale.com version
```

`cmd/main.go` (current placeholder) moves to `cmd/stagbru/main.go` as part of
Phase 1 scaffolding.

Dependencies:

- `golang.zx2c4.com/wireguard/wgctrl`
- `github.com/vishvananda/netlink`
- `github.com/google/nftables`
- `k8s.io/client-go`
- `tailscale.com` (pinned; both `tailscale.com/client/local` for LocalAPI and
  `tailscale.com/cmd/tailscaled` built from the same version — see above)
- `github.com/knadh/koanf/v2` + `github.com/knadh/koanf/providers/{confmap,file,env,posflag}`
  and `github.com/knadh/koanf/parsers/{yaml,toml}`
- `github.com/prometheus/client_golang`

## Configuration

Bare env var names below describe the **single, default network** — what
the in-cluster manifests set directly today (unchanged from today where
applicable, see current-state.md), and internally they populate network
`default`'s fields in the `wg.networks` list described above. The same keys
are also valid in an optional YAML/TOML config file via koanf, lowercased
and dotted (e.g. `wg.interface`, `tailscale.routes`); a config file is also
the only way to express a *second* network (`wg.networks[1]...`), since
there's no clean flat-env-var spelling for a list of structs.

| Var | Default | Purpose |
|---|---|---|
| `WG_INTERFACE` | `wg0` | interface name (default network) |
| `WG_LISTEN_PORT` | `51820` | (default network) |
| `WG_SELF_SECRET` | `<GATEWAY_POD>-self` | private key + `IP` env keys (envFrom also acceptable) |
| `WG_PEERS_SECRET` | `wg0-conf` | ExternalSecret output, key `peers.json` |
| `WG_SELF_NAME` | `<WG_NODE_NAME>` | excluded from peers defensively |
| `WG_MASQUERADE` | `true` | install `oifname wg0 masquerade`? (default network) |
| `WG_ADVERTISE_TO_TAILSCALE` | `true` | union this network's peer AllowedIPs into the tailnet's advertised routes? |
| `POD_NAMESPACE` | downward API | |
| `TS_LOGIN_SERVER` | `<HEADSCALE_URL>` (see `current-state.md`; real value redacted here since this repo is public) | |
| `TS_AUTHKEY` | from `tailscale-authkey` Secret, key `TS_AUTHKEY` | |
| `TS_STATE_DIR` | `/var/lib/tailscale` | PVC mount |
| `TS_HOSTNAME` | `<TS_HOSTNAME>` | **must match today's exactly** — see current-state.md |
| `TS_ROUTES` | `172.16.6.0/24,100.192.8.0/24` | **extra** advertised routes not already covered by any network's own peer AllowedIPs (today's hardcoded local-only routes — see the "derived, not hand-maintained" point above for why this is shorter than the original spec's 6-CIDR default) |
| `HTTP_ADDR` | `:9090` | probes + metrics |
| `STAGBRU_CONFIG` | unset | optional path to a YAML/TOML config file (koanf `file` provider) — required to configure more than one network |

`internal/config` must still validate that the effective `TS_ROUTES` ∪
(every `advertise_to_tailscale` network's peer AllowedIPs) reproduces
today's full advertised-routes value from current-state.md exactly, as a
Phase 1/2 test — the derivation is only trustworthy if it's checked against
the known-good value at least once, not assumed correct by construction.

## Behaviour spec

(Unchanged from the original spec in substance; repeated here so this repo
is self-contained.)

### Startup order

Each step must succeed before the next one starts. On failure, exit non-zero
and let Kubernetes restart the pod.

1. Verify `net.ipv4.ip_forward=1`. An initContainer sets it (see Manifests).
   If it isn't set, fail with a clear error.
2. Validate the configured `wg.networks` list (unique names/interfaces/listen
   ports, interface names ≤ 15 bytes, no cross-network AllowedIPs overlap —
   see "Generalize `internal/wg`" above), then for each network, in config
   order: create or adopt its interface (set private key, listen port,
   address; bring the link up; if a stale link exists, adopt it rather than
   failing). All configured networks must come up before continuing.
3. Install `table inet stagbru`:
   - forward accept for `iifname {wg0, wg1, ..., tailscale0}` and
     `oifname {wg0, wg1, ..., tailscale0}` — the full configured interface
     set plus `tailscale0`,
   - one postrouting `oifname <interface> masquerade` line per network with
     `masquerade: true` (replaces today's `iptables ... MASQUERADE` — see
     current-state.md for exactly why the one network today needs it).
4. Start one peer informer per distinct `peers_secret` across all networks.
   Wait for every network's first sync, then do each network's first full
   reconcile (a failure in one network's informer/reconcile must not block
   another's — see above).
5. Start `tailscaled` as a child process: `--tun=tailscale0
   --state=$TS_STATE_DIR/tailscaled.state`, socket in an emptyDir, stdout and
   stderr forwarded to stagbru's log with a prefix.
6. Through LocalAPI: set prefs (`ControlURL`, `Hostname`, `AdvertiseRoutes`,
   `RouteAll=false`, `CorpDNS=false`); if backend state is `NeedsLogin`, call
   Start/Login with the auth key; otherwise reuse persisted state.
7. Mark the pod ready.

### Peer reconcile

Runs independently per network (own informer-triggered loop, own 5-minute
resync timer, own failure isolation). Replaces `wg-quick` + the `wg-reload`
sidecar's polling `wg syncconf` loop (current-state.md) for today's one
network; generalizes directly for any additional ones.

- **Desired state** from that network's `peers.json`. Validate every entry:
  key is valid base64 of 32 bytes, CIDRs parse, endpoint resolves. Drop
  invalid entries (log + metric, tagged with `network`), never drop the
  whole set.
- **Actual state** from that network's `wgctrl.Device()`. Diff by public
  key, one `ConfigureDevice` call, `ReplacePeers=false`: new → add, changed
  (AllowedIPs/endpoint differ) → update with `ReplaceAllowedIPs=true`, gone →
  `Remove=true`.
- **No-op rule**: no changes → no syscalls. Unchanged peers must keep their
  handshake.
- **Routes**: for every AllowedIP of every peer, a route via that network's
  own interface (`scope link`), tagged with one shared dedicated `proto`
  number (shared across all networks, so shutdown/cleanup can identify every
  stagbru-managed route in one pass regardless of which network added it) so
  only stagbru-managed routes are ever touched. Skip any AllowedIP
  overlapping that network's own gateway address.
- **PersistentKeepalive**: 25s for peers with an endpoint (matches today's
  `peers.conf` template exactly).
- Triggered by that network's informer events plus its own periodic
  5-minute resync.

### Tailscale supervision

- Restart `tailscaled` on exit with backoff (1s, doubling, capped at 30s).
  Re-apply prefs after each restart.
- Prefs' `AdvertiseRoutes` is computed from `TS_ROUTES` ∪ every
  `advertise_to_tailscale` network's current peer AllowedIPs (see
  "Generalize `internal/wg`" above) — recomputed whenever any contributing
  network's peer set changes, not just at startup.
- Watch the LocalAPI IPN bus; expose backend state, tailnet IP, and
  per-advertised-route approval status as metrics.

### Shutdown (SIGTERM)

1. Mark the pod not ready.
2. SIGTERM `tailscaled`, wait up to 10s.
3. Delete `table inet stagbru`.
4. Delete every configured network's interface, best-effort (don't stop at
   the first failure — attempt all, then exit non-zero if any failed).
5. Exit 0.

### Probes and metrics

- `/healthz`: process alive, no configured network's reconcile loop wedged
  (< 10 min since its last iteration).
- `/readyz`: every configured network's interface up and first reconcile
  done, tailscale backend state `Running`.
- Metrics, prefixed `stagbru_`:
  `wg_peer_last_handshake_seconds{network,peer}`,
  `wg_peer_rx_bytes{network,peer}`, `wg_peer_tx_bytes{network,peer}`,
  `wg_peers_desired{network}`, `wg_peers_configured{network}`,
  `wg_peers_invalid_total{network}`, `reconcile_total{network,result}`,
  `tailscale_backend_state{state}`, `tailscale_route_approved{route}`,
  `tailscaled_restarts_total`.

No ServiceMonitor/PodMonitor — confirmed absent cluster-wide
(current-state.md); don't add one speculatively.

## Manifest changes

These belong in `<INFRA_REPO>`, not this repo — produce diffs there,
never `tofu apply`/push without explicit approval. Everything they need to
reference (identity values, routes, Service shape, PVC name, Secret
names/keys) is already pinned down in `current-state.md` so no further
spelunking in that repo should be needed to write them.

None of this is affected by the multi-network generalization above: the
real cutover ships `wg.networks` as a one-element list built from the exact
flat env vars below, which is the one real network that exists today. A
second network is a future addition to this same manifest, not something
Phase 4/5 needs to provision.

1. **`wg0-conf` ExternalSecret.** Change the template to emit a `peers.json`
   key: a JSON array of `{name, public_key, allowed_ips[], endpoint|null}`.
   Keep the `<DMZ_NET>` → `global` endpoint preference and the self-exclusion
   exactly as today. Keep the old `peers.conf` key alongside it until
   cutover is done.
2. **New `stagbru/` app** (replacing `<GATEWAY_POD>/` +
   `<TAILSCALE_POD>/`), with:
   - **Deployment**, 1 replica, `Recreate`: privileged `enable-ip-forward`
     initContainer (unchanged); main container with `NET_ADMIN`, `NET_RAW`,
     `/dev/net/tun` hostPath, downward-API `POD_NAMESPACE`, `envFrom`
     `<GATEWAY_POD>-self`, `TS_AUTHKEY` from `tailscale-authkey`; volumes:
     the existing `tailscale-state` PVC (reused, not recreated) and an
     emptyDir for the tailscaled socket; probes on `:9090`.
   - **ServiceAccount, Role, RoleBinding**: `get`/`list`/`watch` on Secret
     `wg0-conf` only (`resourceNames`).
   - **LoadBalancer Service**: copy `<GATEWAY_POD>`'s current Service
     **verbatim**, including `externalTrafficPolicy: Local` (do not "fix" it
     to `Cluster` — see hard constraints), selector changed to the new app.
3. **Leave the `<TAILSCALE_POD>/tofu` tofu where it is** (default per
   current-state.md — moving/renaming the Terraform object is a state
   change requiring explicit sign-off). `stagbru/`'s manifests just reference
   the `tailscale-authkey` Secret it already produces.
4. **Rewrite or drop
   `<GATEWAY_POD>/tests/connectivity-test.yaml`** — it depends on the
   pod affinity + `<GATEWAY_POD>-pod` DNS + `onlink` routes, none of
   which exist once there's one pod. A stagbru-era replacement is a plain Job
   with no affinity that execs into the single pod and runs `wg show` /
   `ping`. (This manifest wasn't in the original spec's deletion list —
   found while extracting current-state.md.)
5. **Delete only after cutover is verified**: the `<GATEWAY_POD>` and
   `<TAILSCALE_POD>` Deployments, the headless `<GATEWAY_POD>-pod`
   Service, the `<TAILSCALE_POD>-start` ConfigMap, the old LoadBalancer
   Service, `peers.conf` from the `wg0-conf` template.
6. **One README** at `stagbru/README.md` merging the still-relevant parts of
   both old READMEs: identity, why MASQUERADE is needed, route
   auto-approval, kernel mode and nftables, exposure (including the
   `externalTrafficPolicy: Local` rationale verbatim — it's a hard-won fix,
   don't let it get silently "corrected" by a future reader), verification.
   Drop the affinity and `onlink` sections.

## Phases and acceptance criteria

### Phase 1 — Go binary, WireGuard only (no cluster changes)

Scaffold `cmd/stagbru/main.go` (move off the current placeholder
`cmd/main.go`). Implement `config` (koanf wiring, including the
`wg.networks` list and its single-network env-var fallback), `peers`, `wg`,
`nft`, `health` — all built against the multi-network model from the start
(see "Generalize `internal/wg`" above), exercised in these tests with both
one and two configured networks.

Unit tests: peer diff (add/update/remove/no-op/invalid), route set
computation, nft table construction for one network *and* for two networks
with only one `masquerade: true` (golden-file comparison), JSON decoding,
koanf layering (file < env < flag precedence, missing file is not an
error), network-list validation (duplicate interface/listen_port/name
rejected, interface name > 15 bytes rejected, overlapping AllowedIPs across
two networks rejected), and the derived-`TS_ROUTES` computation reproducing
current-state.md's known-good 6-CIDR value from one network's peers plus
the 2-CIDR `TS_ROUTES` override.

Integration test: a privileged Docker container or netns with the
`wireguard` kernel module. Bring up two stagbru-like peers on one network,
check: they handshake; a peer update doesn't reset the other peer's
handshake; a peer removal removes its routes; then repeat with a second,
independent network configured alongside the first and confirm each
network's reconcile is unaffected by a deliberately broken peers Secret on
the other.

**Done when** `go test ./...`, `go vet`, and `staticcheck` are green and the
integration test passes.

### Phase 2 — Tailscale supervision

Implement `internal/tailscale`, including the pinned `tailscale.com` Dockerfile
build verification described above. Integration test against a throwaway
headscale container: node joins, routes advertise, a restart reuses state
(same node ID, no re-registration).

**Done when** tests are green and prefs are verified via `tailscale debug
prefs` (or the equivalent LocalAPI call), and the "builds the pinned
`cmd/tailscaled` cleanly" check from the architectural-decisions section
above has actually been run, not assumed.

### Phase 3 — Image and CI

#### Dockerfile

Multi-stage, **Alpine-based** (decided over a Red Hat UBI base: both
binaries build `CGO_ENABLED=0`, so musl's usual cgo/DNS gotchas don't apply;
UBI's actual advantages — RHEL support entitlement, FIPS-validated crypto —
don't apply to this project, it just buys unnecessary image weight. Alpine
also makes the debugging tools below a one-line `apk add`):

1. **`tailscaled` build stage** (`golang:<pinned>-alpine`): `go build
   -trimpath -ldflags="-s -w" -o /out/tailscaled tailscale.com/cmd/tailscaled`
   at the exact `tailscale.com` version pinned in `go.mod` (see
   "Architectural decisions" above — this is *why* this is a separate
   stage from `stagbru`'s own build: same `go.sum`-locked version, one
   source of truth).
2. **`stagbru` build stage** (same base image/Go version): `go build
   -trimpath -ldflags="-s -w" -o /out/stagbru ./cmd/stagbru`.
3. **Final stage** (`alpine:<pinned>`):
   - `apk add --no-cache ca-certificates wireguard-tools iproute2` — CA
     certs for TLS to Vault/the headscale control server; `wg`/`ip` are for
     humans running `kubectl exec ... -- wg show` (current-state.md's
     verification steps lean on exactly this), never called
     programmatically by stagbru itself (that's `wgctrl`/`netlink`
     directly).
   - `COPY --from=` both binaries to `/usr/local/bin/`.
   - No `USER` directive — runs as root (needs `NET_ADMIN`/`NET_RAW`, same
     as today's two pods).
   - `ENTRYPOINT ["/usr/local/bin/stagbru"]`.

Pin both the Go version and the Alpine version as explicit tags (not
`:latest`/`:alpine`) — same reasoning as pinning `tailscale.com`: this
project's whole point is removing unpinned `:latest` images (today's
`tailscale/tailscale:latest`, current-state.md), don't reintroduce the same
problem one layer down.

#### GitHub Actions

No existing CI or registry convention to inherit anywhere in `<INFRA_REPO>`
(current-state.md) — this plan defaults to **GitHub Actions** +
**`ghcr.io/snaerverk/stagbru`** (this repo already lives at
`github.com/snaerverk/stagbru`, and GHCR needs no extra registry
infrastructure or secret: `GITHUB_TOKEN` with `packages: write` is enough);
confirm or override before Phase 3 starts.

Two jobs, one workflow (`.github/workflows/ci.yml`):

1. **`test`** — runs on every push and every PR, any branch: `go build
   ./...`, `go vet ./...`, `go test ./...`, `staticcheck ./...`. This is the
   gate; the image job depends on it passing.
2. **`image`** — runs only on push to `main` (and on `v*` tags for
   releases), **after** `test` passes: `docker/build-push-action` with
   QEMU/buildx, pushing to `ghcr.io/snaerverk/stagbru`. Tags:
   - `sha-<short-sha>` on every `main` push (traceable to an exact commit),
   - `latest` on every `main` push (floating pointer to the newest build —
     fine for a floating *tag*, this is orthogonal to "don't run unpinned
     images in the cluster": the cluster manifest pins `sha-<short-sha>` or
     `vX.Y.Z`, never `latest`),
   - `vX.Y.Z` (+ `v1`/`v1.2` convenience tags) on a pushed `v*` git tag.

   Illustrative shape (refine at implementation time, not a final file):

   ```yaml
   name: ci
   on:
     push:
       branches: [main]
       tags: ["v*"]
     pull_request:

   permissions:
     contents: read
     packages: write

   jobs:
     test:
       runs-on: ubuntu-latest
       steps:
         - uses: actions/checkout@v4
         - uses: actions/setup-go@v5
           with: { go-version-file: go.mod }
         - run: go build ./...
         - run: go vet ./...
         - run: go test ./...
         - uses: dominikh/staticcheck-action@v1

     image:
       needs: test
       if: github.event_name == 'push'
       runs-on: ubuntu-latest
       steps:
         - uses: actions/checkout@v4
         - uses: docker/setup-buildx-action@v3
         - uses: docker/login-action@v3
           with:
             registry: ghcr.io
             username: ${{ github.actor }}
             password: ${{ secrets.GITHUB_TOKEN }}
         - uses: docker/metadata-action@v5
           id: meta
           with:
             images: ghcr.io/snaerverk/stagbru
             tags: |
               type=sha,prefix=sha-,format=short
               type=raw,value=latest,enable={{is_default_branch}}
               type=semver,pattern={{version}}
         - uses: docker/build-push-action@v6
           with:
             push: true
             tags: ${{ steps.meta.outputs.tags }}
             labels: ${{ steps.meta.outputs.labels }}
   ```

   Open detail: whether to build `linux/amd64` only or also `linux/arm64`
   depends on the cluster nodes' actual architecture, which hasn't been
   confirmed — default to `linux/amd64` only until that's checked, since
   adding a platform later is a one-line change to the `build-push-action`
   `platforms:` input, not a redesign.

- Tailscale version to pin: **no existing pinned version to inherit** — the
  current image runs `:latest`. Pick an explicit `tailscale.com` tag at the
  start of Phase 3 by checking the current stable release on
  `github.com/tailscale/tailscale/releases` at implementation time, rather
  than hardcoding a guess into this plan.

**Done when** `test` and `image` both pass in CI on a `main` push, and
image size/contents are listed in the PR.

### Phase 4 — Manifests (PR only)

All changes from "Manifest changes" above, in one PR, old apps still
present, `peers.json` added alongside `peers.conf`.

**Done when** manifests render cleanly through `kustomize build` and the PR
description includes the cutover runbook (Phase 5).

### Phase 5 — Cutover (human executes; this repo's job is the runbook)

1. Pre-checks: record `wg show` and `tailscale status` from the old pods;
   record the tailnet hostname (`<TS_HOSTNAME>`), node ID, and IP.
2. Scale `<GATEWAY_POD>` and `<TAILSCALE_POD>` to 0 (releases UDP
   51820, the shared IP, and the PVC).
3. Scale `stagbru` to 1.
4. Verify:
   - `kubectl -n <NAMESPACE> exec deploy/stagbru -- wg show`: every live <MESH>
     node appears exactly once, no self-peer, fresh handshakes.
   - Tailnet node keeps the same name (`<TS_HOSTNAME>`) and IP; all six
     route components in `TS_ROUTES` show approved.
   - From a tailnet device: reach a <REMOTE_SITE_B> node in `100.244.64.0/18`, a
     <REMOTE_SITE_A> node in `100.244.128.0/18`, a `site-c` host in
     `10.44.64.0/20`, a `site-d` host in `10.44.128.0/20`, and a host in
     `172.16.6.0/24`.
   - From <REMOTE_SITE_B>: reach a `<CLUSTER>` node in `100.192.8.0/24`.
5. Rollback: scale `stagbru` to 0, old Deployments back to 1.
6. After 24h clean: the deletion PR (manifest step 5/6 above).

### Phase 6 — Cleanup PR

Manifest steps 5–6 plus the merged README.

## Open questions

Resolved against the real repos where possible; genuinely open ones are
flagged.

| # | Question | Status |
|---|---|---|
| 1 | Go module / repo | **Resolved**: this repo, `github.com/snaerverk/stagbru`, already exists (`go.mod`, `go 1.27.1`). Binary moves to `cmd/stagbru/`. |
| 2 | Registry | **Resolved (default taken)**: `ghcr.io/snaerverk/stagbru` — see Phase 3's GitHub Actions section. |
| 3 | CI system | **Resolved**: GitHub Actions — see Phase 3 for the concrete two-job workflow (`test`, `image`) and Dockerfile shape (Alpine-based). |
| 4 | Tailscale version to pin | **Open** — current image is unpinned (`:latest`). Pick explicitly at the start of Phase 3 (see above); don't guess a version number now. |
| 5 | Current tailnet hostname | **Resolved**: `<TS_HOSTNAME>` (read from the live Deployment env — see current-state.md). |
| 6 | Other references to the pieces being deleted | **Resolved, with a correction**: `tests/connectivity-test.yaml` also depends on `<GATEWAY_POD>-pod` + the affinity + `onlink` routes and needs rewriting, not just the two ConfigMaps/Services the original spec named. |
| 7 | Does the <TAILSCALE_POD> tofu move? | **Resolved (default taken)**: stays in place. |
| 8 | Flux or ArgoCD? | **Resolved**: Flux + tofu-controller, confirmed via `kustomization.yaml` and the `Terraform` CRD in use. |
| 9 | Prometheus Operator present? | **Resolved**: no, confirmed absent (`grep` for ServiceMonitor/PodMonitor across the whole infra repo is empty). Skip. |
| 10 | Is a second WireGuard network actually planned, or is this purely anticipatory? | **Open** — current-state.md documents exactly one real network today; nothing in the infra repo references a second one. This plan builds the multi-network model into Phase 1 regardless (see "Generalize `internal/wg`") because retrofitting it later is expensive, but if a second network's shape (how many peers, does it need `masquerade`, does it need `advertise_to_tailscale`) is already known, say so before Phase 1 so the config schema/validation can be checked against a real second case instead of only a hypothetical one. |
