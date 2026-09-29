#!/usr/bin/env bash
# Brings up the regtest service from nothing: a chain past the BLAKE2b fork,
# funded wallets, a channel each way between the swap node and the user's
# node, and the swap backend.
set -euo pipefail
cd "$(dirname "$0")"

cli() { docker compose exec -T knots bitcoin-cli -regtest -rpcuser=lab -rpcpassword=lab "$@"; }
lncli() { local n=$1; shift; docker compose exec -T "$n" lncli --network=regtest "$@"; }
# The backend wants exactly one wallet loaded on the node, so there is no
# separate miner wallet: blocks pay the service's own wallet.
mine() { cli generatetoaddress "$1" "$(cli -rpcwallet=boltz getnewaddress)" >/dev/null; }
wait_for() {
	local what=$1; shift
	for _ in $(seq 1 90); do "$@" >/dev/null 2>&1 && return 0; sleep 2; done
	echo "timed out waiting for $what" >&2; exit 1
}
synced() { lncli "$1" getinfo | grep -Eq '"synced_to_chain": +true'; }

docker compose up -d knots shim
wait_for knots cli getblockchaininfo
cli -named createwallet wallet_name=boltz load_on_startup=true >/dev/null 2>&1 || cli -named loadwallet filename=boltz load_on_startup=true >/dev/null 2>&1 || true
mine 150
echo "chain at $(cli getblockcount); header of block 30 is $(( $(cli getblockheader "$(cli getblockhash 30)" false | wc -c) / 2 )) bytes"

docker compose up -d lnd-swap lnd-user
wait_for lnd-swap synced lnd-swap
wait_for lnd-user synced lnd-user

for n in lnd-swap lnd-user; do
	addr=$(lncli $n newaddress p2tr | jq -r .address)
	cli -rpcwallet=boltz sendtoaddress "$addr" 5 >/dev/null
done
mine 6
wait_for "lnd-swap funds" sh -c "docker compose exec -T lnd-swap lncli --network=regtest walletbalance | jq -e '.confirmed_balance != \"0\"'"
wait_for "lnd-user funds" sh -c "docker compose exec -T lnd-user lncli --network=regtest walletbalance | jq -e '.confirmed_balance != \"0\"'"

swap_pub=$(lncli lnd-swap getinfo | jq -r .identity_pubkey)
user_pub=$(lncli lnd-user getinfo | jq -r .identity_pubkey)
lncli lnd-user connect "$swap_pub@lnd-swap:9735" >/dev/null 2>&1 || true
# One channel each way: the user's outbound pays reverse swaps, the swap
# node's pays submarine swaps. One at a time: a peer allows one pending.
open_if_missing() {
	local from=$1 to_pub=$2
	local have
	have=$(lncli "$from" listchannels | jq --arg p "$to_pub" '[.channels[] | select(.remote_pubkey == $p and .initiator)] | length')
	have=$((have + $(lncli "$from" pendingchannels | jq --arg p "$to_pub" '[.pending_open_channels[] | select(.channel.remote_node_pub == $p)] | length')))
	if [ "$have" = 0 ]; then
		lncli "$from" openchannel --node_key "$to_pub" --local_amt 10000000 >/dev/null
	fi
	mine 6
	sleep 3
}
open_if_missing lnd-user "$swap_pub"
open_if_missing lnd-swap "$user_pub"
wait_for "channels" sh -c "[ \"\$(docker compose exec -T lnd-user lncli --network=regtest listchannels | jq '[.channels[] | select(.active)] | length')\" = 2 ]"
lncli lnd-user listchannels | jq -c '.channels[] | {remote_pubkey, capacity, local_balance, unified_sigs}'

docker compose up -d postgres boltz
wait_for "boltz API" curl -sf http://127.0.0.1:19001/v2/swap/submarine
curl -s http://127.0.0.1:19001/v2/swap/submarine | jq -c .
curl -s http://127.0.0.1:19001/v2/swap/reverse | jq -c .
echo "regtest service up"
