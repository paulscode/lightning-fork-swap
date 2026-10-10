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

# The host's scheduled jobs live in /etc/cron.d, outside the state that
# moves: install them wherever the service now runs.
install_crons() {
	echo "17 * * * * root $DEPLOY/scripts/backup-secrets.sh" > /etc/cron.d/lfswap-backup
	mkdir -p "$ROOT/monitor"
	chmod 700 "$ROOT/monitor"
	{
		echo "*/5 * * * * root /usr/bin/python3 $DEPLOY/monitor/lfswap_monitor.py >> $ROOT/monitor/monitor.log 2>&1"
		# The weekly report: balances, capacity, swaps and fees
		echo "0 13 * * 1 root /usr/bin/python3 $DEPLOY/monitor/lfswap_monitor.py --report --send >> $ROOT/monitor/monitor.log 2>&1"
	} > /etc/cron.d/lfswap-monitor
	# The on-chain rebalancer; it does nothing while REBALANCE_MODE is off
	mkdir -p "$ROOT/rebalance"
	chmod 700 "$ROOT/rebalance"
	echo "11,41 * * * * root /usr/bin/python3 $DEPLOY/rebalance/lfswap_rebalance.py >> $ROOT/rebalance/rebalance.log 2>&1" > /etc/cron.d/lfswap-rebalance
	if [ "${REBALANCE_MODE:-off}" = off ]; then
		log "REBALANCE_MODE is off in .env: the rebalancer is scheduled but moves nothing"
	fi
	if [ -z "${ALERT_WEBHOOK_URL:-}" ]; then
		log "no ALERT_WEBHOOK_URL in .env: alerts are only written to $ROOT/monitor/monitor.log"
	fi
	if [ -n "${BACKUP_AGE_RECIPIENT:-}" ]; then
		echo "40 3 * * * root $DEPLOY/scripts/backup-offsite.sh" > /etc/cron.d/lfswap-offsite
	else
		rm -f /etc/cron.d/lfswap-offsite
		log "no BACKUP_AGE_RECIPIENT in .env: daily offsite backups are not scheduled"
	fi
	chmod 644 /etc/cron.d/lfswap-backup /etc/cron.d/lfswap-offsite /etc/cron.d/lfswap-monitor /etc/cron.d/lfswap-rebalance 2>/dev/null || true
}

remove_crons() {
	rm -f /etc/cron.d/lfswap-backup /etc/cron.d/lfswap-offsite /etc/cron.d/lfswap-monitor /etc/cron.d/lfswap-rebalance
}

wait_for() {
	local what=$1 tries=$2; shift 2
	for _ in $(seq 1 "$tries"); do
		if "$@" >/dev/null 2>&1; then return 0; fi
		sleep 5
	done
	die "timed out waiting for $what"
}
