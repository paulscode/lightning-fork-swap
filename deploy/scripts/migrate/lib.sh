# Shared by the migration scripts. Source it; it sets strict mode.
set -euo pipefail

DEPLOY=/opt/lfswap/deploy
cd "$DEPLOY"
if [ -f .env ]; then
	set -a
	# shellcheck disable=SC1091
	. ./.env
	set +a
fi
ROOT=${LFSWAP_ROOT:-/srv/lfswap}
NETWORK=${NETWORK:-mainnet}
MIGRATION=$ROOT/migration
# Knots keeps each network but mainnet in a subdirectory of its datadir.
case "$NETWORK" in
mainnet) KNOTS_NET=knots/data ;;
*) KNOTS_NET=knots/data/$NETWORK ;;
esac
mkdir -p "$MIGRATION"
chmod 700 "$MIGRATION"

log() { printf '%s %s\n' "$(date -u +%H:%M:%SZ)" "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

dc() { docker compose "$@"; }
knots() { dc exec -T knots bitcoin-cli -datadir=/data "$@" </dev/null; }
lncli() { dc exec -T lnd lncli --network="$NETWORK" "$@" </dev/null; }
psql_q() { dc exec -T postgres psql -U boltz -d boltz -Atc "$1" </dev/null; }

running() { [ -n "$(dc ps -q --status running "$1" 2>/dev/null)" ]; }

# The file that marks a host whose state has moved elsewhere.
TOMBSTONE=$ROOT/MIGRATED

refuse_if_migrated() {
	if [ -f "$TOMBSTONE" ]; then
		die "this host's state was migrated away ($(tr '\n' ' ' < "$TOMBSTONE")); refusing"
	fi
}

wait_for() {
	local what=$1 tries=$2; shift 2
	for _ in $(seq 1 "$tries"); do
		if "$@" >/dev/null 2>&1; then return 0; fi
		sleep 5
	done
	die "timed out waiting for $what"
}
