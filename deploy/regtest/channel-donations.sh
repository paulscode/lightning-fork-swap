#!/usr/bin/env bash
# Channel donations end to end on the regtest service (deploy/regtest),
# with the real guard in lnd's middleware chain: an order through the API,
# a payment, the channel opened from lnd-swap to lnd-user from exactly the
# donated coins; then what the guard must refuse, called with the worker's
# own macaroon, and lnd refusing that macaroon while the guard is away.
#
#   deploy/regtest/channel-donations.sh        (the regtest service up)
#
# Starts three containers (lfswap-cd-*) on the regtest network and a
# donations database in the regtest Postgres; removes the containers at the
# end. lnd-swap gets --rpcmiddleware.enable (docker-compose.yml).
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
cd "$here"
NET=lfswap-regtest_default
IMAGE=${IMAGE:-lfswap/donations:dev}
work=$(mktemp -d)
cleanup() {
	docker rm -f lfswap-cd-guard lfswap-cd-channels lfswap-cd-api >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap '[ -n "${KEEP:-}" ] || cleanup' EXIT
log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok() { printf '  ok  %s\n' "$*"; }
fail() { printf '  FAIL %s\n' "$*" >&2; exit 1; }
K() { docker compose exec -T knots bitcoin-cli -regtest -rpcuser=lab -rpcpassword=lab "$@"; }
LS() { docker compose exec -T lnd-swap lncli --network=regtest "$@"; }
LU() { docker compose exec -T lnd-user lncli --network=regtest "$@"; }
mine() { K generatetoaddress "$1" "$(K -rpcwallet=boltz getnewaddress)" >/dev/null; }
psql_root() { docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -q -U boltz -d "${1:-postgres}"; }

log "the donations image"
docker build -q -t "$IMAGE" ../../donations >/dev/null
ok "built"

log "lnd-swap with its middleware chain on"
docker compose up -d lnd-swap lnd-user >/dev/null 2>&1
for _ in $(seq 1 60); do LS getinfo >/dev/null 2>&1 && break; sleep 2; done
LS getinfo | jq -e '.synced_to_chain' >/dev/null || mine 1
ok "lnd-swap up"

log "macaroons"
CHANNELS_URIS=(
	uri:/lnrpc.Lightning/GetInfo uri:/lnrpc.Lightning/GetNodeInfo uri:/lnrpc.Lightning/ListPeers
	uri:/lnrpc.Lightning/ListChannels uri:/lnrpc.Lightning/PendingChannels
	uri:/lnrpc.Lightning/ClosedChannels uri:/lnrpc.Lightning/GetTransactions
	uri:/walletrpc.WalletKit/ListUnspent uri:/walletrpc.WalletKit/ListLeases
	uri:/walletrpc.WalletKit/EstimateFee uri:/lnrpc.Lightning/NewAddress
	uri:/walletrpc.WalletKit/LeaseOutput uri:/walletrpc.WalletKit/ReleaseOutput
	uri:/lnrpc.Lightning/ConnectPeer uri:/lnrpc.Lightning/OpenChannelSync
	# Not used by the worker: to show the guard refuses it
	uri:/lnrpc.Lightning/SendCoins
)
LS bakemacaroon --custom_caveat_name lfswap-donations --custom_caveat_condition v1 \
	--save_to /root/.lnd/channels.macaroon "${CHANNELS_URIS[@]}" >/dev/null
LS bakemacaroon --save_to /root/.lnd/guard.macaroon \
	uri:/lnrpc.Lightning/RegisterRPCMiddleware uri:/walletrpc.WalletKit/ListLeases >/dev/null
for f in tls.cert channels.macaroon guard.macaroon; do
	docker compose cp "lnd-swap:/root/.lnd/$f" "$work/$f" >/dev/null 2>&1
done
chmod 755 "$work"; chmod 644 "$work"/*
ok "channels.macaroon (custom caveat) and guard.macaroon"

log "the donations database"
W=$(head -c 18 /dev/urandom | base64 | tr -dc A-Za-z0-9); A=$(head -c 18 /dev/urandom | base64 | tr -dc A-Za-z0-9)
psql_root <<SQL
SELECT 'CREATE ROLE donations_worker LOGIN' WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'donations_worker') \gexec
SELECT 'CREATE ROLE donations_api LOGIN' WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'donations_api') \gexec
ALTER ROLE donations_worker PASSWORD '$W';
ALTER ROLE donations_api PASSWORD '$A';
DROP DATABASE IF EXISTS donations WITH (FORCE);
CREATE DATABASE donations OWNER donations_worker;
SQL
psql_root donations <<'SQL'
GRANT USAGE ON SCHEMA public TO donations_api;
ALTER DEFAULT PRIVILEGES FOR ROLE donations_worker IN SCHEMA public GRANT SELECT ON TABLES TO donations_api;
SQL
ok "fresh"

log "guard, worker and API"
common=(--network "$NET" -v "$work:/lnd:ro" -e LND_TLS_CERT=/lnd/tls.cert
	-e LND_REST_URL=https://lnd-swap:8080 -e LND_GRPC=lnd-swap:10009)
docker run -d --name lfswap-cd-guard "${common[@]}" -e GUARD_MACAROON=/lnd/guard.macaroon \
	"$IMAGE" guard >/dev/null
registered() { docker logs lfswap-cd-guard 2>&1 | grep -q "registered with lnd"; }
for _ in $(seq 1 30); do registered && break; sleep 1; done
registered || { docker logs lfswap-cd-guard; fail "the guard did not register"; }
ok "the guard registered with lnd"
docker run -d --name lfswap-cd-channels "${common[@]}" -e CHANNELS_MACAROON=/lnd/channels.macaroon \
	-e DONATIONS_DB_URL="postgres://donations_worker:$W@postgres:5432/donations?sslmode=disable" \
	"$IMAGE" channels >/dev/null
docker run -d --name lfswap-cd-api --network "$NET" -p 127.0.0.1:59010:9010 \
	-e CHANNEL_DONATIONS=on -e CHANNEL_POW_BITS=12 \
	-e DONATIONS_DB_URL="postgres://donations_api:$A@postgres:5432/donations?sslmode=disable" \
	"$IMAGE" api >/dev/null
for _ in $(seq 1 30); do
	curl -sf 127.0.0.1:59010/donate/v1/info | jq -e .available >/dev/null 2>&1 && break
	sleep 1
done
curl -sf 127.0.0.1:59010/donate/v1/info | jq -e .available >/dev/null || { docker logs lfswap-cd-channels; fail "not available"; }
ok "the API says channel donations are available"

log "a channel donation to lnd-user"
USER_KEY=$(LU getinfo | jq -r .identity_pubkey)
# The user's node is a peer of ours already: nothing to dial
LS listpeers | jq -e --arg k "$USER_KEY" '.peers[] | select(.pub_key == $k)' >/dev/null ||
	LS connect "$USER_KEY@lnd-user:9735" >/dev/null
info=$(curl -sf 127.0.0.1:59010/donate/v1/info)
body=$(python3 - "$info" "$USER_KEY" <<'PY'
import hashlib, json, sys
info, key = json.loads(sys.argv[1]), sys.argv[2]
c, bits = info["challenge"], info["powBits"]
n = 0
while True:
    h = hashlib.sha256(f"{c}:{n}".encode()).digest()
    if int.from_bytes(h, "big") >> (256 - bits) == 0:
        break
    n += 1
print(json.dumps({"node": key, "accepted": True, "disclaimerVersion": info["disclaimerVersion"],
                  "challenge": c, "nonce": str(n)}))
PY
)
created=$(curl -sf -X POST -H 'Content-Type: application/json' --data "$body" 127.0.0.1:59010/donate/v1/channel-orders)
ID=$(jq -r .id <<<"$created")
ADDR=$(jq -r .order.address <<<"$created")
[ -n "$ADDR" ] && [ "$ADDR" != null ] || fail "no address: $created"
ok "order $ID, pay to $ADDR"
state() { curl -sf "127.0.0.1:59010/donate/v1/channel-orders/$ID" | jq -r .state; }
before=$(LU listchannels | jq '.channels | length')
K -rpcwallet=boltz -named sendtoaddress address="$ADDR" amount=0.02 fee_rate=2 >/dev/null
for _ in $(seq 1 20); do [ "$(state)" = payment_seen ] && break; sleep 1; done
[ "$(state)" = payment_seen ] || fail "payment not seen: $(state)"
ok "payment seen"
mine 3
for _ in $(seq 1 40); do [ "$(state)" = funding_broadcast ] && break; sleep 1; done
order=$(curl -sf "127.0.0.1:59010/donate/v1/channel-orders/$ID")
[ "$(jq -r .state <<<"$order")" = funding_broadcast ] || { docker logs lfswap-cd-channels | tail; fail "not opened: $(jq -c '{state, errorCode}' <<<"$order")"; }
POINT=$(jq -r .channelPoint <<<"$order")
ok "funding published: $POINT, capacity $(jq .capacitySat <<<"$order")"
# Funded from the donation alone
FUNDING=${POINT%:*}
for i in $(K getrawtransaction "$FUNDING" true | jq -r '.vin[] | "\(.txid):\(.vout)"'); do
	# The donation came from the regtest wallet (no txindex here)
	spent=$(K -rpcwallet=boltz gettransaction "${i%:*}" true true | jq -r --argjson n "${i#*:}" '.decoded.vout[$n].scriptPubKey.address')
	[ "$spent" = "$ADDR" ] || fail "the channel spent $i (of $spent), not the donation"
done
ok "the funding spends exactly the donated coin"
mine 6
for _ in $(seq 1 40); do [ "$(state)" = open ] && break; mine 1; sleep 1; done
[ "$(state)" = open ] || fail "not open: $(state)"
ch=$(LU listchannels | jq --arg p "$POINT" '.channels[] | select(.channel_point == $p)')
[ -n "$ch" ] || fail "lnd-user does not see the channel"
[ "$(jq -r .local_balance <<<"$ch")" = 0 ] || fail "something was pushed to the donor: $(jq .local_balance <<<"$ch")"
[ "$(LU listchannels | jq '.channels | length')" -gt "$before" ] || fail "no new channel"
ok "open; lnd-user has the channel, with nothing pushed to it"

log "what the guard refuses, with the worker's own macaroon"
CM="--macaroonpath=/root/.lnd/channels.macaroon"
refused() {
	local what=$1; shift
	if out=$(LS "$CM" "$@" 2>&1); then fail "$what went through: $out"; fi
	grep -q "refused by the donations guard" <<<"$out" || fail "$what: not the guard: $out"
	ok "$what: refused by the guard"
}
refused "sendcoins" sendcoins --addr "$(K -rpcwallet=boltz getnewaddress)" --amt 10000 --force
# Opens as the worker makes them (OpenChannelSync, over REST), from a
# throwaway container on the regtest network
rest() {
	docker run --rm --network "$NET" -v "$work:/lnd:ro" python:3-alpine python3 -c '
import base64, json, ssl, sys, urllib.request
path, body = sys.argv[1], sys.argv[2]
ctx = ssl.create_default_context(cafile="/lnd/tls.cert")
mac = open("/lnd/channels.macaroon", "rb").read().hex()
req = urllib.request.Request("https://lnd-swap:8080" + path, data=body.encode(),
    headers={"Grpc-Metadata-macaroon": mac, "Content-Type": "application/json"})
try:
    print(urllib.request.urlopen(req, context=ctx, timeout=30).read().decode())
except urllib.error.HTTPError as e:
    print(e.read().decode()); sys.exit(1)
' "$@"
}
rest_refused() {
	local what=$1; shift
	if out=$(rest "$@" 2>&1); then fail "$what went through: $out"; fi
	grep -q "refused by the donations guard" <<<"$out" || fail "$what: not the guard: $out"
	ok "$what: refused by the guard"
}
KEY64=$(python3 -c "import base64,sys; print(base64.b64encode(bytes.fromhex(sys.argv[1])).decode())" "$USER_KEY")
SPARE=$(LS listunspent --min_confs 1 | jq -c '.utxos[0].outpoint | split(":") | {txid_str: .[0], output_index: (.[1] | tonumber)}')
rest_refused "an open with a push" /v1/channels \
	"{\"node_pubkey\":\"$KEY64\",\"local_funding_amount\":\"1000000\",\"push_sat\":\"1000\",\"sat_per_vbyte\":\"2\",\"outpoints\":[$SPARE]}"
rest_refused "an open from the service's own coins" /v1/channels \
	"{\"node_pubkey\":\"$KEY64\",\"local_funding_amount\":\"1000000\",\"sat_per_vbyte\":\"2\",\"outpoints\":[$SPARE]}"
rest_refused "an open from any coins" /v1/channels \
	"{\"node_pubkey\":\"$KEY64\",\"local_funding_amount\":\"1000000\",\"sat_per_vbyte\":\"2\"}"
rest_refused "an open closing to their address" /v1/channels \
	"{\"node_pubkey\":\"$KEY64\",\"local_funding_amount\":\"1000000\",\"sat_per_vbyte\":\"2\",\"outpoints\":[$SPARE],\"close_address\":\"$(LU newaddress p2tr | jq -r .address)\"}"
refused "a dial to a private address" connect "$USER_KEY@172.17.0.1:9735"
if out=$(LS "$CM" openchannel --node_key "$USER_KEY" --local_amt 1000000 2>&1); then
	fail "the streaming open went through"
fi
ok "the streaming open: not in the macaroon at all"
if LS "$CM" getinfo >/dev/null 2>&1; then ok "a read passes"; else fail "getinfo refused"; fi

log "the guard away"
docker stop lfswap-cd-guard >/dev/null
sleep 2
if out=$(LS "$CM" getinfo 2>&1); then fail "the worker's macaroon works without the guard"; fi
grep -q "no middleware registered" <<<"$out" || fail "unexpected: $out"
ok "lnd refuses the worker's macaroon: $(grep -o 'cannot accept macaroon[^"]*' <<<"$out" | head -1)"
LS getinfo >/dev/null || fail "lnd's own macaroon refused"
ok "other macaroons (the backend's) are not affected"
docker start lfswap-cd-guard >/dev/null
sleep 3
LS "$CM" getinfo >/dev/null 2>&1 || fail "not back after the guard returned"
ok "back once the guard registers again"

printf '\n\033[1mchannel donations on regtest: all passed\033[0m\n'
