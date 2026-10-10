#!/usr/bin/env bash
# Creates the service's two wallets, once, and writes what is needed to
# recover them to $LFSWAP_ROOT/secrets (root only). Copy those files somewhere
# safe and offline, then delete them from the server.
#
#   knots wallet "boltz"  the backend's on-chain funds (reverse swap lockups)
#   lnd wallet            the Lightning node's funds and channels
#
# The swap backend's own seed (seed.dat) is created by the backend at its
# first start; scripts/backup-secrets.sh copies it next to these.
set -euo pipefail
cd "$(dirname "$0")/.."
set -a; . ./.env; set +a
ROOT=${LFSWAP_ROOT:-/srv/lfswap}
S=$ROOT/secrets
umask 077

knots() { docker compose exec -T knots bitcoin-cli -datadir=/data "$@"; }

# --- Knots wallet ---
if ! knots listwallets | grep -q '"boltz"'; then
	if ! knots -named loadwallet filename=boltz load_on_startup=true >/dev/null 2>&1; then
		knots -named createwallet wallet_name=boltz load_on_startup=true >/dev/null
		echo "created Knots wallet boltz"
	fi
fi
if [ ! -s "$S/knots-boltz-descriptors.json" ]; then
	# Through a temporary file: a failed call must not leave an empty file
	# that a rerun would then take for the backup
	knots -rpcwallet=boltz listdescriptors true > "$S/knots-boltz-descriptors.json.tmp"
	jq -e '.descriptors | length > 0' "$S/knots-boltz-descriptors.json.tmp" >/dev/null
	mv "$S/knots-boltz-descriptors.json.tmp" "$S/knots-boltz-descriptors.json"
	echo "wrote $S/knots-boltz-descriptors.json (private descriptors)"
fi

# --- lnd wallet ---
if [ ! -f "$ROOT/lnd/wallet-password" ]; then
	head -c 32 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 32 > "$ROOT/lnd/wallet-password"
	cp "$ROOT/lnd/wallet-password" "$S/lnd-wallet-password.txt"
fi
docker compose up -d tor lnd >/dev/null
if [ ! -f "$ROOT/lnd/data/chain/bitcoin/${NETWORK:-mainnet}/wallet.db" ]; then
	echo "waiting for lnd to ask for a wallet"
	for _ in $(seq 1 60); do
		curl -sk https://127.0.0.1:8080/v1/genseed >/dev/null 2>&1 && break
		sleep 2
	done
	seed=$(curl -skf https://127.0.0.1:8080/v1/genseed)
	mnemonic=$(echo "$seed" | jq -c .cipher_seed_mnemonic)
	[ "$(echo "$mnemonic" | jq 'length')" = 24 ] || { echo "genseed failed" >&2; exit 1; }
	# The seed is on disk, whole, before any wallet uses it: a wallet whose
	# seed was never written down cannot be recovered
	echo "$seed" | jq -r '.cipher_seed_mnemonic | to_entries | map("\(.key + 1). \(.value)") | .[]' > "$S/lnd-seed.txt.tmp"
	[ "$(wc -l < "$S/lnd-seed.txt.tmp")" = 24 ] || { echo "could not write the seed" >&2; exit 1; }
	sync "$S/lnd-seed.txt.tmp"
	mv "$S/lnd-seed.txt.tmp" "$S/lnd-seed.txt"
	# Neither on the command line, where other users of the host see it: the
	# password from a file, the seed on stdin
	echo "$seed" | jq -c --rawfile pw <(base64 -w0 < "$ROOT/lnd/wallet-password") \
		'{wallet_password: $pw, cipher_seed_mnemonic: .cipher_seed_mnemonic}' |
		curl -skf -X POST https://127.0.0.1:8080/v1/initwallet --data-binary @- >/dev/null ||
		{ echo "initwallet failed; the seed in $S/lnd-seed.txt was not used, run again" >&2; exit 1; }
	echo "created the lnd wallet; its 24-word seed is in $S/lnd-seed.txt (no passphrase)"
fi
# The backend's and the donation watcher's lnd macaroons, each limited to
# what it calls
./scripts/bake-macaroon.sh
ls -l "$S"
