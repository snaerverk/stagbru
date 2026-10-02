# Phase 1 WireGuard integration test

This is the integration test described in `docs/plan.md`'s Phase 1 section:

> Integration test: a privileged Docker container or netns with the
> `wireguard` kernel module. Bring up two stagbru-like peers, check: they
> handshake; a peer update doesn't reset the other peer's handshake; a peer
> removal removes its routes.

It exercises `pkg/wg` (`wg.NewNode`, `(*Node).Reconcile`) against two real
kernel WireGuard interfaces, driven over HTTP by a small test-only wrapper
(`cmd/e2e-peer`) instead of asserting anything from a unit test with a fake
`wgctrl` device.

Only the single-network case is covered here ("two stagbru-like peers on
one network" from the plan). The second half of that Phase 1 bullet — a
second, independent network configured alongside the first, with a
deliberately broken peers Secret on one not affecting the other — is not
covered by this test, since that concerns `internal/peers`/multi-network
wiring in `cmd/stagbru`, none of which exists yet.

## What runs

- `cmd/e2e-peer`: brings up one WireGuard interface via `pkg/wg.NewNode`
  with zero peers, then serves `/healthz`, `POST /peers` (decodes a JSON
  peer list and calls `Reconcile`), and `GET /status` (shells out to
  `wg show <iface> dump`). Test scaffolding only — never built or shipped
  as part of the real `stagbru` binary/image.
- `test/e2e/docker-compose.yml`: two services, `peer-a` (`10.99.0.1/24`)
  and `peer-b` (`10.99.0.2/24`), each with `NET_ADMIN` and `/dev/net/tun`,
  on a shared bridge network, each with its own throwaway WireGuard
  keypair baked into the compose file.
- `test/e2e/run.sh`: the driver. Starts the stack, waits for both peers to
  report healthy, `POST`s each peer's desired peer list to the other
  (exercising the "do they handshake" case for real, rather than assuming
  it from static config), polls `/status` until a real handshake is
  observed, re-POSTs an updated peer (changed `AllowedIPs`, same
  identity/endpoint) and confirms the handshake wasn't reset, then POSTs an
  empty peer list and confirms both the peer and its route disappear.

## Running it

```sh
./test/e2e/run.sh
```

It tears the stack down (`docker compose down -v`) on exit, success or
failure, via a `trap`.

## Prerequisites

- Docker with a kernel WireGuard module available to the containers
  (`cap_add: NET_ADMIN` + `/dev/net/tun`, no `--privileged` needed since
  `pkg/wg` only touches its own interface via `netlink`/`wgctrl`).
- This was built and verified against **Docker Desktop on macOS** (which
  runs containers inside a Linux VM, with in-kernel WireGuard support). It
  has not been verified against Docker directly on a Linux host, a
  non-Docker-Desktop VM, or any CI runner — if the kernel WireGuard module
  genuinely isn't available wherever this runs, interface creation in
  `pkg/wg.NewNode` will fail loudly rather than silently falling back to
  anything else.

## CI

This is **not** wired into `go test ./...`, any Makefile target, or
`.github/workflows/`. There is no CI in this repo yet (see
`docs/current-state.md`'s "CI / registry" section), and adding a GitHub
Actions step for this — privileged Docker-in-Docker, kernel WireGuard
inside whatever runner image is used — is a separate, not-yet-done task.
Run it by hand (or wire it into CI later) via `./test/e2e/run.sh`.
