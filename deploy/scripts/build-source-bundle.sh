#!/usr/bin/env bash
# build-source-bundle.sh [OUT_DIR] [--upload root@HOST]
#
# Builds the source archives the AGPL asks the running service to offer its
# users, from the workstation's checkouts, and optionally uploads them to
# /srv/lfswap/source on the host, where nginx serves them at /source/.
#
#   lightning-fork-swap-<commit>.tar.gz          this repository (deployment, shim)
#   lightning-fork-swap-backend-<commit>.tar.gz  the backend fork, with the
#                                                `hold` submodule its build needs
#   lightning-fork-swap-webapp-<commit>.tar.gz   the web app fork
#   SHA256SUMS, README.txt
#
# Each archive is `git archive` of the checked-out commit: run it after
# building and deploying, so the archives match what runs. Untracked and
# ignored files (private notes, secrets, build output) are never included.
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
OUT=${1:-}; OUT=${OUT:-$REPO/../lfswap-source}
UPLOAD=""
[ "${2:-}" = "--upload" ] && UPLOAD=${3:?--upload needs root@HOST}

for d in "$REPO" "$REPO/backend" "$REPO/webapp"; do
	if [ -n "$(git -C "$d" status --porcelain --untracked-files=no)" ]; then
		echo "$d has uncommitted changes: commit them, so the archive matches what runs" >&2
		exit 1
	fi
done

rm -rf "$OUT" && mkdir -p "$OUT"
root=$(git -C "$REPO" rev-parse --short HEAD)
backend=$(git -C "$REPO/backend" rev-parse --short HEAD)
webapp=$(git -C "$REPO/webapp" rev-parse --short HEAD)
hold=$(git -C "$REPO/backend/hold" rev-parse --short HEAD)

git -C "$REPO" archive --format=tar.gz --prefix="lightning-fork-swap/" \
	-o "$OUT/lightning-fork-swap-$root.tar.gz" HEAD

# The backend with its hold submodule (the sidecar's build reads its protos).
tmp=$(mktemp -d)
git -C "$REPO/backend" archive --prefix="lightning-fork-swap-backend/" HEAD | tar -x -C "$tmp"
git -C "$REPO/backend/hold" archive --prefix="lightning-fork-swap-backend/hold/" HEAD | tar -x -C "$tmp"
tar -C "$tmp" -czf "$OUT/lightning-fork-swap-backend-$backend.tar.gz" lightning-fork-swap-backend
rm -rf "$tmp"

git -C "$REPO/webapp" archive --format=tar.gz --prefix="lightning-fork-swap-webapp/" \
	-o "$OUT/lightning-fork-swap-webapp-$webapp.tar.gz" HEAD

cat > "$OUT/README.txt" <<TXT
Lightning Fork Swap — source code
=================================

Source of the service running at this site, offered under the GNU Affero
General Public License v3.0 (AGPL-3.0), the licence of the Boltz software it
is built on (https://github.com/BoltzExchange).

  lightning-fork-swap-$root.tar.gz
      Deployment (Docker Compose, configuration templates, nginx), the
      txindex shim, end-to-end tests.
  lightning-fork-swap-backend-$backend.tar.gz
      The swap backend: a fork of boltz-backend (via SwapMarket's fork).
      LIGHTNING-FORK.md inside lists every change from upstream. Includes
      the hold submodule ($hold) its build uses.
  lightning-fork-swap-webapp-$webapp.tar.gz
      The web app: a fork of boltz-web-app 2.2.1. README.md lists the changes.

Other software the service runs, unmodified:
  Lightning Fork (lnd for the Bitcoin BLAKE2b chain): https://github.com/paulscode/lightning-fork
  Bitcoin Knots 29.4.2: https://bitcoinknots.org
  PostgreSQL, Tor, nginx: their upstream releases.

Archives built $(date -u +%Y-%m-%d) from: root $root, backend $backend, webapp $webapp.
Checksums: SHA256SUMS.
TXT
(cd "$OUT" && sha256sum ./*.tar.gz README.txt | sed 's| \./| |' > SHA256SUMS)
ls -l "$OUT"

if [ -n "$UPLOAD" ]; then
	ssh "$UPLOAD" 'mkdir -p /srv/lfswap/source'
	rsync -a --delete "$OUT/" "$UPLOAD:/srv/lfswap/source/"
	echo "uploaded to $UPLOAD:/srv/lfswap/source/ (served at /source/)"
fi
