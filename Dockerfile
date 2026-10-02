# Production image for stagbru (cmd/stagbru). See docs/plan.md's Phase 3
# "Image and CI" section for the full rationale behind these choices.
#
# This Dockerfile intentionally builds ONLY the stagbru binary. docs/plan.md
# describes a future `tailscaled` build stage alongside this one, but that
# depends on pkg/tailscale existing (it does not yet) and on picking a
# pinned tailscale.com version, which docs/plan.md lists as an explicitly
# open question (open question #4) — not resolved here. Add that second
# build stage, plus a COPY of its output into the final stage below, once
# pkg/tailscale lands and a tailscale.com version is pinned in go.mod.
#
# Go version note: go.mod pins `go 1.27.1`. At the time this was written
# (2026-10-02), `golang:1.27-alpine` is a real, published, pullable tag
# (verified with `docker manifest inspect golang:1.27-alpine`) — same tag
# already used by test/e2e/Dockerfile for the same reason.
FROM golang:1.27-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/stagbru ./cmd/stagbru
COPY pkg ./pkg

# VERSION is overridden at build time via --build-arg VERSION=... (CI passes
# the resolved git sha/tag here); it maps straight onto cmd/stagbru/main.go's
# `var version = "dev"`, which documents the same -ldflags -X convention.
ARG VERSION=dev

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/stagbru ./cmd/stagbru

# alpine:3.22 verified as a real, published, pullable tag (`docker manifest
# inspect alpine:3.22`) as of 2026-10-02 — same tag already used by
# test/e2e/Dockerfile.
FROM alpine:3.22

# ca-certificates: TLS to Vault / the headscale control server (once
# pkg/tailscale lands).
# wireguard-tools, iproute2: `wg`/`ip` for humans running
# `kubectl exec ... -- wg show`/`ip route` (docs/current-state.md's
# verification steps lean on exactly this) — never called programmatically
# by stagbru itself (that's wgctrl/netlink directly, see pkg/wg).
RUN apk add --no-cache ca-certificates wireguard-tools iproute2

COPY --from=build /out/stagbru /usr/local/bin/stagbru

# No USER directive — runs as root. Needs NET_ADMIN/NET_RAW at the pod
# level, same as today's two pods (docs/current-state.md).
ENTRYPOINT ["/usr/local/bin/stagbru"]
