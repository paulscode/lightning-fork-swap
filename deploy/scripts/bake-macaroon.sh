#!/usr/bin/env bash
# Bakes the macaroons that services other than lnd use with it, each with
# only the calls that service makes. lnd must be running and unlocked.
#
#   boltz.macaroon      the swap backend: invoices, payments, channel and
#                       graph reads, channel backups; whoever takes over the
#                       backend cannot move lnd's on-chain funds, close
#                       channels or bake macaroons of their own
#   donations.macaroon  the donation watcher: lists the wallet's
#                       transactions, nothing else
#   channels.macaroon   the channel donation worker: addresses, its coins'
#                       leases, peers and opens; with the custom caveat
#                       lfswap-donations, so lnd lets it do nothing unless
#                       the guard (below) approves each call
#   guard.macaroon      the guard: registers itself as lnd's middleware and
#                       lists leases
#   graph.macaroon      the graph generator: reads the channel graph
#
#   bake-macaroon.sh            once: keeps the ones that exist
#   bake-macaroon.sh --force    bake new ones (after restoring lnd from its
#                               seed, whose new macaroon database does not
#                               know the old ones)
#
# Each has its own root key, so one can be revoked alone (lncli
# deletemacaroonid N): boltz 1, donations 2, channels 3, guard 4, graph 5.
set -euo pipefail
cd "$(dirname "$0")/.."
set -a; . ./.env; set +a
ROOT=${LFSWAP_ROOT:-/srv/lfswap}
force=${1:-}

# What the backend calls (lib/lightning/LndClient.ts) and its sidecar
# (boltzr/src/lightning/lnd): nothing else
boltz=(
	uri:/lnrpc.Lightning/GetInfo
	uri:/lnrpc.Lightning/AddInvoice
	uri:/lnrpc.Lightning/LookupInvoice
	uri:/lnrpc.Lightning/DecodePayReq
	uri:/lnrpc.Lightning/GetNodeInfo
	uri:/lnrpc.Lightning/GetChanInfo
	uri:/lnrpc.Lightning/DescribeGraph
	uri:/lnrpc.Lightning/QueryRoutes
	uri:/lnrpc.Lightning/ListChannels
	uri:/lnrpc.Lightning/ListPeers
	uri:/lnrpc.Lightning/WalletBalance
	uri:/lnrpc.Lightning/SubscribePeerEvents
	uri:/lnrpc.Lightning/SubscribeChannelEvents
	uri:/lnrpc.Lightning/ExportAllChannelBackups
	uri:/lnrpc.Lightning/SubscribeChannelBackups
	uri:/routerrpc.Router/SendPaymentV2
	uri:/routerrpc.Router/TrackPaymentV2
	uri:/routerrpc.Router/ResetMissionControl
	uri:/invoicesrpc.Invoices/AddHoldInvoice
	uri:/invoicesrpc.Invoices/CancelInvoice
	uri:/invoicesrpc.Invoices/SettleInvoice
	uri:/invoicesrpc.Invoices/SubscribeSingleInvoice
)

# What the donation watcher calls (donations/internal/lnd)
donations=(
	uri:/lnrpc.Lightning/GetTransactions
)

missing() { [ "$force" = --force ] || [ ! -s "$ROOT/lnd/$1" ]; }
# What the channel worker calls (donations/internal/channels), checked
# again call by call by the guard (donations/internal/guard). The caveat
# flags first: lncli takes its flags before the permissions.
channels=(
	--custom_caveat_name lfswap-donations --custom_caveat_condition v1
	uri:/lnrpc.Lightning/GetInfo
	uri:/lnrpc.Lightning/GetNodeInfo
	uri:/lnrpc.Lightning/ListPeers
	uri:/lnrpc.Lightning/ListChannels
	uri:/lnrpc.Lightning/PendingChannels
	uri:/lnrpc.Lightning/ClosedChannels
	uri:/lnrpc.Lightning/GetTransactions
	uri:/lnrpc.Lightning/NewAddress
	uri:/lnrpc.Lightning/ConnectPeer
	uri:/lnrpc.Lightning/OpenChannelSync
	uri:/walletrpc.WalletKit/ListUnspent
	uri:/walletrpc.WalletKit/ListLeases
	uri:/walletrpc.WalletKit/LeaseOutput
	uri:/walletrpc.WalletKit/ReleaseOutput
	uri:/walletrpc.WalletKit/EstimateFee
)
guard=(
	uri:/lnrpc.Lightning/RegisterRPCMiddleware
	uri:/walletrpc.WalletKit/ListLeases
)

graph=(
	uri:/lnrpc.Lightning/GetInfo
	uri:/lnrpc.Lightning/DescribeGraph
)

missing boltz.macaroon || missing donations.macaroon || missing channels.macaroon ||
	missing guard.macaroon || missing graph.macaroon || exit 0

lncli() { docker compose exec -T lnd lncli --network="${NETWORK:-mainnet}" "$@" </dev/null; }
for _ in $(seq 1 150); do
	lncli getinfo >/dev/null 2>&1 && break
	sleep 2
done
lncli getinfo >/dev/null || { echo "lnd is not up and unlocked" >&2; exit 1; }

# bake NAME OWNER ROOT_KEY_ID PERMISSION...: written by lnd into its own
# directory ($ROOT/lnd), then handed to the uid the service runs as and
# moved into place
bake() {
	local name=$1 owner=$2 key=$3; shift 3
	missing "$name" || return 0
	local out=$ROOT/lnd/$name
	lncli bakemacaroon --root_key_id "$key" --save_to "/root/.lnd/$name.tmp" "$@" >/dev/null
	chown "$owner" "$out.tmp"
	chmod 400 "$out.tmp"
	mv "$out.tmp" "$out"
	echo "baked $out"
}
# The backend runs as root; the donations image is distroless's nonroot
bake boltz.macaroon 0:0 1 "${boltz[@]}"
bake donations.macaroon 65532:65532 2 "${donations[@]}"
bake channels.macaroon 65532:65532 3 "${channels[@]}"
bake guard.macaroon 65532:65532 4 "${guard[@]}"
bake graph.macaroon 65532:65532 5 "${graph[@]}"
