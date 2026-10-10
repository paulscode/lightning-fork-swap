#!/usr/bin/env bash
# Tests scripts/channel-donations.sh with stand-ins for docker compose,
# curl and the macaroon baker, in a throwaway copy of deploy/.
#   deploy/scripts/test/channel-donations-test.sh
set -euo pipefail
here=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/deploy/scripts" "$work/bin" "$work/root/lnd"
cp "$here/channel-donations.sh" "$work/deploy/scripts/"
printf '#!/bin/sh\necho baked >> "%s/calls"\n' "$work" > "$work/deploy/scripts/bake-macaroon.sh"
# docker compose: logs say registered when $work/registered exists
cat > "$work/bin/compose" <<STUB
#!/bin/sh
echo "compose \$*" >> "$work/calls"
case "\$1" in logs) [ -f "$work/registered" ] && echo "registered with lnd for the caveat";; esac
exit 0
STUB
cat > "$work/bin/curl" <<STUB
#!/bin/sh
[ -f "$work/available" ] && echo '{"available":true}' && exit 0
exit 22
STUB
chmod +x "$work/bin/"* "$work/deploy/scripts/"*.sh
export PATH="$work/bin:$PATH" COMPOSE="$work/bin/compose" WAIT=2 LFSWAP_ROOT="$work/root"
run() { "$work/deploy/scripts/channel-donations.sh" "$@"; }
env() { cat "$work/deploy/.env"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

printf 'NETWORK=mainnet\nCOMPOSE_PROFILES=extra\n' > "$work/deploy/.env"

# On: both keys, the services, the API recreated
touch "$work/registered" "$work/available"
out=$(run on)
grep -q "channel donations on" <<<"$out" || fail "on"
env | grep -qx "CHANNEL_DONATIONS=on" || fail "CHANNEL_DONATIONS after on: $(env)"
env | grep -qx "COMPOSE_PROFILES=extra,channels" || fail "profiles after on: $(env)"
grep -q "compose up -d donations-guard donations-channels" "$work/calls" || fail "not started"
grep -q "compose up -d --force-recreate donations-api" "$work/calls" || fail "API not recreated"
grep -q baked "$work/calls" || fail "not baked"
# On twice: no duplicate profile
run on >/dev/null
env | grep -qx "COMPOSE_PROFILES=extra,channels" || fail "profiles doubled: $(env)"

# Status
echo 'rpcmiddleware.enable=true' > "$work/root/lnd/lnd.conf"
out=$(run status)
grep -q "guard: registered" <<<"$out" || fail "status: $out"
grep -q "middleware chain on" <<<"$out" || fail "status lnd.conf: $out"

# Off: keys back, services stopped, other profiles kept
: > "$work/calls"
out=$(run off)
grep -q "channel donations off" <<<"$out" || fail "off"
env | grep -qx "CHANNEL_DONATIONS=off" || fail "after off: $(env)"
env | grep -qx "COMPOSE_PROFILES=extra" || fail "profiles after off: $(env)"
grep -q "compose stop donations-channels donations-guard" "$work/calls" || fail "not stopped"

# On, but the guard never registers: turned off again, an error
rm -f "$work/registered"
if run on 2>/dev/null; then fail "on succeeded without the guard"; fi
env | grep -qx "CHANNEL_DONATIONS=off" || fail "left on: $(env)"
env | grep -qx "COMPOSE_PROFILES=extra" || fail "profile left: $(env)"

# A .env without the keys gets them; usage
printf 'NETWORK=mainnet\n' > "$work/deploy/.env"
run off >/dev/null
env | grep -qx "CHANNEL_DONATIONS=off" || fail "key not added: $(env)"
env | grep -qx "COMPOSE_PROFILES=" || fail "empty profiles: $(env)"
if run nonsense 2>/dev/null; then fail "usage"; fi
echo "channel-donations.sh: all passed"
