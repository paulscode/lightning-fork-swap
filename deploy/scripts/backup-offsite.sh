#!/usr/bin/env bash
# backup-offsite.sh [--dest user@host:/path | --dest /local/path]
#
# A daily backup that needs nothing stopped, encrypted to the operator's age
# public key (BACKUP_AGE_RECIPIENT in .env; the private key stays offline, so
# this server can write backups it cannot read), kept 30 days locally in
# $LFSWAP_ROOT/backups and copied to --dest (or BACKUP_DEST in .env).
#
# Contains what recovers the service after the host is lost:
#   Postgres dump             every swap, for claims and refunds
#   backend seed.dat          the swap keys
#   Knots wallet backup       the hot wallet, as a wallet file (a pruned node
#                             cannot rescan old blocks for descriptors)
#   lnd channel.backup        channels, recovered by force close
#   lnd seed + password, .env, TLS certificate, preview password
# Not the live channel.db: restoring a copy of it can lose the channels'
# funds. scripts/migrate/restore.sh uses this archive.
. "$(dirname "$0")/migrate/lib.sh"
DEST=${BACKUP_DEST:-}
while [ $# -gt 0 ]; do
	case "$1" in
	--dest) DEST=$2; shift ;;
	*) die "unknown argument $1" ;;
	esac
	shift
done
refuse_if_migrated
[ -n "${BACKUP_AGE_RECIPIENT:-}" ] || die "set BACKUP_AGE_RECIPIENT (an age public key) in .env"

STAMP=$(date -u +%Y%m%dT%H%M%SZ)
OUT=$ROOT/backups
mkdir -p "$OUT"
chmod 700 "$OUT"
STAGE=$(mktemp -d "$MIGRATION/backup.XXXXXX")
trap 'rm -rf "$STAGE"' EXIT
mkdir -p "$STAGE/payload"
P=$STAGE/payload

# Counted before the dump, so the dump holds at least these; restore.sh
# checks what it restored against them
SUBMARINE=$(psql_q 'select count(*) from swaps')
REVERSE=$(psql_q 'select count(*) from "reverseSwaps"')
dc exec -T postgres pg_dumpall -U boltz > "$P/postgres.sql" </dev/null
cp "$ROOT/boltz/seed.dat" "$P/boltz-seed.dat"
knots -rpcwallet=boltz backupwallet /data/lfswap-wallet-backup.dat
mv "$ROOT/knots/data/lfswap-wallet-backup.dat" "$P/knots-boltz-wallet.dat"
knots -rpcwallet=boltz listdescriptors true > "$P/knots-boltz-descriptors.json"
cp "$ROOT/lnd/data/chain/bitcoin/$NETWORK/channel.backup" "$P/channel.backup"
mkdir -p "$P/secrets" && cp -a "$ROOT/secrets/." "$P/secrets/"
cp "$DEPLOY/.env" "$P/env"
# Telemetry (history for sizing decisions): a consistent copy while the
# monitor may be writing
if [ -f "$ROOT/telemetry/telemetry.db" ]; then
	python3 -c 'import sqlite3, sys; src = sqlite3.connect(sys.argv[1]); dst = sqlite3.connect(sys.argv[2]); src.backup(dst); dst.close()' \
		"$ROOT/telemetry/telemetry.db" "$P/telemetry.db"
fi
[ -d /etc/letsencrypt ] && tar -C /etc -cf "$P/letsencrypt.tar" letsencrypt
[ -f /etc/nginx/lfswap.htpasswd ] && cp /etc/nginx/lfswap.htpasswd "$P/"
[ -f /etc/nginx/lfswap-preview.conf ] && cp /etc/nginx/lfswap-preview.conf "$P/"
jq -n --arg at "$(date -u +%FT%TZ)" --arg host "$(hostname)" --arg network "$NETWORK" \
	--arg pubkey "$(lncli getinfo | jq -r .identity_pubkey)" \
	--argjson submarine "$SUBMARINE" --argjson reverse "$REVERSE" \
	'{kind: "backup", at: $at, host: $host, network: $network, lnd_pubkey: $pubkey,
	  submarine_swaps: $submarine, reverse_swaps: $reverse}' > "$P/manifest.json"
(cd "$P" && find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS)

FILE=$OUT/lfswap-backup-$STAMP.tar.gz.age
# Written aside and renamed once whole: a failed run leaves no file that
# looks like a backup
tar -C "$P" -cf - . | gzip -6 | age -r "$BACKUP_AGE_RECIPIENT" -o "$FILE.tmp"
mv "$FILE.tmp" "$FILE"
sha256sum "$FILE" | cut -d' ' -f1 > "$FILE.sha256"
log "backup $FILE ($(du -h "$FILE" | cut -f1))"

find "$OUT" -name 'lfswap-backup-*' -mtime +30 -delete

if [ -n "$DEST" ]; then
	case "$DEST" in
	*:*) scp -q -o BatchMode=yes "$FILE" "$FILE.sha256" "$DEST/" ;;
	*) mkdir -p "$DEST" && cp "$FILE" "$FILE.sha256" "$DEST/" ;;
	esac
	log "copied to $DEST"
fi
