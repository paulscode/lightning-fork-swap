#!/usr/bin/env bash
# Make sure the backend is using lnd, after a reboot or after lnd was recreated.
#
# Docker restarts every container by itself at boot, without compose's
# depends_on, so boltz can start before lnd answers. It then logs "Skipping LND
# for BTC", never tries lnd again, and serves no pairs until it is restarted.
# Seen on the 2026-10-30 reboot. Restarting lnd alone (docker compose up -d lnd)
# leaves the same state.
#
# This waits for lnd to be healthy, gives the backend a few minutes to come up
# on its own (it can exit and restart while Knots loads its block index), and
# restarts it once if it still offers no swaps. A backend that is fine is left
# alone. Run at boot by lfswap-settle.service (install-host.sh settle), and by
# hand after recreating lnd. Exit status 0 when swaps are offered at the end.
#
#   SETTLE_LND_MINUTES      wait for lnd to be healthy (default 30: its health
#                           check allows 15 minutes to start)
#   SETTLE_BACKEND_MINUTES  wait for the backend, before and after a restart
#                           (default 5)
set -uo pipefail
cd "$(dirname "$0")/.."

LND_MINUTES=${SETTLE_LND_MINUTES:-30}
BACKEND_MINUTES=${SETTLE_BACKEND_MINUTES:-5}
API=${SETTLE_API:-http://127.0.0.1:9001}

log() { echo "settle-backend: $*"; }

lnd_healthy() {
	local id
	id=$(docker compose ps -q lnd 2>/dev/null) && [ -n "$id" ] &&
		[ "$(docker inspect -f '{{.State.Health.Status}}' "$id" 2>/dev/null)" = healthy ]
}

# The same test as the monitor's check_pairs: a BTC/BTC reverse pair.
swaps_offered() {
	curl -fsS --max-time 10 "$API/v2/swap/reverse" 2>/dev/null | jq -e '.BTC.BTC' >/dev/null 2>&1
}

# wait_for <minutes> <test>: poll every 10 s until the test passes.
wait_for() {
	local deadline=$((SECONDS + $1 * 60))
	while [ "$SECONDS" -lt "$deadline" ]; do
		"$2" && return 0
		sleep "${SETTLE_POLL_SECONDS:-10}"
	done
	"$2"
}

if ! wait_for "$LND_MINUTES" lnd_healthy; then
	log "lnd is not healthy after $LND_MINUTES minutes; leaving the backend alone"
	exit 1
fi
log "lnd is healthy"

if wait_for "$BACKEND_MINUTES" swaps_offered; then
	log "the backend offers swaps; nothing to do"
	exit 0
fi

log "the backend offers no swaps; restarting it"
docker compose restart boltz
if wait_for "$BACKEND_MINUTES" swaps_offered; then
	log "the backend offers swaps after the restart"
	exit 0
fi
log "the backend still offers no swaps after a restart; see: docker compose logs boltz"
exit 1
