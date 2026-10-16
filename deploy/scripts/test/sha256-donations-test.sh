#!/usr/bin/env bash
# Tests scripts/sha256-donations.sh with stand-ins for docker compose and
# curl, in a throwaway copy of deploy/.
#   deploy/scripts/test/sha256-donations-test.sh
set -euo pipefail
here=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/deploy/scripts" "$work/bin" "$work/root/knots/data" "$work/root/secrets"
cp "$here/sha256-donations.sh" "$work/deploy/scripts/"
# docker compose: the worker's log says what $work/log holds
cat > "$work/bin/compose" <<STUB
#!/bin/sh
echo "compose \$*" >> "$work/calls"
case "\$1" in
logs) [ -f "$work/log" ] && cat "$work/log";;
ps) [ -f "$work/running" ] && echo 1234abcd;;
esac
exit 0
STUB
cat > "$work/bin/curl" <<STUB
#!/bin/sh
[ -f "$work/available" ] && echo '{"available":true}' && exit 0
exit 22
STUB
chmod +x "$work/bin/"* "$work/deploy/scripts/"*.sh
export PATH="$work/bin:$PATH" COMPOSE="$work/bin/compose" WAIT=2 LFSWAP_ROOT="$work/root"
run() { "$work/deploy/scripts/sha256-donations.sh" "$@"; }
env() { cat "$work/deploy/.env"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

printf 'NETWORK=mainnet\nCOMPOSE_PROFILES=channels\n' > "$work/deploy/.env"

# On: the key, the profile, the worker started, the API recreated
echo "2026/10/16 00:00:00 SHA256 donations: running (coins without a return address to bc1p)" > "$work/log"
touch "$work/available" "$work/running"
out=$(run on)
grep -q "SHA256 donations on" <<<"$out" || fail "on"
env | grep -qx "SHA256_DONATIONS=on" || fail "SHA256_DONATIONS after on: $(env)"
env | grep -qx "COMPOSE_PROFILES=channels,sha256" || fail "profiles after on: $(env)"
grep -q "compose up -d donations-sha256" "$work/calls" || fail "not started"
grep -q "compose up -d --force-recreate donations-api" "$work/calls" || fail "API not recreated"
run on >/dev/null
env | grep -qx "COMPOSE_PROFILES=channels,sha256" || fail "profiles doubled: $(env)"

# A failed pass is not an exit
echo "2026/10/16 00:01:00 sha256 donations: request x: SHA256 explorer: down" >> "$work/log"
grep -q "worker: running" <<<"$(run status)" || fail "a failed pass counted as an exit"

# Status
echo 'rpcwhitelist=donations:getblockcount' > "$work/root/knots/data/bitcoin.conf"
echo 00 > "$work/root/secrets/sha256-donations.seed"
out=$(run status)
grep -q "donations RPC user is there" <<<"$out" || fail "status bitcoin.conf: $out"
grep -q "seed: present" <<<"$out" || fail "status seed: $out"
grep -q "API: SHA256 donations available" <<<"$out" || fail "status API: $out"

# Off: the key, the profile, the worker stopped, the API recreated
: > "$work/calls"
run off >/dev/null
env | grep -qx "SHA256_DONATIONS=off" || fail "after off: $(env)"
env | grep -qx "COMPOSE_PROFILES=channels" || fail "profiles after off: $(env)"
grep -q "compose stop donations-sha256" "$work/calls" || fail "not stopped"

# On when the worker exits (Knots refuses its login): turned off again
echo "donations: our Knots node: knots: Unauthorized (401)" > "$work/log"
if run on >/dev/null 2>&1; then fail "on with a worker that exited"; fi
env | grep -qx "SHA256_DONATIONS=off" || fail "not turned off again: $(env)"
env | grep -qx "COMPOSE_PROFILES=channels" || fail "profile left on: $(env)"

echo "sha256-donations: all tests passed"
