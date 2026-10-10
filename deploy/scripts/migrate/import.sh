#!/usr/bin/env bash
# import.sh ARCHIVE [--public-ip IP] [--no-start] [--returning]
#
# --returning: this host is the one the service left earlier and it now comes
# back (a rollback is a migration in the other direction, never a restart of
# the old copy: the new host's lnd may have moved the channels on since).
# On the new host, after provision-host.sh: decrypt the archive export.sh made
# for this host's age key, check it, put the state in place, render the
# configuration for this host and start the service in order.
#
# Refuses to run over existing lnd data, on a host whose own state was
# migrated away, or with an archive already imported here.
. "$(dirname "$0")/lib.sh"

ARCHIVE=${1:-}
[ -f "$ARCHIVE" ] || die "usage: $0 ARCHIVE [--public-ip IP] [--no-start]"
shift
PUBLIC_IP_NEW=""
START=1
RETURNING=0
while [ $# -gt 0 ]; do
	case "$1" in
	--public-ip) PUBLIC_IP_NEW=$2; shift ;;
	--no-start) START=0 ;;
	--returning) RETURNING=1 ;;
	*) die "unknown argument $1" ;;
	esac
	shift
done

if [ "$RETURNING" = 1 ] && [ -f "$TOMBSTONE" ]; then
	mv "$TOMBSTONE" "$TOMBSTONE.returned-$(date -u +%Y%m%dT%H%M%SZ)"
	log "this host was retired; the service is returning to it"
fi
refuse_if_migrated
KEY=$MIGRATION/host.agekey
[ -f "$KEY" ] || die "no $KEY: run provision-host.sh first"
if [ -e "$ROOT/lnd/data" ] || [ -n "$(ls -A "$ROOT/postgres" 2>/dev/null)" ]; then
	die "$ROOT already holds lnd or Postgres data; refusing to overwrite it"
fi
if [ -f "$ARCHIVE.sha256" ]; then
	[ "$(sha256sum "$ARCHIVE" | cut -d' ' -f1)" = "$(cat "$ARCHIVE.sha256")" ] || die "archive checksum mismatch"
	log "archive checksum ok"
fi

STAGE=$(mktemp -d "$MIGRATION/import.XXXXXX")
trap 'rm -rf "$STAGE"' EXIT
age -d -i "$KEY" "$ARCHIVE" | gunzip | tar -C "$STAGE" -xf -
(cd "$STAGE" && sha256sum --quiet -c SHA256SUMS) || die "archive contents do not match their checksums"
ID=$(jq -r .id "$STAGE/manifest.json")
ARCH_NETWORK=$(jq -r .network "$STAGE/manifest.json")
log "archive $ID from $(jq -r .source_host "$STAGE/manifest.json"), $ARCH_NETWORK, lnd $(jq -r .lnd_pubkey "$STAGE/manifest.json")"
if grep -qx "$ID" "$MIGRATION/imported" 2>/dev/null; then
	die "archive $ID was imported here already"
fi

log "placing state"
tar -C "$ROOT" --numeric-owner -xf "$STAGE/srv/state.tar"
install -m 600 "$STAGE/deploy/.env" "$DEPLOY/.env"
if [ -f "$STAGE/rebalance-state.json" ]; then
	install -d -m 700 "$ROOT/rebalance"
	install -m 600 "$STAGE/rebalance-state.json" "$ROOT/rebalance/state.json"
fi
if [ -f "$STAGE/telemetry.db" ]; then
	install -d -m 700 "$ROOT/telemetry"
	install -m 600 "$STAGE/telemetry.db" "$ROOT/telemetry/telemetry.db"
fi
if [ -n "$PUBLIC_IP_NEW" ]; then
	sed -i "s/^PUBLIC_IP=.*/PUBLIC_IP=$PUBLIC_IP_NEW/" "$DEPLOY/.env"
fi
if [ -f "$STAGE/etc/letsencrypt.tar" ]; then
	tar -C /etc --numeric-owner -xf "$STAGE/etc/letsencrypt.tar"
fi
if [ -f "$STAGE/etc/lfswap-preview.conf" ]; then
	install -m 644 "$STAGE/etc/lfswap-preview.conf" /etc/nginx/lfswap-preview.conf
fi
if [ -f "$STAGE/etc/lfswap.htpasswd" ]; then
	install -o root -g www-data -m 640 "$STAGE/etc/lfswap.htpasswd" /etc/nginx/lfswap.htpasswd
fi
cp "$STAGE/snapshot.json" "$MIGRATION/snapshot-$ID-source.json"
cp "$STAGE/postgres.sql" "$MIGRATION/postgres-$ID.sql"
chmod 600 "$MIGRATION/postgres-$ID.sql"
echo "$ID" >> "$MIGRATION/imported"

log "rendering configuration for this host"
"$DEPLOY/scripts/setup.sh" >/dev/null

if [ -d "/etc/letsencrypt/live/${DOMAIN:-lightningfork.com}" ]; then
	"$DEPLOY/scripts/install-host.sh" nginx >/dev/null
	log "nginx site installed with the moved certificate"
else
	log "no certificate for ${DOMAIN:-lightningfork.com}: the nginx site is not installed (install-host.sh tls after DNS)"
fi

if [ "$START" = 0 ]; then
	log "state imported; not starting (--no-start)"
	exit 0
fi

set -a; . "$DEPLOY/.env"; set +a
log "starting Knots"
dc up -d knots
wait_for "Knots RPC" 120 knots getblockchaininfo
[ -d "$ROOT/$KNOTS_NET/blocks" ] || log "no chain was copied: Knots syncs from scratch"
knots -named loadwallet filename=boltz load_on_startup=true >/dev/null 2>&1 || true
log "starting the shim, Tor and lnd"
dc up -d shim tor lnd
wait_for "lnd" 120 sh -c "docker compose exec -T lnd lncli --network=$NETWORK getinfo </dev/null | grep -Eq '\"synced_to_chain\": +true'"
log "starting Postgres, the backend and the donation services"
dc up -d postgres
wait_for "Postgres" 60 sh -c "docker compose exec -T postgres pg_isready -U boltz </dev/null"
# Both only add what a source from before donations did not have
"$DEPLOY/scripts/init-donations-db.sh" >/dev/null
"$DEPLOY/scripts/bake-macaroon.sh"
dc up -d boltz donations-worker donations-api
wait_for "the backend" 60 curl -sf http://127.0.0.1:9001/version
wait_for "the donations API" 60 curl -sf http://127.0.0.1:9010/donate/v1/health

install_crons

log "comparing with the snapshot taken before the export"
if "$(dirname "$0")/verify.sh" compare "$MIGRATION/snapshot-$ID-source.json"; then
	log "import $ID complete: switch DNS to this host, then maintenance.sh off"
else
	log "import $ID: some checks differ (above); look before switching DNS"
	exit 1
fi
