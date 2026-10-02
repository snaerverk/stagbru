# stagbru

A single Go binary that replaces <CLUSTER>'s `<GATEWAY_POD>` +
`<TAILSCALE_POD>` pods (namespace `<NAMESPACE>`) with one pod owning both `wg0`
(the <MESH> site-to-site WireGuard mesh gateway) and `tailscale0` (a
headscale subnet router), plus the nftables rules connecting them.

- [`docs/plan.md`](docs/plan.md) — the implementation plan: architecture,
  phases, manifest changes, cutover runbook, open questions.
- [`docs/current-state.md`](docs/current-state.md) — every fact about the
  current two-pod deployment the plan depends on, extracted from the private
  infra monorepo so this repo is self-contained. Company-identifying values
  (emails, public IP, internal hostnames) are redacted since this repo is
  public — see the note at the top of that file.

No code beyond a placeholder `cmd/main.go` exists yet — see `docs/plan.md`
Phase 1 for what's next.
