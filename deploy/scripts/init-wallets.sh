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
if [ ! -f "$S/knots-boltz-descriptors.json" ]; then
	knots -rpcwallet=boltz listdescriptors true > "$S/knots-boltz-descriptors.json"
	echo "wrote $S/knots-boltz-descriptors.json (private descriptors)"
fi

# --- lnd wallet ---
if [ ! -f "$ROOT/lnd/wallet-password" ]; then
	head -c 32 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 32 > "$ROOT/lnd/wallet-password"
	cp "$ROOT/lnd/wallet-password" "$S/lnd-wallet-password.txt"
fi
docker compose up -d tor lnd >/dev/null
if [ ! -f "$ROOT/lnd/data/chain/bitcoin/mainnet/wallet.db" ]; then
	echo "waiting for lnd to ask for a wallet"
	for _ in $(seq 1 60); do
		curl -sk https://127.0.0.1:8080/v1/genseed >/dev/null 2>&1 && break
		sleep 2
	done
	seed=$(curl -sk https://127.0.0.1:8080/v1/genseed)
	mnemonic=$(echo "$seed" | jq -c .cipher_seed_mnemonic)
	[ "$mnemonic" != null ] || { echo "genseed failed: $seed" >&2; exit 1; }
	pw=$(base64 -w0 < "$ROOT/lnd/wallet-password")
	curl -sk -X POST https://127.0.0.1:8080/v1/initwallet \
		-d "{\"wallet_password\":\"$pw\",\"cipher_seed_mnemonic\":$mnemonic}" >/dev/null
	echo "$seed" | jq -r '.cipher_seed_mnemonic | to_entries | map("\(.key + 1). \(.value)") | .[]' > "$S/lnd-seed.txt"
	echo "created the lnd wallet; its 24-word seed is in $S/lnd-seed.txt (no passphrase)"
fi
ls -l "$S"
