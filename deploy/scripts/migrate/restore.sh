#!/usr/bin/env bash
# restore.sh BACKUP --identity OPERATOR_AGE_KEY [--public-ip IP]
#
# Brings the service back on a provisioned host from a backup made by
# backup-offsite.sh, when the old host is lost and no clean export exists.
# The operator's private age key is needed only here, and only for the run.
#
#   - Postgres, the backend seed and the Knots wallet come back as they were
#     at the backup; swaps made after it are not in it (the operator can
#     still find their on-chain side, and users can refund).
#   - lnd comes back from its seed and channel.backup: the same node, same
#     on-chain wallet, but every channel is closed by force through its peer
#     and the funds return on chain after the channel's delay. There is no
#     safe way to resume channels without the live database.
#
# The chain: Knots needs a synced chain before anything else is useful. Copy
# a pruned chain from a node you trust (copy-chain.sh from a standby) before
# running this, or let it sync from peers, which takes weeks on a small host.
. "$(dirname "$0")/lib.sh"

BACKUP=${1:-}
[ -f "$BACKUP" ] || die "usage: $0 BACKUP --identity KEY [--public-ip IP]"
shift
IDENTITY=""
PUBLIC_IP_NEW=""
while [ $# -gt 0 ]; do
	case "$1" in
	--identity) IDENTITY=$2; shift ;;
	--public-ip) PUBLIC_IP_NEW=$2; shift ;;
	*) die "unknown argument $1" ;;
	esac
	shift
done
[ -f "$IDENTITY" ] || die "--identity must be the operator's age key file"
refuse_if_migrated
if [ -e "$ROOT/lnd/data" ] || [ -n "$(ls -A "$ROOT/postgres" 2>/dev/null)" ]; then
	die "$ROOT already holds lnd or Postgres data; refusing to overwrite it"
fi

STAGE=$(mktemp -d "$MIGRATION/restore.XXXXXX")
trap 'rm -rf "$STAGE"' EXIT
age -d -i "$IDENTITY" "$BACKUP" | gunzip | tar -C "$STAGE" -xf -
(cd "$STAGE" && sha256sum --quiet -c SHA256SUMS) || die "backup contents do not match their checksums"
log "backup of $(jq -r '.host + " at " + .at' "$STAGE/manifest.json"), lnd $(jq -r .lnd_pubkey "$STAGE/manifest.json")"

install -m 600 "$STAGE/env" "$DEPLOY/.env"
[ -n "$PUBLIC_IP_NEW" ] && sed -i "s/^PUBLIC_IP=.*/PUBLIC_IP=$PUBLIC_IP_NEW/" "$DEPLOY/.env"
mkdir -p "$ROOT/boltz" "$ROOT/secrets" "$ROOT/lnd"
install -m 600 "$STAGE/boltz-seed.dat" "$ROOT/boltz/seed.dat"
cp -a "$STAGE/secrets/." "$ROOT/secrets/"
if [ -f "$STAGE/telemetry.db" ]; then
	install -d -m 700 "$ROOT/telemetry"
	install -m 600 "$STAGE/telemetry.db" "$ROOT/telemetry/telemetry.db"
fi
# Once the operator has taken the seeds offline they are not in the backup:
# they go back into secrets/ from the offline copy before a restore
for f in lnd-seed.txt lnd-wallet-password.txt; do
	[ -s "$ROOT/secrets/$f" ] ||
		die "$ROOT/secrets/$f is missing (taken offline): copy it there from the offline backup, then run this again"
done
install -m 600 "$ROOT/secrets/lnd-wallet-password.txt" "$ROOT/lnd/wallet-password"
[ -f "$STAGE/letsencrypt.tar" ] && tar -C /etc -xf "$STAGE/letsencrypt.tar"
[ -f "$STAGE/lfswap.htpasswd" ] && install -o root -g www-data -m 640 "$STAGE/lfswap.htpasswd" /etc/nginx/lfswap.htpasswd
[ -f "$STAGE/lfswap-preview.conf" ] && install -m 644 "$STAGE/lfswap-preview.conf" /etc/nginx/lfswap-preview.conf
"$DEPLOY/scripts/setup.sh" >/dev/null
set -a; . "$DEPLOY/.env"; set +a

log "Knots, and its wallet from the backup"
dc up -d knots
wait_for "Knots RPC" 120 knots getblockchaininfo
install -o 1000 -g 1000 -m 600 "$STAGE/knots-boltz-wallet.dat" "$ROOT/knots/data/lfswap-restore-wallet.dat"
knots -named restorewallet wallet_name=boltz backup_file=/data/lfswap-restore-wallet.dat load_on_startup=true >/dev/null
rm -f "$ROOT/knots/data/lfswap-restore-wallet.dat"

log "Postgres, from the dump"
dc up -d postgres
wait_for "Postgres" 60 sh -c "docker compose exec -T postgres pg_isready -U boltz </dev/null"
# The dump recreates the role and the database the container already has,
# so some statements fail by design: judge the restore by what it restored
dc exec -T postgres psql -q -U boltz -d postgres -v ON_ERROR_STOP=0 < "$STAGE/postgres.sql" > "$STAGE/postgres-restore.log" 2>&1 || true
SUBMARINE=$(psql_q 'select count(*) from swaps')
REVERSE=$(psql_q 'select count(*) from "reverseSwaps"')
log "restored $SUBMARINE submarine and $REVERSE reverse swaps"
if jq -e 'has("submarine_swaps")' "$STAGE/manifest.json" >/dev/null; then
	[ "$SUBMARINE" -ge "$(jq .submarine_swaps "$STAGE/manifest.json")" ] &&
		[ "$REVERSE" -ge "$(jq .reverse_swaps "$STAGE/manifest.json")" ] ||
		die "the backup holds $(jq -r '"\(.submarine_swaps) submarine and \(.reverse_swaps) reverse"' "$STAGE/manifest.json") swaps; see $STAGE/postgres-restore.log"
else
	log "this backup predates swap counts in its manifest: check the numbers above"
fi
# Swaps made after the backup used key indexes the restored database does
# not know; new swaps must not use them again
psql_q 'update keys set "highestUsedIndex" = "highestUsedIndex" + 1000' >/dev/null
# The donations database came with the dump (a backup from before it
# existed gets an empty one); its roles get the passwords of this .env
"$DEPLOY/scripts/init-donations-db.sh" >/dev/null

log "lnd, from its seed and channel.backup"
dc up -d shim tor lnd
wait_for "lnd to ask for a wallet" 60 curl -skf https://127.0.0.1:8080/v1/genseed
# The password, seed and channel backup go from files and stdin, never on a
# command line, where other users of the host would see them
sed -E 's/^[0-9]+\. //' "$ROOT/secrets/lnd-seed.txt" | jq -R . | jq -sc \
	--rawfile pw <(base64 -w0 < "$ROOT/lnd/wallet-password") \
	--rawfile scb <(base64 -w0 < "$STAGE/channel.backup") \
	'{wallet_password: $pw, cipher_seed_mnemonic: ., recovery_window: 2500,
	  channel_backups: {multi_chan_backup: {multi_chan_backup: $scb}}}' |
	curl -skf -X POST https://127.0.0.1:8080/v1/initwallet --data-binary @- >/dev/null
wait_for "lnd" 120 sh -c "docker compose exec -T lnd lncli --network=$NETWORK getinfo </dev/null | grep -Eq '\"synced_to_chain\": +true'"
[ "$(lncli getinfo | jq -r .identity_pubkey)" = "$(jq -r .lnd_pubkey "$STAGE/manifest.json")" ] || die "lnd came back with a different identity"

log "the backend and the donation services, with macaroons of the restored lnd"
"$DEPLOY/scripts/bake-macaroon.sh" --force
dc up -d boltz donations-worker donations-api
wait_for "the backend" 60 curl -sf http://127.0.0.1:9001/version
wait_for "the donations API" 60 curl -sf http://127.0.0.1:9010/donate/v1/health
install_crons

cat <<MSG

Restored. lnd is the same node; its channels are being closed by force
through their peers (lncli pendingchannels), and their funds return to the
on-chain wallet after each channel's delay. Open new channels once they do.
Then: DNS to this host, install-host.sh nginx (or tls), maintenance.sh off.
MSG
