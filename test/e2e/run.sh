#!/usr/bin/env bash
# Phase 1 WireGuard integration test driver (docs/plan.md's Phase 1
# "Integration test" bullet). Brings up two stagbru-like peers (cmd/e2e-peer,
# wrapping pkg/wg) via Docker Compose and checks:
#   1. they handshake after a peer update (Reconcile) is pushed to each;
#   2. a peer update (changed AllowedIPs, same key/endpoint) doesn't reset
#      the other peer's handshake;
#   3. a peer removal removes its route.
#
# See test/e2e/README.md for prerequisites and how to run this.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE_FILE="$SCRIPT_DIR/docker-compose.yml"
COMPOSE=(docker compose -f "$COMPOSE_FILE")

# Must match test/e2e/docker-compose.yml's WG_PRIVATE_KEY/PEER_*_PUBLIC_KEY
# env vars.
PEER_A_PUB="5nugtxtT5TAp2VF5Gns5zpHQRT0hQuF/ygU5pNGdqEw="
PEER_B_PUB="aecnfF7mIaRnImSJChoUWnidkQN5h2bPcKdjbjdLBVQ="

HEALTH_TIMEOUT=60
HANDSHAKE_TIMEOUT=30

log() { printf '>>> %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

cleanup() {
	log "tearing down compose stack"
	"${COMPOSE[@]}" down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

exec_in() {
	# exec_in <service> <command...>
	local svc="$1"
	shift
	"${COMPOSE[@]}" exec -T "$svc" "$@"
}

curl_in() {
	# curl_in <service> <curl args...>
	local svc="$1"
	shift
	exec_in "$svc" curl -sS "$@"
}

wait_healthy() {
	local svc="$1"
	local deadline=$((SECONDS + HEALTH_TIMEOUT))
	log "waiting for $svc /healthz"
	while (( SECONDS < deadline )); do
		if curl_in "$svc" -f -o /dev/null http://localhost:8080/healthz 2>/dev/null; then
			log "$svc is healthy"
			return 0
		fi
		sleep 1
	done
	fail "$svc never became healthy within ${HEALTH_TIMEOUT}s"
}

post_peers() {
	# post_peers <service> <json-body>
	local svc="$1"
	local body="$2"
	curl_in "$svc" -f -X POST http://localhost:8080/peers \
		-H 'Content-Type: application/json' \
		-d "$body"
}

status_of() {
	local svc="$1"
	curl_in "$svc" -f http://localhost:8080/status
}

# handshake_of <status-output> <peer-public-key> -> prints the
# latest-handshake field (unix seconds, "0" if none) for that peer's line.
handshake_of() {
	local status="$1"
	local pubkey="$2"
	printf '%s\n' "$status" | awk -F'\t' -v key="$pubkey" '$1 == key { print $5 }'
}

has_peer() {
	local status="$1"
	local pubkey="$2"
	printf '%s\n' "$status" | awk -F'\t' -v key="$pubkey" '$1 == key { found=1 } END { exit !found }'
}

wait_for_handshake() {
	# wait_for_handshake <service> <peer-public-key> -> echoes the
	# nonzero handshake value once observed.
	local svc="$1"
	local pubkey="$2"
	local deadline=$((SECONDS + HANDSHAKE_TIMEOUT))
	local status hs
	while (( SECONDS < deadline )); do
		status="$(status_of "$svc")"
		hs="$(handshake_of "$status" "$pubkey")"
		if [[ -n "$hs" && "$hs" != "0" ]]; then
			echo "$hs"
			return 0
		fi
		sleep 2
	done
	printf 'last /status from %s:\n%s\n' "$svc" "$status" >&2
	fail "$svc never handshaked with peer $pubkey within ${HANDSHAKE_TIMEOUT}s"
}

log "building and starting compose stack"
"${COMPOSE[@]}" up -d --build

wait_healthy peer-a
wait_healthy peer-b

log "pushing initial peer config to peer-a (peer-b) and peer-b (peer-a)"
post_peers peer-a "$(cat <<JSON
[{"name":"peer-b","public_key":"$PEER_B_PUB","allowed_ips":["10.99.0.2/32"],"endpoint":"peer-b:51820"}]
JSON
)" >/dev/null

post_peers peer-b "$(cat <<JSON
[{"name":"peer-a","public_key":"$PEER_A_PUB","allowed_ips":["10.99.0.1/32"],"endpoint":"peer-a:51820"}]
JSON
)" >/dev/null

log "waiting for handshake (peer-a's view of peer-b)"
hs1="$(wait_for_handshake peer-a "$PEER_B_PUB")"
log "peer-a <-> peer-b handshake at $hs1"

log "waiting for handshake (peer-b's view of peer-a)"
hs1b="$(wait_for_handshake peer-b "$PEER_A_PUB")"
log "peer-b <-> peer-a handshake at $hs1b"

log "update check: re-POSTing peer-b to peer-a with changed AllowedIPs"
post_peers peer-a "$(cat <<JSON
[{"name":"peer-b","public_key":"$PEER_B_PUB","allowed_ips":["10.99.0.2/32","10.99.0.3/32"],"endpoint":"peer-b:51820"}]
JSON
)" >/dev/null

status_after_update="$(status_of peer-a)"
hs2="$(handshake_of "$status_after_update" "$PEER_B_PUB")"
if [[ -z "$hs2" || "$hs2" == "0" ]]; then
	printf '%s\n' "$status_after_update" >&2
	fail "peer-a lost its handshake with peer-b after an AllowedIPs update"
fi
if (( hs2 < hs1 )); then
	printf '%s\n' "$status_after_update" >&2
	fail "peer-a's handshake with peer-b regressed after update (was $hs1, now $hs2) -- looks like the peer was removed and re-added instead of updated in place"
fi
log "handshake preserved across update (was $hs1, now $hs2)"

log "removal check: POSTing an empty peer list to peer-a"
post_peers peer-a '[]' >/dev/null

status_after_removal="$(status_of peer-a)"
if has_peer "$status_after_removal" "$PEER_B_PUB"; then
	printf '%s\n' "$status_after_removal" >&2
	fail "peer-a still lists peer-b after removal"
fi
log "peer-a no longer lists peer-b"

routes="$(exec_in peer-a ip route show dev wg0)"
if printf '%s\n' "$routes" | grep -q '10\.99\.0\.2/32'; then
	printf '%s\n' "$routes" >&2
	fail "peer-a still has a route for 10.99.0.2/32 after peer removal"
fi
log "peer-a's route to 10.99.0.2/32 is gone"

log "PASS: handshake established, update preserved it, removal cleaned up the route"
