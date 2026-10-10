#!/usr/bin/env python3
"""Runs the rebalancer against the regtest service (deploy/regtest), with
real wallets and real commands: each rule's move sent and confirmed, a stuck
move bumped from either wallet, and no hot wallet spend while a reverse swap
waits to lock up. Bands are set around the regtest balances for each step.

  cd deploy/regtest && docker compose up -d   (and bootstrap.sh once)
  python3 deploy/rebalance/regtest.py

Leaves a few transfers between the regtest wallets behind; nothing else.
"""

import datetime
import json
import os
import secrets
import subprocess
import sys
import tempfile
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
REGTEST = os.path.join(os.path.dirname(HERE), "regtest")
sys.path.insert(0, HERE)
import lfswap_rebalance as r  # noqa: E402

API = os.environ.get("REGTEST_API", "http://127.0.0.1:19001")
KNOTS = ["-regtest", "-rpcuser=lab", "-rpcpassword=lab"]


class RegtestRunner(r.lfswap_monitor.Runner):
    """The production commands, pointed at the regtest services."""

    def compose(self, *args, timeout=60):
        args = list(args)
        if args[:3] == ["exec", "-T", "knots"]:
            args = args[:4] + KNOTS + [a for a in args[4:]
                                       if a != "-datadir=/data"]
        elif args[:3] == ["exec", "-T", "lnd"]:
            args[2] = "lnd-swap"
        return super().compose(*args, timeout=timeout)


runner = RegtestRunner(REGTEST)


def knots(*args):
    return runner.compose("exec", "-T", "knots", "bitcoin-cli", *args)


def mine(n=1):
    address = knots("-rpcwallet=boltz", "getnewaddress").strip()
    knots("generatetoaddress", str(n), address)


def balances():
    w = r.Wallets(r.lfswap_monitor.Monitor(runner, {}, now(), "regtest"))
    coins = w.hot_coins()
    hot = sum(r.sat(c["amount"]) for c in coins) - w.committed()[0]
    confirmed, reserve = w.lnd_balance()
    return hot, confirmed - reserve, coins


def now():
    return datetime.datetime.now(datetime.timezone.utc)


def run(cfg, state):
    the_run = r.Run(r.Wallets(r.lfswap_monitor.Monitor(
        runner, {}, now(), "regtest")), cfg, state, now(), "on")
    the_run.run()
    for line in the_run.lines:
        print(f"    {line}")
    return the_run


def check(ok, what):
    if not ok:
        raise SystemExit(f"FAIL: {what}")
    print(f"  ok  {what}")


def base(**kw):
    """Brakes off that a test run cannot wait for."""
    return r.Config(cooldown_hours=0, max_moves_day=100, max_share_day=1,
                    min_move=100_000, **kw)


def settle(state, cfg):
    """Mines the move in flight and lets the next run record it."""
    mine(1)
    run(cfg, state)
    check("inflight" not in state and state["moves"][-1].get("fee", 0) > 0,
          f"the move confirmed and its fee was recorded "
          f"({state['moves'][-1].get('fee')} sat)")


def step_lnd_to_hot():
    print("rule 1: the hot wallet below its floor, lnd above its target")
    hot, lnd, _ = balances()
    cfg = base(hot_floor=hot + 3_000_000, hot_target=hot + 3_000_001,
               hot_ceiling=hot + 900_000_000, lnd_floor=0,
               lnd_pot=1_000_000, lnd_headroom=1)
    state = {}
    run(cfg, state)
    flight = state.get("inflight")
    check(flight and (flight["source"], flight["dest"]) == ("lnd", "hot"),
          "sent from lnd to the hot wallet")
    check(abs(flight["amount"] - 3_000_000) <= 1,
          f"enough to reach the hot wallet's target ({flight['amount']:,})")
    tx = json.loads(knots("-rpcwallet=boltz", "gettransaction",
                          flight["txid"]))
    received = sum(r.sat(d["amount"]) for d in tx["details"]
                   if d["category"] == "receive")
    check(received == flight["amount"], "the hot wallet's own address got it")
    settle(state, cfg)
    return state


def step_hot_to_lnd():
    print("rule 3: the hot wallet above its ceiling, lnd below its target")
    hot, lnd, coins = balances()
    cfg = base(hot_floor=1, hot_target=2, hot_ceiling=3, lnd_floor=0,
               lnd_pot=lnd + 2_000_000, lnd_headroom=1)
    state = {}
    run(cfg, state)
    flight = state.get("inflight")
    check(flight and (flight["source"], flight["dest"]) == ("hot", "lnd"),
          "sent from the hot wallet to lnd")
    check(flight["amount"] == 2_000_000, "up to lnd's target")
    raw = json.loads(knots("-rpcwallet=boltz", "gettransaction",
                           flight["txid"], "true", "true"))["decoded"]
    largest = max(coins, key=lambda c: r.sat(c["amount"]))
    check([(v["txid"], v["vout"]) for v in raw["vin"]] ==
          [(largest["txid"], largest["vout"])],
          "spent the largest coin, nothing else")
    check(all(v["sequence"] < 0xfffffffe for v in raw["vin"]),
          "replaceable")
    settle(state, cfg)


def empty_blocks(n):
    """Blocks without the mempool's transactions: a move that waits, as if
    its fee were too low."""
    address = knots("-rpcwallet=boltz", "getnewaddress").strip()
    for _ in range(n):
        knots("generateblock", address, "[]")


def step_bumps():
    for source in ("hot", "lnd"):
        print(f"a stuck move from {source} is bumped after 6 blocks")
        hot, lnd, _ = balances()
        if source == "lnd":
            cfg = base(hot_floor=hot + 1_000_000, hot_target=hot + 1_000_001,
                       hot_ceiling=hot + 900_000_000, lnd_floor=0,
                       lnd_pot=1_000_000, lnd_headroom=1)
        else:
            cfg = base(hot_floor=1, hot_target=2, hot_ceiling=3, lnd_floor=0,
                       lnd_pot=lnd + 1_000_000, lnd_headroom=1)
        state = {}
        run(cfg, state)
        first = state["inflight"]["txid"]
        empty_blocks(6)
        # A fuller mempool than the move paid for
        cfg = r.Config(**{**cfg.__dict__, "fee_fallback": 5})
        bumped = run(cfg, state)
        check(any("bumped" in text for _, text in bumped.messages),
              f"bumped ({state['inflight']['txid'][:16]}...)")
        if source == "hot":
            check(state["inflight"]["txid"] != first,
                  "the hot wallet replaced it (RBF)")
            mempool = json.loads(knots("getrawmempool"))
            check(first not in mempool and state["inflight"]["txid"] in
                  mempool, "the replacement is in the mempool, not the first")
        else:
            child = json.loads(knots("getmempooldescendants", first))
            check(len(child) == 1, "lnd spent its change to pay for it (CPFP)")
        settle(state, cfg)


def step_reverse_waiting():
    print("no hot wallet spend while a reverse swap waits to lock up")
    request = json.dumps({
        "from": "BTC", "to": "BTC", "invoiceAmount": 100_000,
        "preimageHash": secrets.token_hex(32),
        "claimPublicKey": "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d9"
                          "59f2815b16f81798"}).encode()
    swap = json.loads(urllib.request.urlopen(urllib.request.Request(
        f"{API}/v2/swap/reverse", data=request,
        headers={"Content-Type": "application/json"}), timeout=30).read())
    hot, lnd, _ = balances()
    check(hot >= 0, f"reverse swap {swap['id']} created, not paid")
    cfg = base(hot_floor=1, hot_target=2, hot_ceiling=3, lnd_floor=0,
               lnd_pot=lnd + 1_000_000, lnd_headroom=1)
    state = {}
    the_run = run(cfg, state)
    check("inflight" not in state and "about to lock up" in the_run.lines[-1],
          "waited for the reverse swap")


def step_main():
    print("main() with a .env: dry-run says it, sends nothing")
    hot, lnd, _ = balances()
    with tempfile.TemporaryDirectory() as root:
        with open(os.path.join(root, ".env"), "w") as f:
            f.write(f"LFSWAP_ROOT={root}\nNETWORK=regtest\n"
                    "REBALANCE_MODE=dry-run\n"
                    f"REBALANCE_HOT_FLOOR={hot + 2_000_000}\n"
                    f"REBALANCE_HOT_TARGET={hot + 2_000_001}\n"
                    f"REBALANCE_HOT_CEILING={hot + 900_000_000}\n")
        before = json.loads(knots("getrawmempool"))
        old = r.DEPLOY
        r.DEPLOY = root
        try:
            r.main([], runner=runner)
        finally:
            r.DEPLOY = old
        with open(os.path.join(root, "rebalance", "state.json")) as f:
            state = json.load(f)
        check(state["last"]["text"].startswith("would move"),
              "recorded what it would do")
        check(json.loads(knots("getrawmempool")) == before, "sent nothing")


def main():
    mine(1)
    step_lnd_to_hot()
    step_hot_to_lnd()
    step_bumps()
    step_main()
    step_reverse_waiting()
    print("regtest rebalancing: all steps passed")


if __name__ == "__main__":
    try:
        main()
    except subprocess.SubprocessError as error:
        raise SystemExit(f"FAIL: {error}")
