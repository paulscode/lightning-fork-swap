#!/usr/bin/env bash
# The SHA256-chain donations against two real nodes on regtest that share
# their first 19 blocks: the BLAKE2b Knots (from height 20 its own rules)
# and a SHA256 Knots. Coins mined before the split exist on both; a donation
# from them is copied over as the network would; our forward of it must be
# taken by the BLAKE2b node and refused by the SHA256 one.
#
#   deploy/regtest/sha256-donations-twochain.sh
#
# SHA256_BITCOIND: a SHA256-chain node (bitcoin-cli next to it): Knots'
# policy series on Core (knots-sha256), relaying data outputs up to 200
# bytes (-datacarriersize), as the lock needs; stock Core 30 or later does
# by default. Not Knots 29: it applies the BIP110 data rules in its
# mempool whatever its options, which is the point of them on the BLAKE2b
# chain.
# Both nodes are throwaway: a container and a temporary directory, on
# 127.0.0.1 ports 19443-19544. The regtest stack is not touched.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
SHA256_BITCOIND=${SHA256_BITCOIND:-/mnt/Black/knots-sha256-framework/build/ks-31-093226b/bin/bitcoind}
SHA256_CLI=$(dirname "$SHA256_BITCOIND")/bitcoin-cli
KNOTS_IMAGE=${KNOTS_IMAGE:-lfswap/knots:29.4.2}
name=lfswap-twochain-$$
work=$(mktemp -d)
sha_pid=
cleanup() {
	docker rm -f "$name" >/dev/null 2>&1 || true
	if [ -n "$sha_pid" ]; then kill "$sha_pid" 2>/dev/null || true; wait "$sha_pid" 2>/dev/null || true; fi
	rm -rf "$work"
}
trap cleanup EXIT

"$SHA256_BITCOIND" -help | grep -qi blake2b && { echo "$SHA256_BITCOIND knows BLAKE2b: a SHA256-chain node is needed" >&2; exit 1; }

docker run -d --rm --name "$name" --network host "$KNOTS_IMAGE" \
	-regtest -server -listen=1 -port=19444 -rpcport=19443 -rpcbind=127.0.0.1 -rpcallowip=127.0.0.1 \
	-rpcuser=lab -rpcpassword=lab -testactivationheight=blake2b@20 -rdtsexpiry=1819843200 \
	-fallbackfee=0.0002 -disablewallet=0 -listenonion=0 -dnsseed=0 -fixedseeds=0 >/dev/null
mkdir -p "$work/sha"
"$SHA256_BITCOIND" -regtest -datadir="$work/sha" -server -listen=1 -port=19544 -rpcport=19543 \
	-rpcbind=127.0.0.1 -rpcallowip=127.0.0.1 -rpcuser=lab -rpcpassword=lab -fallbackfee=0.0002 \
	-listenonion=0 -onion=0 -proxy=0 -dnsseed=0 -fixedseeds=0 -natpmp=0 -datacarriersize=200 \
	-printtoconsole=0 >"$work/sha.log" 2>&1 &
sha_pid=$!

b() { docker exec "$name" bitcoin-cli -regtest -rpcport=19443 -rpcuser=lab -rpcpassword=lab "$@"; }
s() { "$SHA256_CLI" -regtest -rpcport=19543 -rpcuser=lab -rpcpassword=lab "$@"; }
for _ in $(seq 1 60); do b getblockcount >/dev/null 2>&1 && s getblockcount >/dev/null 2>&1 && break; sleep 1; done

s createwallet donor >/dev/null
s createwallet miner >/dev/null
donor=$(s -rpcwallet=donor getnewaddress "" bech32)
miner=$(s -rpcwallet=miner getnewaddress "" bech32)

# Blocks 1 to 19, before the BLAKE2b rules: both chains have them, and their
# coins are the donor's
b generatetoaddress 19 "$donor" >/dev/null
s addnode 127.0.0.1:19444 onetry
for _ in $(seq 1 60); do [ "$(s getblockcount)" = 19 ] && break; sleep 1; done
[ "$(s getblockcount)" = 19 ] || { echo "the SHA256 node did not take the shared blocks" >&2; exit 1; }
[ "$(s getblockhash 19)" = "$(b getblockhash 19)" ] || { echo "the chains differ before the split" >&2; exit 1; }
s setnetworkactive false >/dev/null
b setnetworkactive false >/dev/null

# The split: each chain mines on, to the miner, until the shared coins mature
b generatetoaddress 110 "$miner" >/dev/null
s generatetoaddress 110 "$miner" >/dev/null
[ "$(s getblockhash 20)" != "$(b getblockhash 20)" ] || { echo "the chains did not split" >&2; exit 1; }
echo "two chains: shared to 19, BLAKE2b at $(b getblockcount), SHA256 at $(s getblockcount)"

cd "$repo/donations"
TWOCHAIN_BLAKE2B=http://lab:lab@127.0.0.1:19443 TWOCHAIN_SHA256=http://lab:lab@127.0.0.1:19543 \
	go test -count=1 -run TestTwoChains -v ./internal/sha256don
