#!/usr/bin/env bash
# Copies what is needed to recover the service's funds into
# $LFSWAP_ROOT/secrets, from where the operator takes it offline. Run hourly
# by /etc/cron.d/lfswap-backup; safe to run by hand.
#
#   lnd channel.backup   changes with every channel open or close; without a
#                        recent copy, channels cannot be recovered from the seed
#   backend seed.dat     the swap keys: needed to refund or claim swaps
set -euo pipefail
ROOT=${LFSWAP_ROOT:-/srv/lfswap}
S=$ROOT/secrets
umask 077

NETWORK=$(sed -n 's/^NETWORK=//p' /opt/lfswap/deploy/.env 2>/dev/null)
scb=$ROOT/lnd/data/chain/bitcoin/${NETWORK:-mainnet}/channel.backup
# Copied aside and renamed, so a full disk never leaves a cut-off copy in
# place of a good one
if [ -f "$scb" ] && ! cmp -s "$scb" "$S/channel.backup"; then
	cp "$scb" "$S/.channel.backup.tmp"
	cp "$S/.channel.backup.tmp" "$S/channel.backup.$(date -u +%Y%m%dT%H%M%SZ)"
	mv "$S/.channel.backup.tmp" "$S/channel.backup"
	# keep the last 20 versions
	ls -1t "$S"/channel.backup.2* 2>/dev/null | tail -n +21 | xargs -r rm -f
fi
if [ -f "$ROOT/boltz/seed.dat" ] && ! cmp -s "$ROOT/boltz/seed.dat" "$S/boltz-seed.dat"; then
	cp "$ROOT/boltz/seed.dat" "$S/.boltz-seed.dat.tmp"
	mv "$S/.boltz-seed.dat.tmp" "$S/boltz-seed.dat"
fi
