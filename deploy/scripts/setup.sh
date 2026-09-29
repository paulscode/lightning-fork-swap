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
for var in LND_RPCPASSWORD BOLTZ_RPCPASSWORD SHIM_RPCPASSWORD POSTGRES_PASSWORD TOR_CONTROL_PASSWORD; do
	if ! grep -q "^$var=." .env; then
		sed -i "s|^$var=.*|$var=$(rand)|" .env
	fi
done

set -a
. ./.env
set +a
ROOT=${LFSWAP_ROOT:-/srv/lfswap}

mkdir -p "$ROOT"/{knots/data,shim,tor/data,lnd,postgres,boltz,secrets,webapp}
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

# Tor wants its control password hashed; ask a tor binary for the hash.
TOR_CONTROL_HASH=$(docker run --rm --entrypoint tor lfswap/tor:dev --hash-password "$TOR_CONTROL_PASSWORD" | tail -1)
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

echo "rendered configuration under $ROOT"
