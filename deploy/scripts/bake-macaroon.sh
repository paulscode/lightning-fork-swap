#!/usr/bin/env bash
# Bakes the macaroon the swap backend uses with lnd: only the calls it makes
# (invoices, payments, channel and graph reads, channel backups), so that
# whoever takes over the backend cannot move lnd's on-chain funds, close
# channels or bake macaroons of their own. lnd must be running and unlocked.
#
#   bake-macaroon.sh            once: keeps an existing one
#   bake-macaroon.sh --force    bake a new one (after restoring lnd from its
#                               seed, whose new macaroon database does not
#                               know the old one)
#
# Revoke all macaroons baked here: lncli deletemacaroonid 1
set -euo pipefail
cd "$(dirname "$0")/.."
set -a; . ./.env; set +a
ROOT=${LFSWAP_ROOT:-/srv/lfswap}
out=$ROOT/lnd/boltz.macaroon

if [ -s "$out" ] && [ "${1:-}" != --force ]; then
	exit 0
fi

# What the backend calls (lib/lightning/LndClient.ts) and its sidecar
# (boltzr/src/lightning/lnd): nothing else
permissions=(
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

lncli() { docker compose exec -T lnd lncli --network="${NETWORK:-mainnet}" "$@" </dev/null; }
for _ in $(seq 1 150); do
	lncli getinfo >/dev/null 2>&1 && break
	sleep 2
done
lncli getinfo >/dev/null || { echo "lnd is not up and unlocked" >&2; exit 1; }

# Written by lnd into its own directory ($ROOT/lnd), then moved into place
lncli bakemacaroon --root_key_id 1 --save_to /root/.lnd/boltz.macaroon.tmp \
	"${permissions[@]}" >/dev/null
chmod 600 "$out.tmp"
mv "$out.tmp" "$out"
echo "baked $out"
