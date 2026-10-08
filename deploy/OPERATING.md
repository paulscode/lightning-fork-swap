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
to a working float and the rest cold. The pair limit (`MAX_SWAP_SAT`, 1,000,000
sat to start) caps any single swap.

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
   $K getnewaddress                   # send coins here
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
| lnd can send little; hot wallet growing | mostly submarine swaps | move hot wallet coins into channels: `$K sendtoaddress <lnd newaddress> <BTC>`, then open a channel; or run a **reverse** swap through the service yourself (pay from your own node, receive on chain), which turns the service's inbound back into outbound |
| lnd can receive little, or hot wallet low | mostly reverse swaps | add to the hot wallet; or run a **submarine** swap through the service yourself (refills the hot wallet and the inbound together); or get another inbound channel |
| a channel never moves | a poor peer | close it (`$L closechannel --funding_txid ... --output_index ...`); the coins return to lnd's on-chain wallet |
| both directions low | the service has outgrown its capital | add capital, or lower `MAX_SWAP_SAT` meanwhile |

Running swaps through the service yourself is the cleanest rebalancing: it
uses the service's own paths and costs only miner fees (the service fee goes
to the service). Use your own wallets and nodes, never a user's.

After closing or opening channels, copy `channel.backup` offline again.

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
days and since the start: swaps and volume, service fees earned, costs (miner
fees of claims and lockups, Lightning routing fees paid), routing fees lnd
earned, and the net. Every Monday at 13:00 UTC it also goes to Telegram.

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

## 7. Routine

| When | What |
| --- | --- |
| As it comes | Telegram alerts: each says what is wrong; the README's Alerts section lists them |
| Weekly | the Monday report: rebalance (§4), adjust fees (§5) |
| After any channel open or close | copy `channel.backup` offline |
| Monthly, or as you like | take earnings out (§6) |
| Before raising limits or the hot wallet | weeks without trouble |

## 8. Before opening the service to users

1. Funding as in §3: lnd's on-chain wallet, outbound channels, inbound, the
   hot wallet, the monitor floors.
2. `channel.backup` copied offline.
3. Fees and limits confirmed in `.env` (and the services restarted if
   changed).
4. A real swap each way from your own wallets, through the site: a reverse
   swap (pay from your node, receive on chain) and a submarine swap (lock on
   chain, receive on your node). Check the report afterwards.
5. Open the site: `: > /etc/nginx/lfswap-preview.conf && systemctl reload nginx`.
