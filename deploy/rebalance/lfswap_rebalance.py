#!/usr/bin/env python3
"""Keeps the two on-chain wallets of Lightning Fork Swap right-sized: the
Knots hot wallet (reverse swap lockups) and lnd's on-chain wallet (channel
opens, its anchor reserve). Run every 30 minutes by cron
(/etc/cron.d/lfswap-rebalance); safe to run by hand. Python 3 standard
library only.

Each wallet has a floor, a target and a ceiling. A run makes at most one
move, by the first of these that applies:

  1. hot wallet below its floor, lnd above its target: lnd -> hot, up to
     the hot wallet's target, never taking lnd below its target
  2. lnd below its floor, hot wallet above its target: hot -> lnd, likewise
  3. hot wallet above its ceiling, lnd below its target: hot -> lnd, up to
     lnd's target
  4. both above their ceilings, a cold address configured: the hot wallet's
     excess over its target to cold storage
  5. otherwise nothing

A move fills the receiving wallet to its target and leaves the sender at
or above its own, so afterwards both sit inside their bands, where no rule
fires: it cannot go back and forth. Brakes: a minimum move, a cooldown, a
daily count and share, a fee cap (urgent moves may pay up to the fee
ceiling), one move in flight at a time, and no hot wallet spend while a
reverse swap is about to lock up.

Coins only ever go to the service's own wallets (a fresh address each time,
checked as the wallet's own), or to the one cold address in .env, checked
against its fingerprint.

Configuration, from deploy/.env (sat unless said otherwise):

  REBALANCE_MODE            off, dry-run (say what it would do) or on
  REBALANCE_HOT_FLOOR       default 4000000 (two maximum swaps)
  REBALANCE_HOT_TARGET      default 10000000; never above lnd's inbound
                            capacity, which is all reverse swaps can use
  REBALANCE_HOT_CEILING     default 25000000
  REBALANCE_LND_FLOOR       above lnd's anchor reserve, default 200000
  REBALANCE_LND_POT         lnd's target above its reserve: coins ready for
                            the next channel, default 2000000
  REBALANCE_LND_HEADROOM    lnd's ceiling above its target, default 5000000
  REBALANCE_MIN_MOVE        default 1000000
  REBALANCE_COOLDOWN_HOURS  after a move, default 12
  REBALANCE_MAX_MOVES_DAY   default 2
  REBALANCE_MAX_SHARE_DAY   of both wallets together, default 0.5
  REBALANCE_FEE_CAP         sat/vB for an ordinary move, default 10
  REBALANCE_FEE_CEILING     sat/vB for an urgent one, default 100
  REBALANCE_FEE_FALLBACK    sat/vB when Knots has no estimate, default 2
  REBALANCE_COLD_ADDRESS    empty: no cold storage (rule 4 off)
  REBALANCE_COLD_ADDRESS_SHA256
                            the first 16 hex digits of the address's
                            SHA-256, kept by the operator: a changed
                            address is refused
  MAX_SWAP_SAT              the largest swap: urgency for the hot wallet

  lfswap_rebalance.py           one run, in REBALANCE_MODE
  lfswap_rebalance.py --dry-run one run that sends nothing, whatever the mode
  lfswap_rebalance.py --status  balances, bands and what a run would do

State (the move in flight, past moves, the last dry-run message) is kept
in $LFSWAP_ROOT/rebalance/state.json.
"""

import datetime
import fcntl
import hashlib
import json
import os
import sys
from dataclasses import dataclass

HERE = os.path.dirname(os.path.abspath(__file__))
DEPLOY = os.path.dirname(HERE)
sys.path.insert(0, os.path.join(DEPLOY, "monitor"))
import lfswap_monitor  # noqa: E402

HOT, LND, COLD = "hot", "lnd", "cold"
NAMES = {HOT: "the hot wallet", LND: "lnd", COLD: "cold storage"}
LABEL = "lfswap-rebalance"
# Reverse swaps whose lockup is still to be sent from the hot wallet
PRE_LOCKUP = ("swap.created", "minerfee.paid")
# A move unconfirmed after this many blocks is bumped
BUMP_AFTER_BLOCKS = 6
KEEP_MOVES_DAYS = 400


def num(env, key, default):
    return lfswap_monitor.number(env, key, default)


@dataclass(frozen=True)
class Band:
    floor: int
    target: int
    ceiling: int


@dataclass(frozen=True)
class Move:
    source: str
    dest: str
    amount: int
    rule: int
    reason: str
    urgent: bool = False

    def describe(self):
        return (f"{self.amount:,} sat from {NAMES[self.source]} to "
                f"{NAMES[self.dest]}: {self.reason}")


@dataclass(frozen=True)
class Config:
    hot_floor: int = 4_000_000
    hot_target: int = 10_000_000
    hot_ceiling: int = 25_000_000
    lnd_floor: int = 200_000
    lnd_pot: int = 2_000_000
    lnd_headroom: int = 5_000_000
    min_move: int = 1_000_000
    cooldown_hours: float = 12
    max_moves_day: int = 2
    max_share_day: float = 0.5
    fee_cap: float = 10
    fee_ceiling: float = 100
    fee_fallback: float = 2
    max_swap: int = 0
    cold_address: str = ""

    @classmethod
    def from_env(cls, env):
        d = cls()
        return cls(
            hot_floor=int(num(env, "REBALANCE_HOT_FLOOR", d.hot_floor)),
            hot_target=int(num(env, "REBALANCE_HOT_TARGET", d.hot_target)),
            hot_ceiling=int(num(env, "REBALANCE_HOT_CEILING", d.hot_ceiling)),
            lnd_floor=int(num(env, "REBALANCE_LND_FLOOR", d.lnd_floor)),
            lnd_pot=int(num(env, "REBALANCE_LND_POT", d.lnd_pot)),
            lnd_headroom=int(num(env, "REBALANCE_LND_HEADROOM",
                                 d.lnd_headroom)),
            min_move=int(num(env, "REBALANCE_MIN_MOVE", d.min_move)),
            cooldown_hours=num(env, "REBALANCE_COOLDOWN_HOURS",
                               d.cooldown_hours),
            max_moves_day=int(num(env, "REBALANCE_MAX_MOVES_DAY",
                                  d.max_moves_day)),
            max_share_day=num(env, "REBALANCE_MAX_SHARE_DAY",
                              d.max_share_day),
            fee_cap=num(env, "REBALANCE_FEE_CAP", d.fee_cap),
            fee_ceiling=num(env, "REBALANCE_FEE_CEILING", d.fee_ceiling),
            fee_fallback=num(env, "REBALANCE_FEE_FALLBACK", d.fee_fallback),
            max_swap=int(num(env, "MAX_SWAP_SAT", 0)),
            cold_address=env.get("REBALANCE_COLD_ADDRESS", ""),
        )

    def problems(self):
        out = []
        if not self.hot_floor < self.hot_target < self.hot_ceiling:
            out.append("the hot wallet's floor, target and ceiling must rise")
        if not 0 <= self.lnd_floor < self.lnd_pot or self.lnd_headroom <= 0:
            out.append("lnd's floor must be below its pot, and its headroom "
                       "above zero")
        if self.min_move <= 0 or self.fee_cap > self.fee_ceiling:
            out.append("the minimum move must be positive and the fee cap "
                       "at most the fee ceiling")
        return out


# --- the decision ----------------------------------------------------------

def bands(cfg, inbound):
    """Each wallet's band for this run. The hot wallet's target follows
    inbound capacity (reverse swaps cannot use more than lnd can receive),
    but stays within its floor and ceiling. lnd's band is of what it holds
    above its anchor reserve."""
    hot_target = max(cfg.hot_floor, min(cfg.hot_target, inbound,
                                        cfg.hot_ceiling))
    return (Band(cfg.hot_floor, hot_target, cfg.hot_ceiling),
            Band(cfg.lnd_floor, cfg.lnd_pot, cfg.lnd_pot + cfg.lnd_headroom))


def decide(hot, lnd, hot_band, lnd_band, cold=False):
    """The one move the rules call for, or None. hot and lnd are spendable
    confirmed balances (sat)."""
    if hot < hot_band.floor and lnd > lnd_band.target:
        amount = min(hot_band.target - hot, lnd - lnd_band.target)
        return Move(LND, HOT, amount, 1,
                    f"the hot wallet ({hot:,}) is below its floor "
                    f"({hot_band.floor:,})")
    if lnd < lnd_band.floor and hot > hot_band.target:
        amount = min(lnd_band.target - lnd, hot - hot_band.target)
        return Move(HOT, LND, amount, 2,
                    f"lnd ({lnd:,}) is below its floor ({lnd_band.floor:,})")
    if hot > hot_band.ceiling and lnd < lnd_band.target:
        amount = min(lnd_band.target - lnd, hot - hot_band.target)
        return Move(HOT, LND, amount, 3,
                    f"the hot wallet ({hot:,}) is above its ceiling "
                    f"({hot_band.ceiling:,}) and lnd ({lnd:,}) below its "
                    f"target ({lnd_band.target:,})")
    if cold and hot > hot_band.ceiling and lnd > lnd_band.ceiling:
        return Move(HOT, COLD, hot - hot_band.target, 4,
                    "both wallets are above their ceilings")
    return None


def is_urgent(move, hot, lnd_confirmed, reserve, cfg):
    """Urgent moves may pay more than the fee cap: the hot wallet cannot
    take one maximum swap, or lnd cannot cover its anchor reserve."""
    if move.dest == HOT:
        return hot < max(cfg.max_swap, 1)
    if move.dest == LND:
        return lnd_confirmed < reserve
    return False


def within_allowance(move, state, now, combined, cfg):
    """The move, cut to what may still move today (a share of both
    wallets): a large need is met over several days rather than refused."""
    t = now.timestamp()
    moved = sum(m["amount"] for m in state.get("moves", [])
                if t - m["ts"] < 86400)
    allowance = int(cfg.max_share_day * combined) - moved
    if move.amount <= allowance:
        return move
    return Move(move.source, move.dest, max(0, allowance), move.rule,
                move.reason + f" (cut to today's allowance of "
                f"{cfg.max_share_day:.0%} of both wallets)", move.urgent)


def brakes(move, state, now, fee, combined, cfg):
    """Why the move must wait, or None."""
    if move.amount < cfg.min_move:
        return (f"{move.amount:,} sat is less than the minimum move "
                f"({cfg.min_move:,})")
    t = now.timestamp()
    moves = state.get("moves", [])
    if moves and t - moves[-1]["ts"] < cfg.cooldown_hours * 3600:
        left = cfg.cooldown_hours * 3600 - (t - moves[-1]["ts"])
        return f"cooling down after the last move ({left / 3600:.1f} h left)"
    today = [m for m in moves if t - m["ts"] < 86400]
    if len(today) >= cfg.max_moves_day:
        return f"{len(today)} moves in the last day already"
    moved = sum(m["amount"] for m in today)
    if combined > 0 and moved + move.amount > cfg.max_share_day * combined:
        return (f"it would move more than {cfg.max_share_day:.0%} of both "
                "wallets in a day")
    limit = cfg.fee_ceiling if move.urgent else cfg.fee_cap
    if fee > limit:
        return (f"fees are {fee:g} sat/vB, above the "
                f"{'ceiling' if move.urgent else 'cap'} of {limit:g}")
    return None


# --- the wallets -------------------------------------------------------------

def sat(btc):
    return round(float(btc) * 100_000_000)


def btc(sat_amount):
    return f"{sat_amount / 100_000_000:.8f}"


class Wallets:
    """What a run reads from and does with the wallets, through the
    monitor's runner (docker compose exec)."""

    def __init__(self, monitor):
        self.m = monitor

    def knots(self, *args):
        return self.m.knots("-rpcwallet=boltz", *args)

    def height(self):
        return int(json.loads(self.m.knots("getblockchaininfo"))["blocks"])

    def hot_coins(self):
        """Confirmed, spendable coins of the hot wallet."""
        return [c for c in json.loads(self.knots("listunspent", "1"))
                if c.get("spendable", True) and c.get("safe", True)]

    def committed(self):
        """What reverse swaps about to lock up will take from the hot
        wallet, and whether one was created in the last 15 minutes."""
        states = ", ".join(f"'{s}'" for s in PRE_LOCKUP)
        rows = self.m.psql(
            'SELECT coalesce(sum("onchainAmount"), 0), '
            "coalesce(sum(CASE WHEN \"createdAt\" > now() - interval "
            "'15 minutes' THEN 1 ELSE 0 END), 0) "
            f'FROM "reverseSwaps" WHERE status IN ({states})')
        return int(rows[0][0]), int(rows[0][1]) > 0

    def lnd_balance(self):
        """lnd's confirmed balance and its anchor reserve. Leased coins
        (channel donations) are not in the confirmed balance."""
        w = json.loads(self.m.lncli("walletbalance"))
        return (int(w.get("confirmed_balance", 0)),
                int(w.get("reserved_balance_anchor_chan", 0)))

    def inbound(self):
        b = json.loads(self.m.lncli("channelbalance"))
        return int(b.get("remote_balance", {}).get("sat", 0))

    def fee_rate(self, cfg):
        """sat/vB for confirmation within a few blocks."""
        try:
            est = json.loads(self.m.knots("estimatesmartfee", "3"))
            if "feerate" in est:
                return max(1.0, round(est["feerate"] * 100_000, 1))
        except Exception:  # noqa: BLE001 (no estimate: the fallback)
            pass
        return cfg.fee_fallback

    def hot_address(self):
        """A fresh hot wallet address, checked as the wallet's own."""
        address = self.knots("getnewaddress", LABEL, "bech32m").strip()
        info = json.loads(self.knots("getaddressinfo", address))
        if not (info.get("ismine") and info.get("solvable")):
            raise RuntimeError(f"{address} is not the hot wallet's own")
        return address

    def lnd_address(self):
        """A fresh lnd address, checked as lnd's own."""
        address = json.loads(self.m.lncli("newaddress", "p2tr"))["address"]
        listed = json.loads(self.m.lncli("wallet", "addresses", "list"))
        own = {a.get("address") for account in listed.get(
            "account_with_addresses", []) for a in account.get(
            "addresses", [])}
        if address not in own:
            raise RuntimeError(f"{address} is not among lnd's addresses")
        return address

    def send_from_lnd(self, address, amount, fee):
        out = json.loads(self.m.lncli(
            "sendcoins", "--addr", address, "--amt", str(amount),
            "--sat_per_vbyte", str(max(1, int(fee + 0.999))),
            "--min_confs", "1", "--label", LABEL, "--force"))
        return out["txid"]

    def send_from_hot(self, address, amount, fee, coins):
        """Spends the largest coins first, so the rest of the hot wallet
        stays confirmed for the backend's lockups."""
        chosen, total = [], 0
        for coin in sorted(coins, key=lambda c: -sat(c["amount"])):
            chosen.append({"txid": coin["txid"], "vout": coin["vout"]})
            total += sat(coin["amount"])
            vsize = 11 + 58 * len(chosen) + 2 * 43
            if total >= amount + fee * vsize:
                break
        else:
            raise RuntimeError(f"the hot wallet's confirmed coins "
                               f"({total:,}) cannot pay {amount:,}")
        out = json.loads(self.knots(
            "-named", "send",
            "outputs=" + json.dumps({address: btc(amount)}),
            f"fee_rate={fee:g}",
            "options=" + json.dumps({"inputs": chosen, "add_inputs": False,
                                     "replaceable": True})))
        if not out.get("complete"):
            raise RuntimeError(f"the hot wallet did not send: {out}")
        return out["txid"]

    def confirmations(self, txid):
        """Confirmations of a move, from the hot wallet's view (every move
        involves it); negative when it conflicts with another."""
        tx = json.loads(self.knots("gettransaction", txid))
        return int(tx.get("confirmations", 0)), tx

    def fee_paid(self, move, tx):
        if move["source"] == HOT:
            return sat(abs(float(tx.get("fee", 0))))
        listed = json.loads(self.m.lncli(
            "listchaintxns", "--start_height", str(move["height"] - 1)))
        for t in listed.get("transactions", []):
            if t.get("tx_hash") == move["txid"]:
                return int(t.get("total_fees", 0))
        return 0

    def bump(self, move, fee):
        if move["source"] == HOT:
            out = json.loads(self.knots(
                "bumpfee", move["txid"], json.dumps({"fee_rate": fee})))
            return out["txid"]
        # lnd: a child spending its change pays for both
        listed = json.loads(self.m.lncli(
            "listchaintxns", "--start_height", str(move["height"] - 1)))
        for t in listed.get("transactions", []):
            if t.get("tx_hash") != move["txid"]:
                continue
            for out in t.get("output_details", []):
                if out.get("is_our_address"):
                    # --immediate: now, not at the sweeper's next block
                    self.m.lncli(
                        "wallet", "bumpfee", "--sat_per_vbyte",
                        str(int(fee + 0.999)), "--immediate",
                        f"{move['txid']}:{out['output_index']}")
                    return move["txid"]
        raise RuntimeError("lnd has no output of this move to bump it with")


# --- a run -------------------------------------------------------------------

class Run:
    def __init__(self, wallets, cfg, state, now, mode):
        self.w = wallets
        self.cfg = cfg
        self.state = state
        self.now = now
        self.mode = mode
        self.messages = []  # (title, text) to send
        self.lines = []     # for the log

    def say(self, text, alert=True, title="Rebalancing"):
        self.lines.append(text)
        if alert:
            self.messages.append((f"Lightning Fork Swap: {title}", text))

    def look(self):
        """Balances, bands and the move the rules call for."""
        coins = self.w.hot_coins()
        committed, recent = self.w.committed()
        hot = sum(sat(c["amount"]) for c in coins) - committed
        lnd_confirmed, reserve = self.w.lnd_balance()
        lnd = lnd_confirmed - reserve
        inbound = self.w.inbound()
        hot_band, lnd_band = bands(self.cfg, inbound)
        move = decide(hot, lnd, hot_band, lnd_band,
                      cold=bool(self.cfg.cold_address))
        if move is not None:
            move = Move(move.source, move.dest, move.amount, move.rule,
                        move.reason,
                        urgent=is_urgent(move, hot, lnd_confirmed, reserve,
                                         self.cfg))
        return {"coins": coins, "committed": committed, "recent": recent,
                "hot": hot, "lnd": lnd, "lnd_confirmed": lnd_confirmed,
                "reserve": reserve, "inbound": inbound, "hot_band": hot_band,
                "lnd_band": lnd_band, "move": move}

    def in_flight(self):
        """Follows the move in flight; True while it waits."""
        flight = self.state.get("inflight")
        if not flight:
            return False
        confs, tx = self.w.confirmations(flight["txid"])
        if confs > 0:
            fee = self.w.fee_paid(flight, tx)
            self.state.setdefault("moves", []).append(
                {**flight, "fee": fee, "confirmed": self.now.timestamp()})
            self.state.pop("inflight")
            self.say(f"confirmed: {flight['amount']:,} sat from "
                     f"{NAMES[flight['source']]} to {NAMES[flight['dest']]} "
                     f"({flight['txid']}), fee {fee:,} sat", alert=False)
            return False
        if confs < 0:
            self.say(f"the move {flight['txid']} conflicts with another "
                     "transaction and will not confirm: look at both "
                     "wallets before anything else moves")
            self.state["blocked"] = flight["txid"]
            self.state.pop("inflight")
            return True
        waited = self.w.height() - flight["height"]
        if waited >= BUMP_AFTER_BLOCKS and self.mode == "on":
            fee = min(self.cfg.fee_ceiling,
                      max(self.w.fee_rate(self.cfg),
                          flight["fee_rate"] * 1.5))
            if fee <= flight["fee_rate"]:
                self.say(f"the move {flight['txid']} is unconfirmed after "
                         f"{waited} blocks at the fee ceiling: look at it")
            else:
                try:
                    flight["txid"] = self.w.bump(flight, fee)
                    flight["fee_rate"] = fee
                    flight["height"] = self.w.height()
                    self.say(f"bumped the move to {fee:g} sat/vB after "
                             f"{waited} blocks: {flight['txid']}")
                except Exception as error:  # noqa: BLE001
                    self.say(f"could not bump the move {flight['txid']}: "
                             f"{str(error)[:300]}")
        return True

    def run(self):
        if self.state.get("blocked"):
            self.say(f"stopped: the move {self.state['blocked']} conflicted; "
                     "remove \"blocked\" from the state file once checked",
                     alert=False)
            return
        if self.in_flight():
            return
        seen = self.look()
        move = seen["move"]
        status = (f"hot {seen['hot']:,} (band {seen['hot_band'].floor:,}/"
                  f"{seen['hot_band'].target:,}/{seen['hot_band'].ceiling:,}"
                  f"), lnd {seen['lnd']:,} above its reserve of "
                  f"{seen['reserve']:,} (band {seen['lnd_band'].floor:,}/"
                  f"{seen['lnd_band'].target:,}/{seen['lnd_band'].ceiling:,})")
        self.lines.append(status)
        if move is None:
            self.state.pop("dry", None)
            self._last("nothing to move")
            return
        fee = self.w.fee_rate(self.cfg)
        combined = seen["hot"] + seen["lnd"]
        move = within_allowance(move, self.state, self.now, combined,
                                self.cfg)
        why_not = brakes(move, self.state, self.now, fee, combined, self.cfg)
        if why_not is None and move.source == HOT and seen["recent"]:
            why_not = "a reverse swap is about to lock up from the hot wallet"
        if why_not:
            text = f"would move {move.describe()}; waiting: {why_not}"
            self._last(text)
            # Said once, then again only if it is urgent and new
            self.say(text, alert=move.urgent and self._new(text))
            return
        if self.mode != "on":
            text = (f"dry run: would move {move.describe()} at "
                    f"{fee:g} sat/vB. {status}")
            self._last(f"would move {move.describe()}")
            self.say(text, alert=self._new(f"{move.source}>{move.dest}:"
                                           f"{move.rule}"))
            return
        self.send(move, fee, seen)

    def _last(self, text):
        """What the last run decided, for the weekly report."""
        self.state["last"] = {"ts": self.now.timestamp(), "text": text}

    def _new(self, key):
        """True once a day for the same message."""
        dry = self.state.get("dry", {})
        t = self.now.timestamp()
        if dry.get("key") == key and t - dry.get("ts", 0) < 86400:
            return False
        self.state["dry"] = {"key": key, "ts": t}
        return True

    def send(self, move, fee, seen):
        try:
            if move.dest == HOT:
                txid = self.w.send_from_lnd(self.w.hot_address(), move.amount,
                                            fee)
            elif move.dest == LND:
                txid = self.w.send_from_hot(self.w.lnd_address(), move.amount,
                                            fee, seen["coins"])
            else:
                txid = self.w.send_from_hot(self.cfg.cold_address,
                                            move.amount, fee, seen["coins"])
        except Exception as error:  # noqa: BLE001
            self.say(f"could not move {move.describe()}: {str(error)[:300]}")
            return
        self.state["inflight"] = {
            "ts": self.now.timestamp(), "source": move.source,
            "dest": move.dest, "amount": move.amount, "rule": move.rule,
            "txid": txid, "fee_rate": fee, "height": self.w.height()}
        self.state.pop("dry", None)
        self._last(f"moved {move.describe()}")
        self.say(f"moved {move.describe()}, at {fee:g} sat/vB: {txid}")


# --- the operator's view -----------------------------------------------------

def report_lines(state, days, now, telemetry=None, env=None):
    """For the weekly report: moves, their fees, and how long each wallet
    spent below its floor (from the telemetry snapshots)."""
    t = now.timestamp()
    moves = [m for m in state.get("moves", []) if t - m["ts"] < days * 86400]
    lines = ["Rebalancing"]
    if moves:
        total = sum(m["amount"] for m in moves)
        fees = sum(m.get("fee", 0) for m in moves)
        lines.append(f"  {len(moves)} move{'s' if len(moves) != 1 else ''}"
                     f", {total:,} sat, fees {fees:,} sat")
        for m in moves[-5:]:
            stamp = datetime.datetime.fromtimestamp(
                m["ts"], datetime.timezone.utc).strftime("%Y-%m-%d")
            lines.append(f"    {stamp} {m['amount']:,} sat {m['source']} -> "
                         f"{m['dest']} (rule {m['rule']})")
    else:
        lines.append("  no moves")
    if state.get("last"):
        stamp = datetime.datetime.fromtimestamp(
            state["last"]["ts"], datetime.timezone.utc).strftime(
            "%Y-%m-%d %H:%M")
        lines.append(f"  last run ({stamp} UTC): {state['last']['text']}")
    if state.get("inflight"):
        f = state["inflight"]
        lines.append(f"  in flight: {f['amount']:,} sat {f['source']} -> "
                     f"{f['dest']} ({f['txid']})")
    if telemetry is not None and env is not None:
        cfg = Config.from_env(env)
        rows = telemetry.db.execute(
            "SELECT hot_confirmed, lnd_confirmed, lnd_reserve FROM snapshot "
            "WHERE ts >= ? AND hot_confirmed IS NOT NULL",
            (int(t - days * 86400),)).fetchall()
        if rows:
            hot_low = sum(1 for h, _, _ in rows if h < cfg.hot_floor)
            lnd_low = sum(1 for _, ln, r in rows
                          if ln is not None and ln - (r or 0) < cfg.lnd_floor)
            lines.append(f"  below the floor: hot wallet "
                         f"{hot_low / len(rows):.0%} of the time, lnd "
                         f"{lnd_low / len(rows):.0%}")
    return lines


def load_state(path):
    try:
        with open(path) as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


def save_state(path, state):
    t = datetime.datetime.now(datetime.timezone.utc).timestamp()
    state["moves"] = [m for m in state.get("moves", [])
                      if t - m["ts"] < KEEP_MOVES_DAYS * 86400]
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(state, f, indent=1)
    os.chmod(tmp, 0o600)
    os.replace(tmp, path)


def cold_problem(env):
    address = env.get("REBALANCE_COLD_ADDRESS", "")
    if not address:
        return None
    want = env.get("REBALANCE_COLD_ADDRESS_SHA256", "").lower()
    have = hashlib.sha256(address.encode()).hexdigest()[:16]
    if want != have:
        return ("REBALANCE_COLD_ADDRESS does not match "
                "REBALANCE_COLD_ADDRESS_SHA256: cold storage is off")
    return None


def maintenance_on(path="/etc/nginx/lfswap-maintenance.conf"):
    try:
        with open(path) as f:
            return f.read().strip() != ""
    except OSError:
        return False


def main(argv, runner=None, now=None):
    env = lfswap_monitor.load_env(os.path.join(DEPLOY, ".env"))
    root = env.get("LFSWAP_ROOT", "/srv/lfswap")
    network = env.get("NETWORK", "mainnet") or "mainnet"
    now = now or datetime.datetime.now(datetime.timezone.utc)
    stamp = now.strftime("%Y-%m-%dT%H:%M:%SZ")
    mode = env.get("REBALANCE_MODE", "off") or "off"
    if "--dry-run" in argv or "--status" in argv:
        mode = "dry-run"
    if mode not in ("off", "dry-run", "on"):
        print(f"{stamp} REBALANCE_MODE={mode}: off, dry-run or on")
        return 1
    if mode == "off":
        return 0
    if os.path.exists(os.path.join(root, "MIGRATED")):
        print(f"{stamp} this host was migrated away; nothing to move")
        return 0
    # Maintenance pauses moves; --status still tells
    if maintenance_on() and "--status" not in argv:
        print(f"{stamp} maintenance mode: nothing moves")
        return 0

    cfg = Config.from_env(env)
    problems = cfg.problems()
    cold = cold_problem(env)
    if cold:
        # The rest still runs; only cold storage is off
        print(f"{stamp} {cold}")
        lfswap_monitor.send(env, "Lightning Fork Swap: rebalancing", cold)
        cfg = Config(**{**cfg.__dict__, "cold_address": ""})
    if problems:
        text = "; ".join(problems)
        print(f"{stamp} not running: {text}")
        lfswap_monitor.send(env, "Lightning Fork Swap: rebalancing is off",
                            text)
        return 1

    state_dir = os.path.join(root, "rebalance")
    os.makedirs(state_dir, mode=0o700, exist_ok=True)
    with open(os.path.join(state_dir, "lock"), "w") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            print(f"{stamp} another run is in progress")
            return 0
        state_path = os.path.join(state_dir, "state.json")
        state = load_state(state_path)
        monitor = lfswap_monitor.Monitor(
            runner or lfswap_monitor.Runner(DEPLOY), env, now, network)
        run = Run(Wallets(monitor), cfg, state, now, mode)
        if "--status" in argv:
            seen = run.look()
            print(f"hot wallet: {seen['hot']:,} spendable "
                  f"({seen['committed']:,} committed to reverse swaps), "
                  f"band {seen['hot_band']}")
            print(f"lnd: {seen['lnd']:,} above its reserve of "
                  f"{seen['reserve']:,}, band {seen['lnd_band']}")
            print(f"inbound: {seen['inbound']:,}")
            print("would move " + seen["move"].describe()
                  if seen["move"] else "nothing to move")
            return 0
        try:
            run.run()
        except Exception as error:  # noqa: BLE001
            run.say(f"the run failed: {str(error)[:300]}")
        for line in run.lines:
            print(f"{stamp} {line}")
        if "--dry-run" not in argv:
            save_state(state_path, state)
            for title, text in run.messages:
                try:
                    lfswap_monitor.send(env, title, text)
                except Exception as error:  # noqa: BLE001
                    print(f"{stamp} could not send: {error}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
