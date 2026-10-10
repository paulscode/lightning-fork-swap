#!/usr/bin/env bash
# verify.sh snapshot [FILE]        write what must survive a move, as JSON
# verify.sh compare OLD.json       snapshot this host and compare with OLD
#
# Compared: lnd identity, its on-chain balance, the set of channels and their
# local balances, the Knots wallet's balance, every swap's id and status, the
# backend's seed fingerprint and the donations on record. Not compared:
# whether channels are active (peers need a moment to reconnect) or block
# heights.
. "$(dirname "$0")/lib.sh"

snapshot() {
	local info wallet channels pending knotsbal swaps rswaps seedfp donations
	info=$(lncli getinfo)
	wallet=$(lncli walletbalance)
	channels=$(lncli listchannels | jq -c '[.channels[] | {chan_point: .channel_point, capacity, local_balance, remote_pubkey}] | sort_by(.chan_point)')
	pending=$(lncli pendingchannels | jq -c '{open: (.pending_open_channels | length), closing: (.waiting_close_channels | length), force: (.pending_force_closing_channels | length)}')
	knotsbal=$(knots -rpcwallet=boltz getbalances | jq -c '.mine')
	swaps=$(psql_q "select coalesce(json_agg(json_build_array(id, status) order by id), '[]') from swaps")
	rswaps=$(psql_q "select coalesce(json_agg(json_build_array(id, status) order by id), '[]') from \"reverseSwaps\"")
	seedfp=$(sha256sum "$ROOT/boltz/seed.dat" | cut -c1-16)
	# Their txids (verdicts change with re-checks); null on a host without
	# the donations database
	donations=$(dc exec -T postgres psql -U boltz -d donations -Atc \
		"select coalesce(json_agg(txid order by txid), '[]') from replay_checks" </dev/null 2>/dev/null || echo null)
	jq -n \
		--arg host "$(hostname)" --arg at "$(date -u +%FT%TZ)" \
		--argjson info "$info" --argjson wallet "$wallet" \
		--argjson channels "$channels" --argjson pending "$pending" \
		--argjson knots "$knotsbal" --argjson swaps "$swaps" --argjson rswaps "$rswaps" \
		--argjson donations "$donations" \
		--arg seedfp "$seedfp" --arg version "$(curl -s http://127.0.0.1:9001/version | jq -r .version)" \
		'{host: $host, at: $at,
		  lnd: {pubkey: $info.identity_pubkey, version: $info.version, height: $info.block_height,
		        onchain: $wallet.total_balance, channels: $channels, pending: $pending},
		  knots_wallet: $knots, backend: {version: $version, seed: $seedfp},
		  swaps: $swaps, reverse_swaps: $rswaps, donations: $donations}'
}

case "${1:-}" in
snapshot)
	out=${2:-$MIGRATION/snapshot-$(hostname)-$(date -u +%Y%m%dT%H%M%SZ).json}
	snapshot > "$out"
	log "snapshot written to $out"
	echo "$out"
	;;
compare)
	[ -f "${2:-}" ] || die "usage: $0 compare OLD.json"
	new=$(snapshot)
	old=$(cat "$2")
	fail=0
	check() {
		local what=$1 path=$2
		local a b
		a=$(jq -cS "$path" <<<"$old"); b=$(jq -cS "$path" <<<"$new")
		if [ "$a" = "$b" ]; then
			printf '  ok    %s\n' "$what"
		else
			printf '  DIFF  %s\n        was %s\n        now %s\n' "$what" "${a:0:300}" "${b:0:300}"
			fail=1
		fi
	}
	echo "comparing $(jq -r '.host + " at " + .at' <<<"$old") with this host"
	check "lnd identity" .lnd.pubkey
	check "lnd on-chain balance" .lnd.onchain
	check "channels and local balances" .lnd.channels
	check "pending channels" .lnd.pending
	check "Knots wallet balance" .knots_wallet
	check "backend seed" .backend.seed
	check "submarine swaps and statuses" .swaps
	check "reverse swaps and statuses" .reverse_swaps
	# Snapshots from before donations have none to compare
	if [ "$(jq '.donations != null' <<<"$old")" = true ]; then
		check "donations on record" .donations
	fi
	exit $fail
	;;
*) die "usage: $0 snapshot [FILE] | compare OLD.json" ;;
esac
