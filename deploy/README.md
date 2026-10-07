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
8. `./scripts/install-crons.sh`: the hourly copy of the recovery files, the
   alert monitor every five minutes and, once `BACKUP_AGE_RECIPIENT` is set,
   the daily offsite backup.

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

## Alerts

`monitor/lfswap_monitor.py` runs every five minutes from
`/etc/cron.d/lfswap-monitor` and reports what needs a person: a container
down; no block for two hours, the node behind, or a lost branch of two or
more blocks; lnd unsynced, without peers, or holding an HTLC within 24 blocks
of its expiry; a reverse swap within 30 blocks of its timeout that the user
has not claimed; a submarine swap paying for over an hour, waiting for its
claim, or paid while recorded as failed; backend log lines such as a lockup
that may have been broadcast before an error; the wallet below
`MONITOR_MIN_WALLET_SAT`; the disk over 90 %, the host stalling on IO or
memory; a `channel.backup` the hourly copy missed; the TLS certificate close
to expiry. It also says once a day that it runs, so silence means it does not.

Set `ALERT_WEBHOOK_URL` and `ALERT_WEBHOOK_KIND` in `.env` (see
`.env.example`), then check delivery:

```sh
python3 monitor/lfswap_monitor.py --test       # one test message
python3 monitor/lfswap_monitor.py --dry-run    # what it would report now
tail /srv/lfswap/monitor/monitor.log
```

An alert is sent when it appears, repeated every six hours while it holds,
and reported once more when it clears. A log line is reported once.

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
The code is AGPL-3.0, so users of the site must be able to download its
source. The site serves it at `/source/` (the app's footer and Terms page
link there). After every deployment of a new backend or web app build,
rebuild the archives from the workstation, with the deployed commits checked
out:

```sh
deploy/scripts/build-source-bundle.sh "" --upload root@lightningfork.com
```

It refuses to run with uncommitted changes, so the archives always match a
commit. Publishing the repositories on GitHub as well is optional.

## Moving to another host

The service can move to a new host with its channels, swaps in progress and
wallets intact, in under a minute of downtime once the chain has been copied
ahead of time. The scripts are in `scripts/migrate/`. They are ordered around
one rule: **lnd must never run on two hosts with the same channel
database**. The old host is stopped and retired before the archive exists,
and it cannot start lnd again (a tombstone, and a `guard` service in the
compose file that lnd, Knots and the backend depend on).

On the **new host** (Debian 12):

```sh
# a fresh Debian has no rsync: copy the repository with tar, without the
# private notes and the source trees the host does not need
tar -czf - -C /path/to/lightning-fork-swap --exclude=.git --exclude=internal_docs \
    --exclude=references --exclude=backend --exclude=webapp --exclude=e2e . \
  | ssh root@NEW 'mkdir -p /opt/lfswap && tar -xzf - -C /opt/lfswap'
ssh root@NEW /opt/lfswap/deploy/scripts/provision-host.sh     # prints an age public key
ssh root@NEW /opt/lfswap/deploy/scripts/install-host.sh firewall   # confirm from a 2nd session
docker save lfswap/boltz:dev | gzip | ssh root@NEW 'gunzip | docker load'
```

The **old host** needs `age` (`apt-get install age`, or run
`provision-host.sh` there, which is safe on a running host) and root SSH
access to the new one for the move (remove it afterwards).

Days ahead: lower the DNS TTL to 300 s, and from the **old host** copy the
chain while it runs:

```sh
scripts/migrate/copy-chain.sh --pre root@NEW
```

The move, from the **old host**:

```sh
scripts/migrate/maintenance.sh on          # new swaps refused; others carry on
scripts/migrate/export.sh --check          # nothing times out within ~6 hours?
scripts/migrate/export.sh --recipient AGE_KEY_OF_NEW_HOST
scripts/migrate/copy-chain.sh --final root@NEW
scp /srv/lfswap/migration/lfswap-*.tar.gz.age* root@NEW:/srv/lfswap/migration/
```

and on the **new host**:

```sh
scripts/migrate/import.sh /srv/lfswap/migration/lfswap-ID.tar.gz.age --public-ip NEW_IP
```

`import.sh` checks the archive, puts the state in place (with the TLS
certificate, so HTTPS works as soon as DNS points here), starts everything in
order and compares the result with a snapshot taken before the export: lnd
identity, balances, channels, the Knots wallet, the backend seed and every
swap. It also installs the backup cron jobs (export removed them from the old
host). Then switch the DNS A records; maintenance is off on the new host
already. Peers find the node's new address through gossip; one without a
public address of its own has to connect to the node again itself
(`lncli connect <pubkey>@NEW_IP:9735`).

The archive is encrypted to the new host's age key: only that host can open
it. Keep the old host (stopped) for a couple of weeks, then destroy it.

To go back, move again in the other direction: export on the new host to the
old host's key, and import there with `--returning`. Never restart the old
copy: the new host's lnd may have moved the channels on since.

## Backups and disaster recovery

`scripts/backup-offsite.sh` makes a backup without stopping anything:
Postgres, the backend seed, the Knots wallet (as a wallet file), lnd's
`channel.backup`, the seeds, `.env` and the certificate. It is encrypted to
`BACKUP_AGE_RECIPIENT` (your age public key; keep the private key offline) and
copied to `BACKUP_DEST`. Run it daily from cron:

```sh
echo "40 3 * * * root /opt/lfswap/deploy/scripts/backup-offsite.sh" > /etc/cron.d/lfswap-offsite
```

If the host is lost: provision a new one, copy a pruned chain to it from a
node you trust (or let it sync, slowly), then

```sh
scripts/migrate/restore.sh BACKUP --identity YOUR_AGE_KEY --public-ip NEW_IP
```

The node comes back with the same identity and wallets and every swap up to
the backup. Its channels cannot be resumed safely without the live database:
their peers close them, and the funds return on chain after each channel's
delay. Open new channels afterwards.

## Rehearsing

`rehearsal/rehearse.sh` runs all of the above on three throwaway KVM VMs on a
workstation, in regtest: a migration with a reverse swap and a submarine swap
in flight, a move back, and a lost host restored from backup with its channel
funds recovered. It never touches production.

## Tests

On a workstation with Docker, none of them touching production:

```sh
deploy/nginx/test/run.sh                    # the nginx config, in the host's nginx 1.22.1
python3 -m unittest discover -s deploy/monitor
(cd shim && go test -race ./...)
deploy/regtest/bootstrap.sh                 # the service on regtest
(cd e2e && node run.mjs)                    # swaps, refunds and failures through the API
# the web app through the production nginx config, in Chrome:
(cd webapp && bun run regtest && VITE_API_URL=https://localhost:18443 \
  npx vite build --outDir /tmp/lfs-ui-dist)
deploy/regtest/ui-nginx.sh up /tmp/lfs-ui-dist
(cd e2e && node ui.mjs)
deploy/rehearsal/rehearse.sh                # a move, a rollback and a restore, on VMs
```

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
