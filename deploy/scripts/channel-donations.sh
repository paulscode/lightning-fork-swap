#!/usr/bin/env bash
# Turns channel donations on or off (off by default): a donation that asks
# for a channel from our node to the donor's (OPERATING.md §8).
#
#   channel-donations.sh status
#   channel-donations.sh on     bakes any missing macaroon, starts the guard
#                               and the worker, adds the routes to the API,
#                               and checks the guard registered with lnd
#   channel-donations.sh off    stops them and takes the routes away; orders
#                               in progress stay in the database
#
# It sets CHANNEL_DONATIONS and COMPOSE_PROFILES in .env, which the monitor
# also reads. lnd must run with rpcmiddleware.enable=true (setup.sh renders
# it; lnd reads it when it starts).
set -euo pipefail
cd "$(dirname "$0")/.."
COMPOSE=${COMPOSE:-docker compose}
API=${DONATIONS_API:-http://127.0.0.1:9010}
WAIT=${WAIT:-30}

die() { echo "channel-donations: $*" >&2; exit 1; }
[ -f .env ] || die "no .env (run scripts/setup.sh)"

value() { sed -n "s/^$1=//p" .env | tail -1; }

# set KEY VALUE: replaces the key's line, or appends one
set_env() {
	if grep -q "^$1=" .env; then
		sed -i "s|^$1=.*|$1=$2|" .env
	else
		echo "$1=$2" >> .env
	fi
}

# The profiles without "channels", then with it if wanted
profiles() {
	local want=$1 out=() p
	IFS=',' read -r -a list <<<"$(value COMPOSE_PROFILES)"
	for p in "${list[@]}"; do
		p=${p// /}
		[ -n "$p" ] && [ "$p" != channels ] && out+=("$p")
	done
	[ "$want" = on ] && out+=(channels)
	local IFS=,
	echo "${out[*]}"
}

registered() { $COMPOSE logs --no-color donations-guard 2>&1 | grep -q "registered with lnd"; }
available() { curl -sf "$API/donate/v1/info" 2>/dev/null | grep -q '"available":true'; }

status() {
	echo "CHANNEL_DONATIONS=$(value CHANNEL_DONATIONS)"
	echo "COMPOSE_PROFILES=$(value COMPOSE_PROFILES)"
	local root=${LFSWAP_ROOT:-$(value LFSWAP_ROOT)}
	root=${root:-/srv/lfswap}
	if grep -q '^rpcmiddleware.enable=true' "$root/lnd/lnd.conf" 2>/dev/null; then
		echo "lnd.conf: middleware chain on (lnd reads it when it starts)"
	else
		echo "lnd.conf: middleware chain off (run scripts/setup.sh, then restart lnd)"
	fi
	if registered; then echo "guard: registered with lnd"; else echo "guard: not registered"; fi
	if available; then echo "API: channel donations available"; else echo "API: channel donations not offered"; fi
}

case "${1:-}" in
status)
	status
	;;
on)
	./scripts/bake-macaroon.sh
	set_env CHANNEL_DONATIONS on
	set_env COMPOSE_PROFILES "$(profiles on)"
	$COMPOSE up -d donations-guard donations-channels
	for _ in $(seq 1 "$WAIT"); do registered && break; sleep 1; done
	if ! registered; then
		$COMPOSE logs --no-color --tail 5 donations-guard >&2 || true
		# Nothing is offered while the guard is not there: undo
		"$0" off >/dev/null
		die "the guard did not register with lnd: is lnd running with rpcmiddleware.enable=true? (setup.sh, then restart lnd); turned off again"
	fi
	# The API reads CHANNEL_DONATIONS when it starts
	$COMPOSE up -d --force-recreate donations-api
	for _ in $(seq 1 "$WAIT"); do available && break; sleep 1; done
	available || echo "channel-donations: on, but the API does not offer them yet (the worker's first pass?)" >&2
	echo "channel donations on"
	;;
off)
	set_env CHANNEL_DONATIONS off
	set_env COMPOSE_PROFILES "$(profiles off)"
	$COMPOSE stop donations-channels donations-guard >/dev/null 2>&1 || true
	$COMPOSE up -d --force-recreate donations-api
	echo "channel donations off"
	;;
*)
	echo "usage: $0 status|on|off" >&2
	exit 2
	;;
esac
