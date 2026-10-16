#!/usr/bin/env bash
# Turns SHA256-chain donations on or off (off by default): an API through
# which anyone donates coins on the SHA256 chain; what reaches the request's
# address on the BLAKE2b chain is passed on (OPERATING.md §8b).
#
#   sha256-donations.sh status
#   sha256-donations.sh on      starts the worker, adds the routes to the
#                               API, and checks the worker reached Knots
#   sha256-donations.sh off     stops it and takes the routes away; requests
#                               and coins stay (the seed holds them)
#
# It sets SHA256_DONATIONS and COMPOSE_PROFILES in .env. Knots must know the
# donations RPC user (setup.sh renders it; Knots reads it when it starts).
set -euo pipefail
cd "$(dirname "$0")/.."
COMPOSE=${COMPOSE:-docker compose}
API=${DONATIONS_API:-http://127.0.0.1:9010}
WAIT=${WAIT:-30}

die() { echo "sha256-donations: $*" >&2; exit 1; }
[ -f .env ] || die "no .env (run scripts/setup.sh)"

value() { sed -n "s/^$1=//p" .env | tail -1; }

set_env() {
	if grep -q "^$1=" .env; then
		sed -i "s|^$1=.*|$1=$2|" .env
	else
		echo "$1=$2" >> .env
	fi
}

# The profiles without "sha256", then with it if wanted
profiles() {
	local want=$1 out=() p
	IFS=',' read -r -a list <<<"$(value COMPOSE_PROFILES)"
	for p in "${list[@]}"; do
		p=${p// /}
		[ -n "$p" ] && [ "$p" != sha256 ] && out+=("$p")
	done
	[ "$want" = on ] && out+=(sha256)
	local IFS=,
	echo "${out[*]}"
}

# The worker runs and, since it last started, did not exit: it exits with a
# line starting "donations: " when it cannot reach Knots or read its seed
running() {
	[ -n "$($COMPOSE ps -q --status running donations-sha256 2>/dev/null)" ] || return 1
	$COMPOSE logs --no-color --no-log-prefix donations-sha256 2>&1 |
		grep -E "SHA256 donations: running|^donations: " | tail -1 | grep -q "SHA256 donations: running"
}
available() { curl -sf "$API/donate/v1/sha256/info" 2>/dev/null | grep -q '"available":true'; }

status() {
	echo "SHA256_DONATIONS=$(value SHA256_DONATIONS)"
	echo "COMPOSE_PROFILES=$(value COMPOSE_PROFILES)"
	local root=${LFSWAP_ROOT:-$(value LFSWAP_ROOT)}
	root=${root:-/srv/lfswap}
	if grep -q '^rpcwhitelist=donations:' "$root/knots/data/bitcoin.conf" 2>/dev/null; then
		echo "bitcoin.conf: the donations RPC user is there (Knots reads it when it starts)"
	else
		echo "bitcoin.conf: no donations RPC user (run scripts/setup.sh, then restart knots)"
	fi
	if [ -s "$root/secrets/sha256-donations.seed" ]; then echo "seed: present"; else echo "seed: missing (run scripts/setup.sh)"; fi
	if running; then echo "worker: running"; else echo "worker: not running"; fi
	if available; then echo "API: SHA256 donations available"; else echo "API: SHA256 donations not offered"; fi
}

case "${1:-}" in
status)
	status
	;;
on)
	set_env SHA256_DONATIONS on
	set_env COMPOSE_PROFILES "$(profiles on)"
	$COMPOSE up -d donations-sha256
	for _ in $(seq 1 "$WAIT"); do running && break; sleep 1; done
	if ! running; then
		$COMPOSE logs --no-color --tail 5 donations-sha256 >&2 || true
		"$0" off >/dev/null
		die "the worker did not start: does Knots know the donations RPC user? (setup.sh, then restart knots); turned off again"
	fi
	# The API reads SHA256_DONATIONS when it starts
	$COMPOSE up -d --force-recreate donations-api
	for _ in $(seq 1 "$WAIT"); do available && break; sleep 1; done
	available || echo "sha256-donations: on, but the API does not offer them yet (the worker's first pass?)" >&2
	echo "SHA256 donations on"
	;;
off)
	set_env SHA256_DONATIONS off
	set_env COMPOSE_PROFILES "$(profiles off)"
	$COMPOSE stop donations-sha256 >/dev/null 2>&1 || true
	$COMPOSE up -d --force-recreate donations-api
	echo "SHA256 donations off"
	;;
*)
	echo "usage: $0 status|on|off" >&2
	exit 2
	;;
esac
