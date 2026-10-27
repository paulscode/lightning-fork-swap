# Operating Lightning Fork Swap: money, liquidity and fees

What the service does with money on its own, what the operator has to do,
first and then over time, and how to take the earnings out. Commands assume
`ssh root@HOST`, then `cd /opt/lfswap/deploy`. The shorthands:

```sh
K="docker compose exec -T knots bitcoin-cli -datadir=/data -rpcwallet=boltz"   # the hot wallet
L="docker compose exec -T lnd lncli"                                          # the Lightning node
python3 monitor/lfswap_monitor.py --report                                    # balances, capacity, earnings
```

The runbook (`README.md`) covers installing, moving hosts and backups.

## 1. Where the money is, and how swaps move it

The service holds funds in three places:

| Place | What it is | Used by |
| --- | --- | --- |
| **Hot wallet** | Knots wallet `boltz` | reverse swaps lock up from it; submarine swaps are claimed into it |
| **lnd's channels** | the Lightning node's balance on its side of each channel ("can send") and the peer's side ("can receive") | submarine swaps send through it; reverse swaps receive through it |
| **lnd's on-chain wallet** | the Lightning node's own coins | opening channels, fee bumps when a channel closes |

What each kind of swap does:

| Swap | The user | The service's hot wallet | lnd can send | lnd can receive |
| --- | --- | --- | --- | --- |
| **Submarine** (chain to Lightning) | locks coins on chain, gets paid over Lightning | **grows** (the claim) | **shrinks** | grows |
| **Reverse** (Lightning to chain) | pays over Lightning, claims coins on chain | **shrinks** (the lockup) | grows | **shrinks** |

So each kind of swap refills what the other uses up. When demand is lopsided,
one side runs dry and the operator moves funds back (§4).

The service's fee (`SUBMARINE_FEE_PERCENT`, `REVERSE_FEE_PERCENT` in `.env`)
stays with the service as part of these movements: a submarine swap's fee is
in the claim (more on chain than was paid on Lightning); a reverse swap's fee
is in the invoice (more on Lightning than was locked up on chain). Miner fees
for the service's own transactions are charged to the user up front.

## 2. What the service does by itself, and what it does not

By itself:
- Refuses a **reverse** swap when the hot wallet's confirmed balance cannot
  cover the lockup ("insufficient liquidity"); nothing is lost.
- Refuses a **submarine** swap when lnd finds no route to the invoice, which
  includes having too little to send.
- Claims, refunds and settles every swap; retries what failed.
- Reports to Telegram (`monitor/lfswap_monitor.py`, every 5 minutes) what
  needs a person, and sends a report every Monday (§6).

Not by itself (the operator's work):
- Putting money in: the hot wallet, lnd's on-chain wallet, channels.
- Getting inbound capacity (someone else's money on their side of a channel
  to the swap node), which reverse swaps need.
- Moving funds between the hot wallet and the channels when demand is
  lopsided.
- Setting fees and limits.
- Taking earnings out.
- Copying `channel.backup` offline after every channel change.

## 3. First funding, step by step

Sizes are yours to choose; the rule of thumb: the **hot wallet is the most
the service can lose** to a bug or an attack on the reverse path, so keep it
to a working float and the rest cold. The pair limit (`MAX_SWAP_SAT`) caps
any single swap; size it to what the network's channels can carry (a swap
larger than the user's own channels cannot be paid anyway).

1. **Fund lnd's on-chain wallet**, enough for the channels you will open plus
   a small reserve (anchor channels keep about 10,000 sat per channel for fee
   bumps):
   ```sh
   $L newaddress p2tr                 # send coins here from your wallet
   $L walletbalance                   # wait for confirmed_balance
   ```
2. **Open channels to well-connected nodes** (this is the "can send" side,
   for submarine swaps). Prefer nodes with many channels and much capacity,
   reachable over clearnet, and check the graph first:
   ```sh
   # the ten nodes with the most channels
   $L describegraph | jq -r '[.edges[] | .node1_pub, .node2_pub] | group_by(.)
     | map({n: length, pub: .[0]}) | sort_by(-.n) | .[:10][] | "\(.n) \(.pub)"'
   $L connect <pubkey>@<host>:<port>                     # if not already a peer
   $L openchannel --node_key <pubkey> --local_amt <sat> --sat_per_vbyte <n>
   $L pendingchannels                                   # 3 confirmations, then active
   ```
   Channels at least as large as `MAX_SWAP_SAT` let a swap go in one path.
3. **Get inbound capacity** (the "can receive" side, for reverse swaps). A
   new node has none: every channel it opens is all on its side. Ways to get
   it:
   - have a node you control open a channel **to** the swap node (its funds,
     on its side, spendable back through the service); or ask a well-connected
     peer to open one;
   - or, once there are outbound channels, run a **submarine swap through the
     service yourself**: you lock coins on chain from your own wallet, the
     service pays an invoice of your own node over Lightning. The swap node's
     channels then have inbound, and the hot wallet is funded by the claim, in
     one move (you pay the service's fee to yourself, plus miner fees).
4. **Fund the hot wallet** for reverse swaps:
   ```sh
   $K getnewaddress "" bech32m        # send coins here (Taproot; the default is the old 1... kind)
   $K getbalances                     # mine.trusted is what the service can use
   ```
5. **Set the alert floors** in `.env` (sat), so Telegram tells you before a
   side runs dry:
   ```
   MONITOR_MIN_WALLET_SAT=...         # hot wallet
   MONITOR_MIN_OUTBOUND_SAT=...       # lnd can send
   MONITOR_MIN_INBOUND_SAT=...        # lnd can receive
   ```
   A floor of about two maximum swaps is a reasonable start. No restart is
   needed; the monitor reads `.env` each run.
6. **Copy `channel.backup` offline** now that channels exist:
   `/srv/lfswap/secrets/channel.backup` (refreshed hourly from lnd).
7. **Check** `python3 monitor/lfswap_monitor.py --report`: "What swaps can
   take now" should show room in both directions.

## 4. Keeping both directions open

Watch the weekly report and the floor alerts. Then:

| What you see | Why | What to do |
| --- | --- | --- |
| lnd can send little; hot wallet growing | mostly submarine swaps | move hot wallet coins into channels: `$K sendtoaddress <lnd newaddress p2tr> <BTC>`, then open a channel; or run a **reverse** swap through the service yourself (pay from your own node, receive on chain), which turns the service's inbound back into outbound |
| lnd can receive little, or hot wallet low | mostly reverse swaps | add to the hot wallet; or run a **submarine** swap through the service yourself (refills the hot wallet and the inbound together); or get another inbound channel |
| a channel never moves | a poor peer | close it (`$L closechannel --funding_txid ... --output_index ...`); the coins return to lnd's on-chain wallet |
| both directions low | the service has outgrown its capital | add capital, or lower `MAX_SWAP_SAT` meanwhile |

Running swaps through the service yourself is the cleanest rebalancing: it
uses the service's own paths and costs only miner fees (the service fee goes
to the service). Use your own wallets and nodes, never a user's.

After closing or opening channels, copy `channel.backup` offline again.

### Automatic moves between the two on-chain wallets

`rebalance/lfswap_rebalance.py` (every 30 minutes) moves coins between the
hot wallet and lnd's on-chain wallet when one runs low and the other has
coins to spare, or when the hot wallet holds more than it can use. It does
not touch channels: the table above stays a person's work.

Each wallet has a floor, a target and a ceiling (`REBALANCE_*` in `.env`;
lnd's are above its anchor reserve). The hot wallet's target never exceeds
lnd's inbound capacity, all reverse swaps can use. A run makes at most one
move, which fills the receiving wallet to its target without taking the
other below its own, so the next run finds nothing to do until swaps have
moved the balances past a floor or a ceiling again. Brakes: at least
`REBALANCE_MIN_MOVE`, 12 hours between moves, two a day and half of both
wallets a day at most (a bigger need is met over several days), fees at most
`REBALANCE_FEE_CAP` sat/vB (an urgent move, the hot wallet below one
maximum swap or lnd below its reserve, up to `REBALANCE_FEE_CEILING`), one
move in flight at a time (bumped if unconfirmed after 6 blocks), and no hot
wallet spend while a reverse swap is about to lock up. Coins only go to a
fresh address the receiving wallet confirms as its own, or to
`REBALANCE_COLD_ADDRESS` (off unless set, and refused unless it matches
`REBALANCE_COLD_ADDRESS_SHA256`).

```sh
python3 rebalance/lfswap_rebalance.py --status     # balances, bands, what a run would do
tail /srv/lfswap/rebalance/rebalance.log
python3 rebalance/simulate.py --set REBALANCE_HOT_TARGET=8000000   # try numbers on simulated demand
```

Start with `REBALANCE_MODE=dry-run`: each decision goes to Telegram ("would
move ...", once a day while it holds) and nothing moves. After a couple of
weeks of sensible messages, set `on`. The weekly report has a "Rebalancing"
block (moves, fees, time below each floor). A move that conflicts with
another transaction stops all moves until you look and remove `"blocked"`
from `/srv/lfswap/rebalance/state.json`.

## 5. Using fees to improve capacity

Two kinds of fee, two levers:

- **Swap fees** (in `.env`, percent): `SUBMARINE_FEE_PERCENT` and
  `REVERSE_FEE_PERCENT`. If one direction drains the service faster than the
  other, make that direction a little dearer and the other cheaper; demand
  moves to where the service has room. After changing them:
  `./scripts/setup.sh && docker compose restart boltz`.
- **Routing fees** (lnd's channel policies): others route payments through
  the swap node and pay for it, which also moves balance between its
  channels. Raise fees on channels whose outbound you want to keep, lower them
  on channels that are too full:
  ```sh
  $L feereport
  $L updatechanpolicy --base_fee_msat 1000 --fee_rate_ppm 500 --time_lock_delta 80 --chan_point <txid:index>
  ```
- **Limits**: raise `MAX_SWAP_SAT` only as liquidity allows (a swap larger
  than what a side holds is refused anyway, but users see the limit), and
  only once the service has run clean for a while.

## 6. Earnings, and taking them out

`python3 monitor/lfswap_monitor.py --report [--days N]` shows, for the last N
days and since the start: swaps and volume, service fees earned, network
fees (what users paid for them in their quotes, and what the service spent:
miner fees of claims and lockups, Lightning routing fees), routing fees lnd
earned, and the net. Every Monday at 13:00 UTC it also goes to Telegram.

The report ends with a "Sizing" block from the telemetry (the README's
Telemetry section): how much the hot wallet held against what reverse swaps
needed at once, the net flow and how long the busier side lasts at that
rate, refused swaps by kind, channels that carried none of our payments,
and any change of terms in the week. `--analysis [--days N]` gives the same
over a longer period; base the floors, bands, limits and fees on it.

The earnings are not in a separate pot: they are part of the balances. Keep
a note of what you put in; what the service holds beyond that (the report's
"total" minus capital in) is profit.

To take profit out, or anything above the floats you want:

- **From the hot wallet** (the simplest; submarine fees accumulate here):
  ```sh
  $K getbalances
  $K sendtoaddress <your cold address> <BTC>      # leave the float in place
  ```
- **From the Lightning side**: pay an invoice of your own node from the swap
  node (`$L payinvoice <invoice>`), or close a channel and send lnd's on-chain
  coins out (`$L sendcoins --addr <address> --amt <sat>`). Paying out over
  Lightning also gives the swap node inbound, which reverse swaps use.

Never take the hot wallet below `MONITOR_MIN_WALLET_SAT`, nor lnd below the
outbound floor, unless you mean to shrink the service.

## 7. A refund that does not confirm

When a reverse swap times out, the service refunds its lockup to the hot
wallet. If that refund is still unconfirmed 20 blocks after the swap's
timeout, the alert says so ("is not confirmed ... blocks after the swap's
timeout"). It matters: about 42 blocks after the timeout lnd gives the
user's Lightning payment back, and the user could then still claim the
lockup. A refund that fell out of the mempool is sent again by itself;
one stuck at too low a fee needs a push. Spend its output to the wallet at
a higher fee (the wallet pays the refund's shortfall too, so the pair
confirms together):

```sh
$K gettransaction <refund txid> | jq '.details[] | {vout, amount}'
$K -named sendall recipients='["'$($K getnewaddress "" bech32m)'"]' \
  inputs='[{"txid":"<refund txid>","vout":<vout>}]' fee_rate=<sat/vB>
```

## 8. Donations

Donations arrive at one address of lnd's on-chain wallet, so they are lnd's
coins like any others. The promise on the site: they are used for
liquidity and its on-chain fees only (channels, inbound, the fees of
moving funds), and nothing is refunded. They are not earmarked: spend them
as part of lnd's on-chain wallet.

### Turning donations on

```sh
$L newaddress p2tr                      # once; this is the donation address
```

1. Put it in `.env` as `DONATION_ADDRESS=`, and build the web app with the
   same address: `VITE_DONATION_ADDRESS=<address> bun run mainnet && bun run
   build` (the donation window shows it; without it there is only the
   "Open a channel" tab).
2. `./scripts/init-donations-db.sh` (once; safe to run again) and
   `./scripts/bake-macaroon.sh` (bakes `donations.macaroon`, which may only
   list lnd's transactions, if it is missing).
3. `docker compose up -d donations-worker donations-api`, then
   `curl -s 127.0.0.1:9010/donate/v1/health`.

The worker finds each transaction paying the address and checks whether it
was sent with replay protection; the API tells the donation window what it
found. The monitor alerts when either is down, when the worker stops
passing, and once for each donation that needs a look; the weekly report
has a "Donations" block.

### Donations sent without replay protection

Coins created before the fork (block 961640) exist on both chains. A wallet
that does not sign with replay protection (the `SIGHASH_UNIFIED` bit)
makes a transaction that is also valid on the SHA256 chain: anyone can copy
it there, and the donor's SHA256-chain coins then land at our donation
address on that chain. We never copy one ourselves. The worker records
each donation's verdict:

| Verdict | Meaning |
| --- | --- |
| `protected` | cannot be replayed: signed with the bit, or its coins can never exist on the SHA256 chain (followed back, their history reaches coins mined after the fork, a transaction signed with the bit, or a coin the SHA256 chain spent differently; a coin made after the fork is not enough on its own, since the transactions that made it may be copied there first) |
| `at_risk` | can be copied to the SHA256 chain now; rechecked hourly for 30 days, then daily for a year |
| `replayed` | it was copied: we hold the donor's SHA256-chain coins at the donation address there |
| `unknown` | scripts it cannot judge, or the explorers did not answer; rechecked, then look by hand |

```sh
D="docker compose exec -T donations-worker /donations"
$D replay list                          # at_risk, replayed and unknown (--at-risk, --replayed, --unknown, --all)
$D replay show <txid>                   # inputs, hash types, the donor's at-risk addresses
$D replay note <txid> "<text>"          # contact, return txid, anything worth keeping
$D check <txid>                         # check any transaction now, without recording it
```

### Giving SHA256-chain coins back (case by case)

Nothing is promised; returns are a courtesy, decided case by case, and only
after proof of ownership. Return the amount received on the SHA256 chain
less the miner fee, once per donation transaction, and note it.

1. **Proof.** The donor signs a message with the key of one of the
   donation's input addresses (`$D replay show` lists them): BIP-322 for
   SegWit and taproot addresses, the classic `signmessage` for legacy ones.
   The message names the donation txid and the SHA256-chain address to
   return to. Check it:
   ```sh
   $D replay verify <txid> <input address> "<message>" <signature>
   ```
2. **Spend our BLAKE2b side first.** The replayed transaction created the
   same output on both chains. A return signed the ordinary way on the
   SHA256 chain is also valid on the BLAKE2b chain while our output there
   is unspent, so anyone could copy the return onto the BLAKE2b chain and
   send our real donation to the donor. Check the BLAKE2b explorer
   (`https://mempool.guide/api/tx/<txid>/outspend/<vout>`): if the output
   is still unspent, move it within lnd first (lnd signs with replay
   protection) and wait for a confirmation:
   ```sh
   $L sendcoins --utxo <txid>:<vout> --sweepall --addr $($L newaddress p2tr | jq -r .address) --sat_per_vbyte <n>
   ```
3. **The key, offline.** lnd signs everything with `SIGHASH_UNIFIED`, which
   the SHA256 chain refuses, so the return is signed elsewhere. On an
   offline machine, with lnd's seed (`/srv/lfswap/secrets/lnd-seed.txt`,
   or the paper copy), derive the donation address's key. Its path is the
   `derivation_path` that `$L wallet addresses list` shows for it (BIP86,
   like `m/86'/0'/0'/0/3`). With
   [chantools](https://github.com/lightninglabs/chantools):
   ```sh
   chantools derivekey --path "<derivation_path>"   # asks for the seed; prints the key (WIF)
   ```
4. **The return, on the SHA256 chain.** In a Bitcoin Core or Knots node of
   the SHA256 chain (never one of the BLAKE2b chain), check the key first:
   the descriptor `tr(<WIF>)` must give the donation address, or stop.
   Then a wallet with that key alone:
   ```sh
   bitcoin-cli getdescriptorinfo "tr(<WIF>)"              # its public "descriptor" and the "checksum"
   bitcoin-cli deriveaddresses "<public descriptor>"     # must be [ "<donation address>" ]
   bitcoin-cli -named createwallet wallet_name=return blank=true
   bitcoin-cli -rpcwallet=return importdescriptors '[{"desc": "<tr(WIF) with its checksum, from getdescriptorinfo>", "timestamp": <time of the replay block>}]'
   bitcoin-cli -rpcwallet=return -named send outputs='{"<donor address>": <amount>}' \
     inputs='[{"txid": "<donation txid>", "vout": <vout>}]' add_inputs=false \
     subtract_fee_from_outputs='[0]' fee_rate=<sat/vB>
   ```
   Only that output: other donors' replayed coins at the same address stay
   out of it (and each needs step 2 before it is ever spent).
   Then `unloadwallet return`, delete the wallet and the key's notes, and
   `$D replay note <txid> "returned <SHA256 txid> to <address>"`.

The rest of the SHA256-chain coins (replays nobody asked about) stay where
they are; the donation address is never reused for anything else.

### Channel donations

A donor can ask for their donation to open a channel from our node to
theirs. Each such donation gets its own lnd address; once 3 confirmations
bring at least 1,000,000 sat plus the funding fee, the worker opens a
public channel from exactly those coins (all of them, up to
`CHANNEL_MAX_SAT`, the rest staying in lnd's wallet). It is best effort and
nothing is refunded: if the channel cannot be opened (the node unreachable
for 48 hours, or waiting 7 days for the donor to fix its details), the
coins stay in lnd's wallet as a general donation.

What keeps it safe: the worker (`donations-channels`) has no listening
port and a macaroon that lnd honours only while the guard
(`donations-guard`) checks every call made with it. The guard allows only
the calls the worker needs, and opens only from coins leased to the
donations (or released by them a moment before), with nothing pushed to
the peer, no address of theirs to close to, a `donation:<id>` memo, a
capped size and fee rate. If the guard stops, lnd refuses the worker's
macaroon and nothing else. The public API only inserts an order or an edit
request into the database; it never talks to lnd.

It is off by default. lnd must run with its middleware chain on
(`rpcmiddleware.enable=true`, which `setup.sh` renders; lnd reads it when
it starts, so after the first `setup.sh` with it, restart lnd once). Then:

```sh
./scripts/channel-donations.sh status    # the keys, lnd.conf, the guard, the API
./scripts/channel-donations.sh on        # bakes any missing macaroon, starts the guard and the
                                         # worker, adds the routes; refuses (and stays off) if
                                         # the guard cannot register with lnd
./scripts/channel-donations.sh off       # stops them, takes the routes away
```

It sets `CHANNEL_DONATIONS` and `COMPOSE_PROFILES` in `.env` (the monitor
reads them too). Orders in progress stay in the database when it is
turned off; their leased coins return to lnd's wallet when the leases end
(14 days), or at once with
`$L wallet releaseoutput --lockid 958afdf67e885dc76265f3739ff7500f25f8efdb14e6d0b28d4ea01002c3971c <txid:index>`.

```sh
D="docker compose exec -T donations-channels /donations"
$D orders list [--all]                  # the active ones (or all)
$D orders show <id>                     # the order and its timeline
$D orders retry <id>                    # try again now (a node that is back)
$D orders fallback <id>                 # end it as a general donation
```

The monitor alerts when the worker stops passing, an order is stuck
before its channel, a funding transaction is unconfirmed after 6 hours
(bump it with `$L wallet bumpfee`), half of the 100 unpaid orders allowed
are waiting, once when a donor needs to act, and at once if a channel we
opened says it is a donation that has no such channel, or pushed coins to
the peer. On either of the last two: `./scripts/channel-donations.sh off`,
and revoke the worker's macaroon (`$L deletemacaroonid 3`) before looking.

### SHA256-chain donations

An API (`/donate/v1/sha256`, described in `donations/SHA256-API.md`)
through which anyone, usually another tool, donates coins on the SHA256
chain. Each request gets an address of its own. What the SHA256 chain pays
it is kept, in a hot wallet for now. What reaches the same address on the
BLAKE2b chain is passed on once it is 6 blocks deep: to the request's
return address if it gave one, or else, together with the others, to
`DONATION_ADDRESS` in lnd's wallet as a liquidity donation. Coins usually
reach the BLAKE2b chain by themselves: a transaction valid on both chains
is relayed on both, since they share peers. They do whenever every coin it
spends exists on both chains (coins from before the fork, or made by
transactions that were themselves copied) and none of its signatures
opts out. A donor whose coins were already protected sends their BLAKE2b
coins separately, and those are passed on the same way.

Every forward is signed with `SIGHASH_UNIFIED`, and before it is relayed
each input is checked to pass the BLAKE2b chain's rules and to fail the
SHA256 chain's. Without that, the forward could be copied to the SHA256
chain, where the same coins are the donation, and would hand the donation
to the return address.

A request may ask (`"replay": true`, with a return address) for the
donation to be copied to the BLAKE2b chain when it has not got there by
itself. The worker does that only with a BIP322 proof from each of the
donation's input addresses, never for an earlier transaction (that would
move someone else's coins), and only after checking that it is not there
already.

What keeps it safe: the worker (`donations-sha256`) signs with a seed of
its own, `$LFSWAP_ROOT/secrets/sha256-donations.seed`, and logs in to Knots
as `donations`, which may read the chain and relay transactions and call
nothing of any wallet: the hot wallet for swaps is out of its reach. It has
no listening port and no lnd macaroon. The public API holds no key; it
inserts a request or a hint into the database and nothing else.

It is off by default. Knots must know the `donations` user: run
`./scripts/setup.sh` (it also makes the seed, once), then restart Knots
once, with swaps paused (`./scripts/migrate/maintenance.sh on`, restart,
wait for lnd and the backend to reconnect, `off`). Then:

```sh
./scripts/sha256-donations.sh status    # Knots' config, the seed, the worker, the API
./scripts/sha256-donations.sh on        # starts the worker, adds the routes; refuses (and
                                        # stays off) if the worker cannot reach Knots
./scripts/sha256-donations.sh off       # stops it, takes the routes away
```

**Keep a copy of the seed offline** before the first donation: it holds the
SHA256-chain donations, and the BLAKE2b coins waiting to be passed on.
`backup-secrets.sh` and the offsite backup take it with the other secrets;
also print it, or write it down (`cat $LFSWAP_ROOT/secrets/sha256-donations.seed`).

```sh
S="docker compose exec -T donations-sha256 /donations"
$S sha256 list [--all]                  # requests with deposits (or all)
$S sha256 show <id>                     # one request: deposits, forwards, copies
$S sha256 export                        # the wallet as a descriptor (public)
$S sha256 export --private              # the same with its private key, for
                                        # a SHA256-chain wallet (Sparrow)
```

**Locks.** Once a SHA256-chain donation is 3 deep, the worker spends it on
the SHA256 chain into the hot wallet (the seed's second branch,
`m/86'/0'/0'/1/j`) in a lock: a transaction with a 103-byte OP_RETURN,
which the BLAKE2b chain's data rules (RDTS) refuse in consensus. A
BLAKE2b deposit is passed on only after its SHA256 twin's lock is 6 deep,
checked again at that moment with every SHA256 explorer configured
(`DONATIONS_SHA256_EXPLORERS`; all must agree); or, with no twin, once the
transaction that paid it cannot be valid on the SHA256 chain, as signed
(its scripts are run under both chains' rules) or by its history. Then nothing that spends it, however it is signed, by us
included, can ever be valid on the SHA256 chain, and anyone can check that
with the lock's txid, which the API gives. The worker relays locks through
the SHA256 explorers, whose Bitcoin Core (30 or later) relays data outputs
of this size; Knots does not by default. A lock no explorer takes is
alerted and tried again an hour later. The data rules end on 1 September
2027; from a month before, the worker makes no new lock, holds what would
need one, and alerts once a day. Locks are chained: each carries an anchor output (1,000 sat, output 1) that
the next lock spends, one lock in flight at a time. A lock is permanent
once one of its inputs can never exist unspent on the BLAKE2b chain: a
twin it locked was passed on (spent there), or its anchor came from a
permanent lock. From then on every later lock is permanent too, data rules
or not. From a month before the rules end, the worker locks only behind a
permanent anchor. From three months before, it alerts once a day with the
number of locks not yet permanent: after that date such a lock could be
mined on the BLAKE2b chain and take its twin to the hot wallet, then the
sweep address, instead of to its donor. Before then, pass those twins on
by hand, or send them back to their donors. Sweeps never spend anchors.

Payments under 5,000 sat to a request's address, on either chain, are
kept as they are (state `kept`): never locked nor passed on. A request
takes at most 50 deposits, and a lock, forward or sweep at most 100
inputs (the rest wait for the next).

**Daily sweeps.** With `SHA256_SWEEP_ADDRESS` set in `.env` (an address
of yours on the SHA256 chain), the worker sends the locks' outputs there
once a day when they come to `SHA256_SWEEP_MIN_SAT` (50,000 sat), at the
explorers' 6-block fee estimate (not above `SHA256_SWEEP_MAX_FEE`, 50
sat/vB, which also caps locks). A lock's output can never exist on the
BLAKE2b chain, so neither can a sweep of it; our BLAKE2b node must refuse
each sweep for a missing coin before it is sent. After changing the
address: `docker compose up -d donations-sha256`.

**Spending the SHA256-chain coins by hand.** The locks' outputs (branch
`m/86'/0'/0'/1/*` of `sha256 export`) can never exist on the BLAKE2b
chain, so anything spending them is safe there. A donation not yet locked
(`sha256 show`) is not: leave it to the worker.

The monitor alerts when the worker stops passing, sends a message for each
SHA256-chain donation, each forward, each copy and each sweep, one when a
forward or a sweep could not be relayed, and one while the SHA256-chain
coins held (not yet swept) pass `MONITOR_SHA256_MAX_SAT` (5,000,000 sat).

## 9. Routine

| When | What |
| --- | --- |
| As it comes | Telegram alerts: each says what is wrong; the README's Alerts section lists them (a late refund: §7; donations: §8) |
| Weekly | the Monday report: rebalance (§4), adjust fees (§5) |
| After any channel open or close | copy `channel.backup` offline |
| Monthly, or as you like | take earnings out (§6) |
| Before raising limits or the hot wallet | weeks without trouble |

### Logs and users' privacy

The client's address stops at nginx: the backend, the sidecar and the
donations API never receive it. nginx keeps two logs:

| Log | What a line holds | Kept |
| --- | --- | --- |
| `/var/log/nginx/access.log` | time, a client tag, method, the path without swap ids, txids, addresses or invoices, status, sizes, timings, the kind of user agent | 10 days |
| `/var/log/lfswap/threat.log` | what was refused as hostile (scanner paths, malformed paths, probes of the API, other host names): the address, the path, the user agent; a client a limit refused: its tag only | 30 days |

The tag is an HMAC of the address under a key that `scripts/log-key.sh`
replaces at midnight (cron): one client has one tag for the day, so a bot
or a flood still shows, and after the day nobody can tell which address
it was. nginx's error log is kept 10 days and no longer gets a line per
rate-limited request. The backend's own log files (`boltz.log`,
`sidecar.log`) are rotated daily and kept 10 days. The containers log to
the journal (`docker compose logs` as before; `journalctl -t
lfswap/<container>` too), which keeps 10 days; lnd writes no log file of
its own.

Looking into traffic: count and describe by tag (`awk '{print $2}'
access.log | sort | uniq -c`); copy no addresses into notes.

The databases: `retention/lfswap_retention.py` (daily, log in
`/srv/lfswap/retention/retention.log`) removes the user's data from a swap
90 days after its last change, once nothing is left to claim or refund (we
claimed it, it was never paid, or its lockup is spent; for a reverse swap:
the user claimed it, we refunded, or we never locked up). The invoice
(its payment hash stays), preimage, the user's key and address and the
script go; amounts, fees, status and dates stay. Any other swap stays
whole, however old. With `RETENTION_MODE=report` (the default) it only
counts swaps, and alerts once any is past its time; set `on` after a
rehearsal on regtest, before the first swaps qualify (January 2027).
In either mode: donations lose a protected donation's inputs after 30
days, an at-risk one's inputs and addresses after 400 days (a replayed one
is kept for its return), SHA256 proofs 90 days after their copy was
decided; and swaps no longer record whether they came from a phone.

Alerts name a donor's address by its first 10 characters only.

## 10. Before opening the service to users

1. Funding as in §3: lnd's on-chain wallet, outbound channels, inbound, the
   hot wallet, the monitor floors.
2. `channel.backup` copied offline.
3. Fees and limits confirmed in `.env` (and the services restarted if
   changed).
4. A real swap each way from your own wallets, through the site: a reverse
   swap (pay from your node, receive on chain) and a submarine swap (lock on
   chain, receive on your node). Check the report afterwards.
5. Open the site: `: > /etc/nginx/lfswap-preview.conf && systemctl reload nginx`.
