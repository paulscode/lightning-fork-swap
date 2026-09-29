# Running Lightning Fork Swap

One Debian 12 host runs everything: a pruned Bitcoin Knots node on the
Bitcoin BLAKE2b chain, the txindex shim, Tor, Lightning Fork (lnd), Postgres
and the swap backend in Docker, and nginx with a Let's Encrypt certificate on
the host serving the web app and the API on one origin.

```
nginx :443 ─┬─ /                 web app (static, /srv/lfswap/webapp)
            ├─ /v2/…             boltzd        127.0.0.1:9001
            ├─ /v2/ws            sidecar WS    127.0.0.1:9004
            ├─ /v2/lightning…    sidecar API   127.0.0.1:9005
            └─ /explorer/api/    https://mempool.guide/api/
boltz ─ shim ─ knots (pruned, wallet "boltz")      lnd :9735 ─ tor
```

State lives in `/srv/lfswap`; recovery material in `/srv/lfswap/secrets`
(root only). The deployment files live in `/opt/lfswap` (a copy of this
repository without the backend and web app sources).

A host with 2 GB of RAM works with 3 GB of swap added; the containers use
about 1 GB between them once running.

## First install

1. Packages: `apt-get install nftables tor nginx certbot python3-certbot-nginx
   apache2-utils jq rsync`, Docker CE and its compose plugin from Docker's
   apt repository. Add swap if the host has little memory.
2. Copy this repository to `/opt/lfswap`, then in `/opt/lfswap/deploy`:
   ```sh
   docker build -t lfswap/knots:29.4.2 knots
   docker build -t lfswap/tor:dev tor
   docker build -t lfswap/txindex-shim:dev ../shim
   # the backend image is built from the backend repository:
   #   docker build -f docker/boltz/Dockerfile --build-arg NODE_VERSION=24-bookworm-slim \
   #     --build-arg SOURCE=local -t lfswap/boltz:dev .
   ./scripts/setup.sh          # .env with fresh secrets, directories, configs
   ```
3. The node. Initial sync of the chain on a small host takes weeks; seed
   `/srv/lfswap/knots/data` with `blocks/` and `chainstate/` from a pruned
   node you trust that was shut down cleanly, then `docker compose up -d
   knots` and check `getblockhash 961640` is
   `0000000000000050c1e5f69672f459293be14f46e5a494e7a8c8541396f18eeb`.
4. `./scripts/install-host.sh firewall` (undoes itself after two minutes
   unless you run `firewall-confirm` from a new SSH session), then `tls`, then
   `nginx` (creates the preview password).
5. `./scripts/init-wallets.sh`: the Knots wallet and the lnd wallet; seeds go
   to `/srv/lfswap/secrets`.
6. `docker compose up -d` (Postgres, the shim and the backend). The backend
   creates its own seed (`/srv/lfswap/boltz/seed.dat`) at first start.
7. Put the web app's `dist/` (built with `bun run mainnet && bun run build`)
   in `/srv/lfswap/webapp`.
8. `echo "17 * * * * root /opt/lfswap/deploy/scripts/backup-secrets.sh" >
   /etc/cron.d/lfswap-backup`.

## Backups

Take these offline, then remove them from the server:

| File | Recovers |
| --- | --- |
| `lnd-seed.txt` (+ `lnd-wallet-password.txt`) | lnd's on-chain funds; with `channel.backup`, its channels |
| `channel.backup` | channels, by force close through their peers (updated hourly, with the last 20 versions kept beside it) |
| `boltz-seed.dat` | every swap key, for claims and refunds |
| `knots-boltz-descriptors.json` | the backend's on-chain wallet |

`channel.backup` changes with every channel open and close; fetch a fresh copy
after changing channels.

## Everyday commands

From `/opt/lfswap/deploy`:

```sh
docker compose ps
docker compose logs -f --tail 100 boltz
docker compose exec lnd lncli getinfo
docker compose exec lnd lncli listchannels
docker compose exec knots bitcoin-cli -datadir=/data -rpcwallet=boltz getbalances
curl -s http://127.0.0.1:9001/v2/swap/submarine    # what the service offers
```

The backend's admin CLI:

```sh
B="docker compose exec boltz boltzr-cli --grpc-certificates /boltz/certificates --jwt-file /boltz/certificates/admin.jwt"
$B get-info
$B wallet get-balance
$B swap pending-sweeps
$B swap sweep          # claim deferred submarine swaps now instead of at the next batch
```

## Liquidity

- **Reverse swaps** (Lightning → chain) pay out of the Knots wallet `boltz`
  and are paid to lnd over Lightning. They need on-chain funds in `boltz` and
  inbound capacity on lnd's channels.
- **Submarine swaps** (chain → Lightning) are paid over lnd's outbound
  capacity and claimed into the `boltz` wallet.

Fund the `boltz` wallet: `bitcoin-cli -datadir=/data -rpcwallet=boltz
getnewaddress`. Fund lnd: `lncli newaddress p2tr`. Open a channel: `lncli
openchannel --node_key <pubkey> --local_amt <sat>` (above 16,777,215 sat only
with peers that accept large channels). When one side runs low, move funds
the other way, or swap through the service itself.

## Terms

Fees and limits come from `.env`:

```sh
REVERSE_FEE_PERCENT=0.5
SUBMARINE_FEE_PERCENT=0.5
MIN_SWAP_SAT=10000
MAX_SWAP_SAT=1000000
```

After changing them: `./scripts/setup.sh && docker compose restart boltz`.

## Going public

The site starts behind a password (`/srv/lfswap/secrets/preview-password.txt`,
user `preview`). To open it, remove the two `auth_basic` lines from
`/etc/nginx/sites-available/lightningfork.conf` and `systemctl reload nginx`.
The code is AGPL-3.0: publish the backend, web app and this repository before
the site is open to others.

## Using a remote node over Tor instead

If the host cannot run the node, point the shim at a remote Knots node that
has `-txindex=1`, reached over Tor: run `socat
TCP-LISTEN:8332,fork SOCKS4A:tor:<onion>:8332,socksport=9050` (and the same
for the two ZMQ ports) in a container on the compose network, set
`SHIM_UPSTREAM` to it, and leave the shim's index to find nothing, since the
node answers by itself. The backend's hot wallet then lives on the remote
node.

## Troubleshooting

- *"Multiple wallets are loaded"* at backend start: the backend needs exactly
  one wallet loaded in Knots. Unload any other.
- *"no transaction with id"* for an old transaction: the node is pruned and
  the shim indexes the last 8,640 blocks (60 days). Raise `SHIM_WINDOW` only
  while it stays well below the blocks the node keeps (about 19,000 at
  `prune=5000`).
- The backend stops with *"node follows another chain"*: the node behind the
  shim is not on the Bitcoin BLAKE2b chain. Fix the node; do not work around
  the check.
