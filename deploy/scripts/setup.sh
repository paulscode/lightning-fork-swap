#!/usr/bin/env bash
# Prepares a host for Lightning Fork Swap, or re-renders its configuration.
#
#   1. creates .env from .env.example with fresh secrets (only the first time)
#   2. creates the state directories under $LFSWAP_ROOT
#   3. renders bitcoin.conf, lnd.conf, torrc and boltz.conf from templates/
#
# Safe to run again: secrets already in .env are kept, and rendered files are
# rewritten from the templates.
set -euo pipefail
cd "$(dirname "$0")/.."

rand() { head -c 32 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 32; }

if [ ! -f .env ]; then
	cp .env.example .env
	chmod 600 .env
fi
for var in LND_RPCPASSWORD BOLTZ_RPCPASSWORD SHIM_RPCPASSWORD POSTGRES_PASSWORD TOR_CONTROL_PASSWORD \
	DONATIONS_WORKER_DBPASSWORD DONATIONS_API_DBPASSWORD; do
	if ! grep -q "^$var=" .env; then
		# A key the .env of an older install does not have
		echo "$var=$(rand)" >> .env
	elif ! grep -q "^$var=." .env; then
		sed -i "s|^$var=.*|$var=$(rand)|" .env
	fi
done

set -a
. ./.env
set +a
ROOT=${LFSWAP_ROOT:-/srv/lfswap}
NETWORK=${NETWORK:-mainnet}

# What differs between networks.
case "$NETWORK" in
mainnet)
	KNOTS_CHAIN_LINE=""
	KNOTS_SECTION=main
	KNOTS_NETWORK_EXTRA=""
	LND_NETWORK_EXTRA=""
	BOLTZ_CURRENCY_NETWORK=bitcoinMainnet
	;;
regtest)
	KNOTS_CHAIN_LINE="chain=regtest"
	KNOTS_SECTION=regtest
	KNOTS_NETWORK_EXTRA="testactivationheight=blake2b@20
blake2b_headline=Lightning Fork Swap regtest
rdtsexpiry=1819843200"
	LND_NETWORK_EXTRA="bitcoin.blake2b-activation-height=20"
	BOLTZ_CURRENCY_NETWORK=bitcoinRegtest
	;;
*)
	echo "NETWORK must be mainnet or regtest, not $NETWORK" >&2
	exit 1
	;;
esac
# A peer to connect to (staging and rehearsals: the node that mines).
if [ -n "${KNOTS_ADDNODE:-}" ]; then
	KNOTS_NETWORK_EXTRA="${KNOTS_NETWORK_EXTRA:+$KNOTS_NETWORK_EXTRA
}addnode=$KNOTS_ADDNODE"
fi
export NETWORK KNOTS_CHAIN_LINE KNOTS_SECTION KNOTS_NETWORK_EXTRA LND_NETWORK_EXTRA BOLTZ_CURRENCY_NETWORK

mkdir -p "$ROOT"/{knots/data,shim,tor/data,lnd,postgres,boltz,secrets,webapp,graph,graph-state}
chmod 700 "$ROOT/secrets"

# rpcauth lines, so the node stores only salted hashes of the passwords.
rpcauth() {
	python3 - "$1" "$2" <<'PY'
import hashlib, hmac, os, sys
user, password = sys.argv[1], sys.argv[2]
salt = os.urandom(16).hex()
digest = hmac.new(salt.encode(), password.encode(), hashlib.sha256).hexdigest()
print(f"rpcauth={user}:{salt}${digest}")
PY
}
RPCAUTH_LINES="$(rpcauth lnd "$LND_RPCPASSWORD")
$(rpcauth boltz "$BOLTZ_RPCPASSWORD")
$(rpcauth shim "$SHIM_RPCPASSWORD")"
export RPCAUTH_LINES

# Tor wants its control password hashed (its salted, iterated SHA-1, as
# tor --hash-password makes it). Here, not by a tor binary, so that the
# password is on no command line; it comes through the environment.
TOR_CONTROL_HASH=$(PW=$TOR_CONTROL_PASSWORD python3 - <<'PY'
import hashlib, os
salt, indicator = os.urandom(8), 0x60
count = (16 + (indicator & 15)) << ((indicator >> 4) + 6)
data = salt + os.environ["PW"].encode()
digest = hashlib.sha1()
while count > 0:
    chunk = data[:count]
    digest.update(chunk)
    count -= len(chunk)
print("16:" + (salt + bytes([indicator]) + digest.digest()).hex().upper())
PY
)
export TOR_CONTROL_HASH

render() {
	python3 - "$1" "$2" <<'PY'
import os, re, sys
src, dst = sys.argv[1], sys.argv[2]
text = open(src).read()
def sub(m):
    name = m.group(1)
    if name not in os.environ:
        sys.exit(f"{src}: ${{{name}}} is not set")
    return os.environ[name]
open(dst, "w").write(re.sub(r"\$\{([A-Z0-9_]+)\}", sub, text))
PY
}

render templates/bitcoin.conf "$ROOT/knots/data/bitcoin.conf"
render templates/lnd.conf "$ROOT/lnd/lnd.conf"
render templates/torrc "$ROOT/tor/torrc"
render templates/boltz.conf "$ROOT/boltz/boltz.conf"
chmod 600 "$ROOT/boltz/boltz.conf" "$ROOT/lnd/lnd.conf"

# The images run as these users.
chown -R 1000:1000 "$ROOT/knots"
TOR_UID=$(docker run --rm --entrypoint id lfswap/tor:dev -u)
chown -R "$TOR_UID" "$ROOT/tor/data"
chmod 700 "$ROOT/tor/data"
chown -R 65532:65532 "$ROOT/shim"
# The graph generator writes the sky's files (served by nginx) and its layout
chown -R 65532:65532 "$ROOT/graph" "$ROOT/graph-state"
chmod 755 "$ROOT/graph"

# The deployment files run as root (cron, these scripts) and the web app is
# served to every user: root's alone, whatever owner a copy kept (tar and
# rsync as root keep the owner of the machine they came from)
# (only where the runbook installs them, never a working copy elsewhere)
DEPLOY_ROOT=$(cd .. && pwd)
for d in "$DEPLOY_ROOT" "$ROOT/webapp"; do
	[ "$DEPLOY_ROOT" = /opt/lfswap ] && [ -d "$d" ] || continue
	chown -R root:root "$d"
	chmod -R go-w "$d"
done

echo "rendered configuration under $ROOT"
