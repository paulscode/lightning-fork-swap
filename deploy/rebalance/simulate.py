#!/usr/bin/env python3
"""Runs the rebalancer's rules (lfswap_rebalance.py) against simulated swap
demand, to choose its bands with evidence: how often it moves, what that
costs in fees, and how often each wallet is below its floor or a swap is
refused, for given numbers. Nothing touches a wallet.

  simulate.py [--days 90] [--runs 20] [--seed 1]
              [--hot 30000000] [--lnd 1000000] [--outbound 80000000]
              [--inbound 11000000]
              [--swaps-per-day 6] [--reverse-share 0.5] [--mean-swap 300000]
              [--telemetry /srv/lfswap/telemetry/telemetry.db]
              [--set REBALANCE_HOT_TARGET=8000000 ...] [--env deploy/.env]

Demand is random (sizes around --mean-swap, up to MAX_SWAP_SAT), or, with
--telemetry, the swaps the service actually saw, replayed in a loop. The
bands come from .env (--env) with --set on top. Fees follow a quiet chain
with occasional spikes. Each run starts from the given balances; the
summary gives the median and the worst of --runs runs.

Simplified on purpose: a reverse swap takes its amount from the hot wallet
and from inbound (adding it to outbound); a submarine swap the other way
round. Channel opens are not simulated.
"""

import argparse
import dataclasses
import os
import random
import sqlite3
import statistics
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import lfswap_rebalance as r  # noqa: E402

TICK = 1800          # one run every 30 minutes
MOVE_VBYTES = 150    # a transfer: one or two inputs, two outputs
LOCKUP_VBYTES = 150  # a reverse swap's lockup, paid from the hot wallet


def fee_at(rng):
    """sat/vB: mostly quiet, sometimes busy."""
    roll = rng.random()
    if roll < 0.85:
        return rng.choice([1, 1, 2, 2, 3, 4, 5])
    if roll < 0.97:
        return rng.randint(6, 20)
    return rng.randint(21, 80)


def random_demand(rng, days, per_day, reverse_share, mean, max_swap):
    """(tick, kind, amount) for each swap."""
    out = []
    ticks = days * 86400 // TICK
    rate = per_day * TICK / 86400
    for tick in range(ticks):
        n = 0
        # Poisson by thinning, small rates
        while rng.random() < rate / (n + 1):
            n += 1
        for _ in range(n):
            amount = int(min(max_swap, max(25_000, rng.expovariate(1 / mean))))
            kind = "reverse" if rng.random() < reverse_share else "submarine"
            out.append((tick, kind, amount))
    return out


def recorded_demand(path, days):
    """The swaps telemetry recorded, replayed from tick 0 and repeated to
    fill the period."""
    db = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
    rows = db.execute(
        "SELECT created, type, coalesce(onchain_amount, invoice_amount) "
        "FROM swap WHERE created IS NOT NULL ORDER BY created").fetchall()
    db.close()
    if not rows:
        raise SystemExit("no swaps recorded")
    start = rows[0][0]
    span = max(rows[-1][0] - start, 86400)
    out = []
    loop = 0
    while loop * span < days * 86400:
        for created, kind, amount in rows:
            t = created - start + loop * span
            if t < days * 86400 and amount:
                out.append((t // TICK, "reverse" if "reverse" in kind
                            else "submarine", int(amount)))
        loop += 1
    return out


@dataclasses.dataclass
class Outcome:
    moves: int = 0
    moved: int = 0
    fees: int = 0
    refused_reverse: int = 0
    refused_submarine: int = 0
    hot_low: float = 0.0
    lnd_low: float = 0.0
    end_hot: int = 0
    end_lnd: int = 0


def simulate(cfg, demand, days, start, seed):
    rng = random.Random(seed)
    hot, lnd, outbound, inbound = start
    by_tick = {}
    for tick, kind, amount in demand:
        by_tick.setdefault(tick, []).append((kind, amount))
    state = {"moves": []}
    inflight = None
    out = Outcome()
    ticks = days * 86400 // TICK
    hot_low = lnd_low = 0
    for tick in range(ticks):
        now = r.datetime.datetime.fromtimestamp(
            tick * TICK, r.datetime.timezone.utc)
        fee = fee_at(rng)
        for kind, amount in by_tick.get(tick, []):
            if kind == "reverse":
                need = amount + LOCKUP_VBYTES * fee
                if hot < need or inbound < amount:
                    out.refused_reverse += 1
                    continue
                hot -= need
                inbound -= amount
                outbound += amount
            else:
                if outbound < amount:
                    out.refused_submarine += 1
                    continue
                hot += amount
                outbound -= amount
                inbound += amount
        # The move sent last tick has confirmed
        if inflight is not None:
            dest, amount = inflight
            if dest == r.HOT:
                hot += amount
            elif dest == r.LND:
                lnd += amount
            inflight = None
        hot_band, lnd_band = r.bands(cfg, inbound)
        hot_low += hot < hot_band.floor
        lnd_low += lnd < lnd_band.floor
        move = r.decide(hot, lnd, hot_band, lnd_band,
                        cold=bool(cfg.cold_address))
        if move is None:
            continue
        move = dataclasses.replace(
            move, urgent=r.is_urgent(move, hot, lnd, 0, cfg))
        move = r.within_allowance(move, state, now, hot + lnd, cfg)
        if r.brakes(move, state, now, fee, hot + lnd, cfg):
            continue
        cost = MOVE_VBYTES * fee
        if move.source == r.HOT:
            hot -= move.amount + cost
        else:
            lnd -= move.amount + cost
        inflight = (move.dest, move.amount)
        state["moves"].append({"ts": now.timestamp(), "amount": move.amount})
        out.moves += 1
        out.moved += move.amount
        out.fees += cost
    out.hot_low = hot_low / ticks
    out.lnd_low = lnd_low / ticks
    out.end_hot, out.end_lnd = hot, lnd
    return out


def summary(outcomes):
    def line(name, values, fmt):
        values = sorted(values)
        return (f"  {name:<28} median {fmt(statistics.median(values))}, "
                f"worst {fmt(values[-1])}")
    sat = lambda v: f"{int(v):,} sat"  # noqa: E731
    pct = lambda v: f"{v:.1%}"  # noqa: E731
    count = lambda v: f"{v:g}"  # noqa: E731
    return [
        line("moves", [o.moves for o in outcomes], count),
        line("moved", [o.moved for o in outcomes], sat),
        line("fees of moves", [o.fees for o in outcomes], sat),
        line("reverse swaps refused", [o.refused_reverse for o in outcomes],
             count),
        line("submarine swaps refused",
             [o.refused_submarine for o in outcomes], count),
        line("hot wallet below its floor", [o.hot_low for o in outcomes],
             pct),
        line("lnd below its floor", [o.lnd_low for o in outcomes], pct),
    ]


def main(argv):
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("--days", type=int, default=90)
    p.add_argument("--runs", type=int, default=20)
    p.add_argument("--seed", type=int, default=1)
    p.add_argument("--hot", type=int, default=30_000_000)
    p.add_argument("--lnd", type=int, default=1_000_000)
    p.add_argument("--outbound", type=int, default=80_000_000)
    p.add_argument("--inbound", type=int, default=11_000_000)
    p.add_argument("--swaps-per-day", type=float, default=6)
    p.add_argument("--reverse-share", type=float, default=0.5)
    p.add_argument("--mean-swap", type=int, default=300_000)
    p.add_argument("--telemetry")
    p.add_argument("--env")
    p.add_argument("--set", action="append", default=[])
    args = p.parse_args(argv)

    env = r.lfswap_monitor.load_env(args.env) if args.env else {}
    for item in args.set:
        key, _, value = item.partition("=")
        env[key] = value
    cfg = r.Config.from_env(env)
    if cfg.problems():
        raise SystemExit("; ".join(cfg.problems()))
    max_swap = cfg.max_swap or 2_000_000
    start = (args.hot, args.lnd, args.outbound, args.inbound)

    outcomes = []
    for run in range(args.runs):
        rng = random.Random(args.seed + run)
        demand = (recorded_demand(args.telemetry, args.days) if args.telemetry
                  else random_demand(rng, args.days, args.swaps_per_day,
                                     args.reverse_share, args.mean_swap,
                                     max_swap))
        outcomes.append(simulate(cfg, demand, args.days, start,
                                 args.seed + run))

    print(f"{args.runs} runs of {args.days} days, from hot {args.hot:,}, "
          f"lnd {args.lnd:,}, outbound {args.outbound:,}, inbound "
          f"{args.inbound:,}")
    print(f"bands: hot {cfg.hot_floor:,}/{cfg.hot_target:,}/"
          f"{cfg.hot_ceiling:,} (target capped by inbound), lnd "
          f"{cfg.lnd_floor:,}/{cfg.lnd_pot:,}/"
          f"{cfg.lnd_pot + cfg.lnd_headroom:,} above its reserve; minimum "
          f"move {cfg.min_move:,}, cooldown {cfg.cooldown_hours:g} h, fee cap "
          f"{cfg.fee_cap:g} sat/vB")
    print("demand: " + (f"recorded, from {args.telemetry}" if args.telemetry
                        else f"{args.swaps_per_day:g} swaps a day, "
                        f"{args.reverse_share:.0%} reverse, mean "
                        f"{args.mean_swap:,} sat"))
    print("\n".join(summary(outcomes)))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
