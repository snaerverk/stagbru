# Test-only image for the Phase 1 WireGuard integration test (see
# docs/plan.md's Phase 1 "Integration test" bullet and test/e2e/README.md).
# This builds cmd/e2e-peer, NOT the production stagbru binary — it is never
# referenced by the real Dockerfile Phase 3 will add.
#
# Go version note: go.mod pins `go 1.27.1`. At the time this was written
# (2026-10-02) `golang:1.27-alpine` already exists as a published tag on
# Docker Hub (verified with `docker manifest inspect golang:1.27-alpine`),
# so it's used directly rather than falling back to `golang:1-alpine`.
FROM golang:1.27-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/e2e-peer ./cmd/e2e-peer
COPY pkg ./pkg

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/e2e-peer ./cmd/e2e-peer

FROM alpine:3.22

# wireguard-tools: the `wg` CLI used by cmd/e2e-peer's /status handler.
# iproute2: used by the test driver (test/e2e/run.sh), via
# `docker compose exec`, to assert that a removed peer's route is gone.
# curl: the test driver also execs into each container to POST /peers and
# poll /healthz and /status, instead of publishing ports to the host.
RUN apk add --no-cache wireguard-tools iproute2 curl

COPY --from=build /out/e2e-peer /usr/local/bin/e2e-peer

ENTRYPOINT ["/usr/local/bin/e2e-peer"]
