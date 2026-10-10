#!/usr/bin/env bash
# export.sh --check [--margin BLOCKS]
#     Report what the service is in the middle of, and fail if a swap or HTLC
#     times out within BLOCKS (default 36, about six hours) of now.
#
# export.sh --recipient AGE_PUBLIC_KEY [--margin BLOCKS] [--force]
#     Stop the service, archive its state encrypted to the new host's key,
#     and retire this host: its state moves to $ROOT/migrated-<id>/ and a
#     tombstone stops the tooling from starting it again. Knots is left
#     stopped with its chain in place for copy-chain.sh --final.
#
# The rule behind the order: lnd must never run in two places with the same
# channel database, so it is stopped before anything is copied, and this
# host is retired before the archive exists.
. "$(dirname "$0")/lib.sh"

MARGIN=36
RECIPIENT=""
FORCE=0
CHECK_ONLY=0
while [ $# -gt 0 ]; do
	case "$1" in
	--check) CHECK_ONLY=1 ;;
	--margin) MARGIN=$2; shift ;;
	--recipient) RECIPIENT=$2; shift ;;
	--force) FORCE=1 ;;
	*) die "unknown argument $1" ;;
	esac
	shift
done

# Statuses in which the service still has something to do for a swap.
ACTIVE_SUBMARINE="'invoice.set','transaction.mempool','transaction.confirmed','invoice.pending','invoice.paid','transaction.claim.pending','transaction.zeroconf.rejected'"
ACTIVE_REVERSE="'minerfee.paid','transaction.mempool','transaction.confirmed','transaction.refunded'"

check() {
	refuse_if_migrated
	local height soonest ok=0
	height=$(knots getblockcount)
	echo "height $height, margin $MARGIN blocks"

	echo "submarine swaps in progress:"
	psql_q "select id||' '||status||' timeout '||\"timeoutBlockHeight\" from swaps where status in ($ACTIVE_SUBMARINE) order by \"timeoutBlockHeight\"" | sed 's/^/  /'
	echo "reverse swaps in progress:"
	psql_q "select id||' '||status||' timeout '||\"timeoutBlockHeight\" from \"reverseSwaps\" where status in ($ACTIVE_REVERSE) order by \"timeoutBlockHeight\"" | sed 's/^/  /'

	soonest=$(psql_q "select min(t) from (select \"timeoutBlockHeight\" t from swaps where status in ($ACTIVE_SUBMARINE) union all select \"timeoutBlockHeight\" from \"reverseSwaps\" where status in ($ACTIVE_REVERSE)) x")
	if [ -n "$soonest" ] && [ "$soonest" -lt $((height + MARGIN)) ]; then
		echo "  a swap times out at $soonest, within $MARGIN blocks"
		ok=1
	fi

	local htlcs
	htlcs=$(lncli listchannels | jq '[.channels[].pending_htlcs[]?] | length')
	echo "HTLCs in flight: $htlcs"
	local soonest_htlc
	soonest_htlc=$(lncli listchannels | jq '[.channels[].pending_htlcs[]?.expiration_height] | min // empty')
	if [ -n "$soonest_htlc" ] && [ "$soonest_htlc" -lt $((height + MARGIN)) ]; then
		echo "  an HTLC expires at $soonest_htlc, within $MARGIN blocks"
		ok=1
	fi

	if [ "$ok" = 0 ]; then echo "OK to export"; else echo "NOT safe to export now"; fi
	return $ok
}

if [ "$CHECK_ONLY" = 1 ]; then
	check
	exit $?
fi

[ -n "$RECIPIENT" ] || die "--recipient is required: the age public key printed by provision-host.sh on the new host"
command -v age >/dev/null || die "age is not installed"
refuse_if_migrated

if ! check; then
	[ "$FORCE" = 1 ] || die "not safe to export now (see above); wait, or pass --force"
	log "exporting anyway (--force)"
fi

ID=$(date -u +%Y%m%dT%H%M%SZ)-$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
STAGE=$MIGRATION/export-$ID
mkdir -p "$STAGE"
log "export $ID"

log "snapshot of the running service"
"$(dirname "$0")/verify.sh" snapshot "$STAGE/snapshot.json" >/dev/null

log "stopping the backend, then lnd"
dc stop -t 60 boltz
dc stop -t 180 lnd
# From here on lnd must not start on this host again, even if this script
# stops half way: the tombstone goes down now (and is completed below). To
# abort a move before the archive reaches the new host, remove it by hand.
printf 'id=%s\nat=%s\nstate=export in progress; if it stopped, the move was not finished\n' \
	"$ID" "$(date -u +%FT%TZ)" > "$TOMBSTONE"
log "dumping and stopping Postgres"
dc exec -T postgres pg_dumpall -U boltz > "$STAGE/postgres.sql" </dev/null
dc stop -t 60 postgres
dc stop shim tor
log "stopping Knots (flushes its wallet and chain state)"
dc stop -t 300 knots

cat > "$STAGE/manifest.json" <<JSON
{
  "id": "$ID",
  "source_host": "$(hostname)",
  "network": "$NETWORK",
  "created": "$(date -u +%FT%TZ)",
  "lnd_pubkey": "$(jq -r .lnd.pubkey "$STAGE/snapshot.json")"
}
JSON

log "building the archive"
PAYLOAD=$STAGE/payload
mkdir -p "$PAYLOAD/srv" "$PAYLOAD/etc" "$PAYLOAD/deploy"
tar -C "$ROOT" --numeric-owner --exclude='boltz/*.log' --exclude='boltz/sidecar/*.log' \
	-cf "$PAYLOAD/srv/state.tar" lnd boltz postgres secrets webapp "$KNOTS_NET/wallets"
cp -a "$DEPLOY/.env" "$PAYLOAD/deploy/.env"
# The rebalancer's state (a move may be in flight) and the telemetry (a
# consistent copy: the monitor may be writing)
if [ -f "$ROOT/rebalance/state.json" ]; then cp -a "$ROOT/rebalance/state.json" "$PAYLOAD/rebalance-state.json"; fi
if [ -f "$ROOT/telemetry/telemetry.db" ]; then
	python3 -c 'import sqlite3, sys; src = sqlite3.connect(sys.argv[1]); dst = sqlite3.connect(sys.argv[2]); src.backup(dst); dst.close()' \
		"$ROOT/telemetry/telemetry.db" "$PAYLOAD/telemetry.db"
fi
if [ -d /etc/letsencrypt ]; then tar -C /etc --numeric-owner -cf "$PAYLOAD/etc/letsencrypt.tar" letsencrypt; fi
if [ -f /etc/nginx/lfswap.htpasswd ]; then cp -a /etc/nginx/lfswap.htpasswd "$PAYLOAD/etc/"; fi
# Whether the site is still behind the preview password (empty once public)
if [ -f /etc/nginx/lfswap-preview.conf ]; then cp -a /etc/nginx/lfswap-preview.conf "$PAYLOAD/etc/"; fi
cp "$STAGE/manifest.json" "$STAGE/snapshot.json" "$STAGE/postgres.sql" "$PAYLOAD/"
(cd "$PAYLOAD" && find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS)

ARCHIVE=$MIGRATION/lfswap-$ID.tar.gz.age
tar -C "$PAYLOAD" -cf - . | gzip -1 | age -r "$RECIPIENT" -o "$ARCHIVE.tmp"
mv "$ARCHIVE.tmp" "$ARCHIVE"
rm -rf "$PAYLOAD" "$STAGE/postgres.sql"
sha256sum "$ARCHIVE" | cut -d' ' -f1 > "$ARCHIVE.sha256"

log "retiring this host"
RETIRED=$ROOT/migrated-$ID
mkdir -p "$RETIRED/knots"
mv "$ROOT/lnd" "$ROOT/boltz" "$ROOT/postgres" "$RETIRED/"
mv "$ROOT/$KNOTS_NET/wallets" "$RETIRED/knots/wallets"
printf 'id=%s\nat=%s\narchive=%s\nretired_state=%s\n' "$ID" "$(date -u +%FT%TZ)" "$ARCHIVE" "$RETIRED" > "$TOMBSTONE"
dc down >/dev/null 2>&1 || true
remove_crons

cat <<MSG

Export $ID done. This host is retired: its state is in $RETIRED and
$TOMBSTONE stops the tooling from starting it again. Do not start lnd here.

Next, from this host:
  $(dirname "$0")/copy-chain.sh --final root@NEW_HOST
  scp $ARCHIVE $ARCHIVE.sha256 root@NEW_HOST:$MIGRATION/
Then on the new host:
  $DEPLOY/scripts/migrate/import.sh $MIGRATION/$(basename "$ARCHIVE")
MSG
