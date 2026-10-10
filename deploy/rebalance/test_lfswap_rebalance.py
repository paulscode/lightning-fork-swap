"""Tests for lfswap_rebalance.py: python3 -m unittest discover deploy/rebalance"""

import datetime
import json
import os
import random
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))
import lfswap_rebalance as r  # noqa: E402

NOW = datetime.datetime(2026, 10, 10, 12, 0, tzinfo=datetime.timezone.utc)
CFG = r.Config(max_swap=2_000_000)


class World:
    """Both wallets, as the commands of a run see them."""

    def __init__(self, hot=10_000_000, lnd=2_000_000, reserve=100_000,
                 inbound=50_000_000, committed=0, recent=False, fee=2.0):
        self.coins = []
        if hot:
            self.add_coin(hot)
        self.lnd = lnd + reserve
        self.reserve = reserve
        self.inbound = inbound
        self.committed = committed
        self.recent = recent
        self.fee = fee
        self.height = 976000
        self.txs = {}       # txid -> {"confirmations", "fee", "source"}
        self.sent = []      # (source, address, amount, fee_rate, inputs)
        self.bumped = []
        self.addresses = 0
        self.not_mine = False

    def add_coin(self, amount):
        self.coins.append({"txid": f"{len(self.coins):064x}", "vout": 0,
                           "amount": amount / 1e8, "spendable": True,
                           "safe": True})

    def compose(self, *args, timeout=60):
        service = args[2]
        if service == "postgres":
            return f"{self.committed}|{1 if self.recent else 0}\n"
        if service == "knots":
            rest = [a for a in args[5:] if a != "-rpcwallet=boltz"]
            return self.knots(rest)
        if service == "lnd":
            return self.lncli(list(args[5:]))
        raise AssertionError(args)

    def knots(self, a):
        cmd = a[0] if a[0] != "-named" else a[1]
        if cmd == "getblockchaininfo":
            return json.dumps({"blocks": self.height})
        if cmd == "listunspent":
            return json.dumps(self.coins)
        if cmd == "estimatesmartfee":
            if self.fee is None:
                return json.dumps({"errors": ["Insufficient data"]})
            return json.dumps({"feerate": self.fee / 100_000})
        if cmd == "getnewaddress":
            self.addresses += 1
            return f"bcrt1phot{self.addresses}\n"
        if cmd == "getaddressinfo":
            return json.dumps({"ismine": not self.not_mine, "solvable": True})
        if cmd == "send":
            named = dict(x.split("=", 1) for x in a[2:])
            outputs = json.loads(named["outputs"])
            options = json.loads(named["options"])
            (address, amount), = outputs.items()
            txid = f"{len(self.txs) + 100:064x}"
            self.txs[txid] = {"confirmations": 0, "fee": -0.00000300,
                              "source": "hot"}
            self.sent.append(("hot", address, round(float(amount) * 1e8),
                              float(named["fee_rate"]), options["inputs"]))
            return json.dumps({"txid": txid, "complete": True})
        if cmd == "gettransaction":
            return json.dumps(self.txs[a[1]])
        if cmd == "bumpfee":
            new = f"{len(self.txs) + 100:064x}"
            self.txs[new] = dict(self.txs[a[1]])
            self.bumped.append((a[1], json.loads(a[2])["fee_rate"]))
            return json.dumps({"txid": new})
        raise AssertionError(a)

    def lncli(self, a):
        if a[0] == "walletbalance":
            return json.dumps({"confirmed_balance": str(self.lnd),
                               "reserved_balance_anchor_chan":
                               str(self.reserve)})
        if a[0] == "channelbalance":
            return json.dumps({"remote_balance": {"sat": str(self.inbound)}})
        if a[0] == "newaddress":
            self.addresses += 1
            return json.dumps({"address": f"bcrt1plnd{self.addresses}"})
        if a[:3] == ["wallet", "addresses", "list"]:
            own = [] if self.not_mine else [
                {"address": f"bcrt1plnd{self.addresses}"}]
            return json.dumps({"account_with_addresses": [
                {"addresses": own}]})
        if a[0] == "sendcoins":
            opts = dict(zip(a[1::2], a[2::2]))
            txid = f"{len(self.txs) + 100:064x}"
            self.txs[txid] = {"confirmations": 0, "source": "lnd"}
            self.sent.append(("lnd", opts["--addr"], int(opts["--amt"]),
                              float(opts["--sat_per_vbyte"]), None))
            assert "--force" in a and opts["--min_confs"] == "1"
            return json.dumps({"txid": txid})
        if a[0] == "listchaintxns":
            return json.dumps({"transactions": [
                {"tx_hash": t, "total_fees": "250", "output_details": [
                    {"is_our_address": False, "output_index": 0},
                    {"is_our_address": True, "output_index": 1}]}
                for t in self.txs]})
        if a[:2] == ["wallet", "bumpfee"]:
            assert "--immediate" in a
            self.bumped.append((a[-1], float(a[3])))
            return "{}"
        raise AssertionError(a)

    def confirm(self, txid):
        """The move confirms: the coins arrive."""
        self.txs[txid]["confirmations"] = 1
        self.height += 1


def run(world, state=None, mode="on", now=NOW, cfg=CFG):
    monitor = r.lfswap_monitor.Monitor(world, {}, now, "regtest")
    state = {} if state is None else state
    the_run = r.Run(r.Wallets(monitor), cfg, state, now, mode)
    the_run.run()
    return the_run, state


class DecideTest(unittest.TestCase):
    hot = r.Band(4_000_000, 10_000_000, 25_000_000)
    lnd = r.Band(200_000, 2_000_000, 7_000_000)

    def test_each_rule_in_order(self):
        cases = [
            # (hot, lnd, cold, expected (source, dest, amount, rule) or None)
            (3_000_000, 9_000_000, False, ("lnd", "hot", 7_000_000, 1)),
            # rule 1 never takes lnd below its target
            (1_000_000, 2_500_000, False, ("lnd", "hot", 500_000, 1)),
            (12_000_000, 100_000, False, ("hot", "lnd", 1_900_000, 2)),
            # rule 2 never takes the hot wallet below its target
            (10_500_000, 100_000, False, ("hot", "lnd", 500_000, 2)),
            (30_000_000, 1_000_000, False, ("hot", "lnd", 1_000_000, 3)),
            (30_000_000, 8_000_000, True, ("hot", "cold", 20_000_000, 4)),
            (30_000_000, 8_000_000, False, None),
            (8_000_000, 1_000_000, False, None),
            # both low: nothing to take from
            (3_000_000, 100_000, False, None),
        ]
        for hot, lnd, cold, want in cases:
            with self.subTest(hot=hot, lnd=lnd, cold=cold):
                move = r.decide(hot, lnd, self.hot, self.lnd, cold)
                got = None if move is None else (
                    move.source, move.dest, move.amount, move.rule)
                self.assertEqual(got, want)

    def test_it_never_goes_back_and_forth(self):
        """Over long random demand, a move never undoes the one before
        without the balances having crossed a floor or ceiling since, and
        after every move both wallets are inside their bands."""
        rng = random.Random(7)
        for _ in range(200):
            hot = rng.randint(0, 40_000_000)
            lnd = rng.randint(0, 12_000_000)
            for _ in range(300):
                move = r.decide(hot, lnd, self.hot, self.lnd, cold=True)
                if move is not None:
                    if move.source == "hot":
                        hot -= move.amount
                    else:
                        lnd -= move.amount
                    if move.dest == "hot":
                        hot += move.amount
                    elif move.dest == "lnd":
                        lnd += move.amount
                    # Straight after a move, no rule fires
                    self.assertIsNone(
                        r.decide(hot, lnd, self.hot, self.lnd, cold=True),
                        (hot, lnd, move))
                    self.assertGreater(move.amount, 0)
                # Swaps drift the hot wallet; channel opens and closes lnd
                hot = max(0, hot + rng.randint(-1_500_000, 1_500_000))
                lnd = max(0, lnd + rng.choice([0] * 8 + [-1_000_000,
                                                          500_000]))

    def test_the_hot_target_follows_inbound(self):
        hot, lnd = r.bands(CFG, inbound=6_000_000)
        self.assertEqual(hot.target, 6_000_000)
        hot, _ = r.bands(CFG, inbound=1_000_000)
        self.assertEqual(hot.target, CFG.hot_floor)  # not below the floor
        hot, _ = r.bands(CFG, inbound=90_000_000)
        self.assertEqual(hot.target, CFG.hot_target)
        self.assertEqual(lnd, r.Band(200_000, 2_000_000, 7_000_000))

    def test_configuration_problems(self):
        self.assertEqual(CFG.problems(), [])
        bad = r.Config(hot_floor=5, hot_target=5, hot_ceiling=9)
        self.assertTrue(bad.problems())
        self.assertTrue(r.Config(fee_cap=200).problems())
        env = {"REBALANCE_HOT_TARGET": "12000000", "MAX_SWAP_SAT": "3000000",
               "REBALANCE_FEE_CAP": "5"}
        cfg = r.Config.from_env(env)
        self.assertEqual((cfg.hot_target, cfg.max_swap, cfg.fee_cap),
                         (12_000_000, 3_000_000, 5))


class BrakesTest(unittest.TestCase):
    move = r.Move("lnd", "hot", 2_000_000, 1, "x")

    def test_minimum_cooldown_count_share_and_fees(self):
        t = NOW.timestamp()
        self.assertIsNone(r.brakes(self.move, {}, NOW, 2, 50_000_000, CFG))
        small = r.Move("lnd", "hot", 999_999, 1, "x")
        self.assertIn("minimum", r.brakes(small, {}, NOW, 2, 5e7, CFG))
        recent = {"moves": [{"ts": t - 3600, "amount": 1}]}
        self.assertIn("cooling", r.brakes(self.move, recent, NOW, 2, 5e7, CFG))
        cfg = r.Config(cooldown_hours=0)
        two = {"moves": [{"ts": t - 7200, "amount": 1},
                         {"ts": t - 3600, "amount": 1}]}
        self.assertIn("moves in the last day",
                      r.brakes(self.move, two, NOW, 2, 5e7, cfg))
        big = {"moves": [{"ts": t - 7200, "amount": 2_000_000}]}
        self.assertIn("50%", r.brakes(self.move, big, NOW, 2, 7_000_000, cfg))
        self.assertIn("cap", r.brakes(self.move, {}, NOW, 11, 5e7, CFG))
        urgent = r.Move("lnd", "hot", 2_000_000, 1, "x", urgent=True)
        self.assertIsNone(r.brakes(urgent, {}, NOW, 50, 5e7, CFG))
        self.assertIn("ceiling", r.brakes(urgent, {}, NOW, 150, 5e7, CFG))

    def test_urgency(self):
        to_hot = r.Move("lnd", "hot", 1, 1, "x")
        self.assertTrue(r.is_urgent(to_hot, 1_999_999, 0, 0, CFG))
        self.assertFalse(r.is_urgent(to_hot, 2_000_000, 0, 0, CFG))
        to_lnd = r.Move("hot", "lnd", 1, 2, "x")
        self.assertTrue(r.is_urgent(to_lnd, 0, 50_000, 100_000, CFG))
        self.assertFalse(r.is_urgent(to_lnd, 0, 150_000, 100_000, CFG))


class RunTest(unittest.TestCase):
    def test_nothing_to_do(self):
        world = World()
        the_run, state = run(world)
        self.assertEqual(world.sent, [])
        self.assertEqual(the_run.messages, [])
        self.assertNotIn("inflight", state)

    def test_lnd_to_hot_wallet_end_to_end(self):
        world = World(hot=3_000_000, lnd=20_000_000)
        the_run, state = run(world)
        self.assertEqual(len(world.sent), 1)
        source, address, amount, fee, _ = world.sent[0]
        self.assertEqual((source, amount, fee), ("lnd", 7_000_000, 2.0))
        self.assertTrue(address.startswith("bcrt1phot"))
        flight = state["inflight"]
        self.assertEqual((flight["source"], flight["dest"], flight["rule"]),
                         ("lnd", "hot", 1))
        self.assertIn("moved 7,000,000 sat", the_run.messages[0][1])

        # Waits for it, then records it with its fee
        world.coins[0]["amount"] = 0.10  # what the move brought
        later = NOW + datetime.timedelta(minutes=30)
        run(world, state, now=later)
        self.assertEqual(len(world.sent), 1)
        world.confirm(flight["txid"])
        world.lnd = 2_100_000
        run(world, state, now=later + datetime.timedelta(minutes=30))
        self.assertNotIn("inflight", state)
        self.assertEqual(state["moves"][0]["fee"], 250)
        self.assertEqual(len(world.sent), 1)

    def test_a_large_need_is_met_over_days(self):
        # 7M needed, 12M in both wallets: 6M today, the rest after the
        # cooldown
        world = World(hot=3_000_000, lnd=9_000_000)
        the_run, state = run(world)
        self.assertEqual(world.sent[0][2], 6_000_000)
        self.assertIn("allowance", the_run.messages[0][1])
        world.confirm(state["inflight"]["txid"])
        world.coins[0]["amount"] = 0.09
        world.lnd = 3_100_000
        run(world, state, now=NOW + datetime.timedelta(hours=1))
        self.assertEqual(len(world.sent), 1)  # cooling down
        # A day later: the hot wallet is within its band; nothing more
        run(world, state, now=NOW + datetime.timedelta(hours=25))
        self.assertEqual(len(world.sent), 1)
        # Had it still been low, the allowance would count only today
        world.coins[0]["amount"] = 0.03
        world.lnd = 9_100_000
        run(world, state, now=NOW + datetime.timedelta(hours=49))
        self.assertEqual(world.sent[1][2], 6_000_000)

    def test_hot_to_lnd_spends_the_largest_coins(self):
        world = World(hot=0, lnd=50_000)
        for amount in (3_000_000, 20_000_000, 9_000_000):
            world.add_coin(amount)
        _, state = run(world)
        source, address, amount, fee, inputs = world.sent[0]
        self.assertEqual((source, amount), ("hot", 1_950_000))
        self.assertTrue(address.startswith("bcrt1plnd"))
        self.assertEqual(inputs, [{"txid": f"{1:064x}", "vout": 0}])

    def test_coins_committed_to_reverse_swaps_are_not_counted(self):
        # 26M, but 2M are about to lock up: not above the ceiling
        world = World(hot=26_000_000, lnd=1_000_000, committed=2_000_000)
        run(world)
        self.assertEqual(world.sent, [])

    def test_no_hot_spend_while_a_reverse_swap_is_about_to_lock_up(self):
        world = World(hot=30_000_000, lnd=0, recent=True)
        the_run, _ = run(world)
        self.assertEqual(world.sent, [])
        self.assertIn("about to lock up", the_run.lines[-1])

    def test_dry_run_says_it_once_a_day(self):
        world = World(hot=3_000_000, lnd=20_000_000)
        the_run, state = run(world, mode="dry-run")
        self.assertEqual(world.sent, [])
        report = "\n".join(r.report_lines(state, 7, NOW))
        self.assertIn("last run (2026-10-10 12:00 UTC): would move 7,000,000",
                      report)
        self.assertIn("dry run: would move 7,000,000 sat",
                      the_run.messages[0][1])
        again, _ = run(world, state, mode="dry-run",
                       now=NOW + datetime.timedelta(hours=1))
        self.assertEqual(again.messages, [])
        tomorrow, _ = run(world, state, mode="dry-run",
                          now=NOW + datetime.timedelta(hours=25))
        self.assertEqual(len(tomorrow.messages), 1)

    def test_fees_above_the_cap_wait_unless_urgent(self):
        world = World(hot=3_000_000, lnd=9_000_000, fee=30)
        the_run, _ = run(world)
        self.assertEqual(world.sent, [])
        self.assertEqual(the_run.messages, [])  # not urgent: logged only
        world = World(hot=1_000_000, lnd=9_000_000, fee=30)
        the_run, _ = run(world)
        self.assertEqual(world.sent[0][3], 30.0)

    def test_no_estimate_uses_the_fallback(self):
        world = World(hot=3_000_000, lnd=9_000_000, fee=None)
        run(world)
        self.assertEqual(world.sent[0][3], CFG.fee_fallback)

    def test_an_address_that_is_not_ours_stops_the_move(self):
        for hot, lnd in ((3_000_000, 9_000_000), (30_000_000, 0)):
            world = World(hot=hot, lnd=lnd)
            world.not_mine = True
            the_run, state = run(world)
            self.assertEqual(world.sent, [])
            self.assertNotIn("inflight", state)
            self.assertIn("could not move", the_run.messages[0][1])

    def test_a_stuck_move_is_bumped(self):
        for hot, lnd, source in ((3_000_000, 9_000_000, "lnd"),
                                 (30_000_000, 0, "hot")):
            world = World(hot=hot, lnd=lnd)
            _, state = run(world)
            txid = state["inflight"]["txid"]
            world.height += 6
            world.fee = 5
            the_run, state = run(world, state)
            self.assertEqual(world.bumped[0][1], 5)
            self.assertIn("bumped", the_run.messages[0][1])
            if source == "lnd":
                self.assertEqual(world.bumped[0][0], f"{txid}:1")
                self.assertEqual(state["inflight"]["txid"], txid)
            else:
                self.assertNotEqual(state["inflight"]["txid"], txid)

    def test_a_conflicted_move_stops_everything(self):
        world = World(hot=3_000_000, lnd=9_000_000)
        _, state = run(world)
        world.txs[state["inflight"]["txid"]]["confirmations"] = -1
        the_run, state = run(world, state)
        self.assertIn("conflicts", the_run.messages[0][1])
        world.txs.clear()
        again, _ = run(world, state, now=NOW + datetime.timedelta(days=2))
        self.assertEqual(len(world.sent), 1)
        self.assertIn("stopped", again.lines[0])

    def test_cold_storage_only_when_configured(self):
        world = World(hot=30_000_000, lnd=12_000_000)
        run(world)
        self.assertEqual(world.sent, [])
        cfg = r.Config(max_swap=2_000_000, cold_address="bcrt1pcold")
        run(world, cfg=cfg)
        self.assertEqual(world.sent[0][1:3], ("bcrt1pcold", 20_000_000))

    def test_cold_address_fingerprint(self):
        env = {"REBALANCE_COLD_ADDRESS": "bc1pcold"}
        self.assertIn("does not match", r.cold_problem(env))
        import hashlib
        env["REBALANCE_COLD_ADDRESS_SHA256"] = hashlib.sha256(
            b"bc1pcold").hexdigest()[:16]
        self.assertIsNone(r.cold_problem(env))
        self.assertIsNone(r.cold_problem({}))


class MainTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.root = self.dir.name
        self.sent = []
        patches = [
            mock.patch.object(r, "DEPLOY", self.root),
            mock.patch.object(r.lfswap_monitor, "send",
                              lambda env, t, x: self.sent.append(x)),
            mock.patch.object(r, "maintenance_on", lambda: False),
        ]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(self.dir.cleanup)

    def env(self, **extra):
        lines = [f"LFSWAP_ROOT={self.root}"] + [f"{k}={v}" for k, v in
                                                extra.items()]
        with open(os.path.join(self.root, ".env"), "w") as f:
            f.write("\n".join(lines) + "\n")

    def state(self):
        with open(os.path.join(self.root, "rebalance", "state.json")) as f:
            return json.load(f)

    def test_off_by_default(self):
        self.env()
        world = World(hot=3_000_000, lnd=9_000_000)
        self.assertEqual(r.main([], runner=world, now=NOW), 0)
        self.assertEqual(world.sent, [])
        self.assertFalse(os.path.exists(os.path.join(self.root, "rebalance")))

    def test_on_moves_and_keeps_its_state_private(self):
        self.env(REBALANCE_MODE="on", MAX_SWAP_SAT=2000000)
        world = World(hot=3_000_000, lnd=9_000_000)
        r.main([], runner=world, now=NOW)
        self.assertEqual(len(world.sent), 1)
        self.assertIn("inflight", self.state())
        path = os.path.join(self.root, "rebalance", "state.json")
        self.assertEqual(os.stat(path).st_mode & 0o777, 0o600)
        self.assertIn("moved", self.sent[0])

    def test_dry_run_flag_sends_and_keeps_nothing(self):
        self.env(REBALANCE_MODE="on")
        world = World(hot=3_000_000, lnd=9_000_000)
        r.main(["--dry-run"], runner=world, now=NOW)
        self.assertEqual(world.sent, [])
        self.assertEqual(self.sent, [])
        self.assertFalse(os.path.exists(
            os.path.join(self.root, "rebalance", "state.json")))

    def test_a_migrated_host_and_maintenance_move_nothing(self):
        self.env(REBALANCE_MODE="on")
        world = World(hot=3_000_000, lnd=9_000_000)
        open(os.path.join(self.root, "MIGRATED"), "w").close()
        r.main([], runner=world, now=NOW)
        os.remove(os.path.join(self.root, "MIGRATED"))
        with mock.patch.object(r, "maintenance_on", lambda: True):
            r.main([], runner=world, now=NOW)
            # --status still reports, and moves nothing
            self.assertEqual(r.main(["--status"], runner=world, now=NOW), 0)
        self.assertEqual(world.sent, [])

    def test_bad_bands_refuse_to_run_and_say_so(self):
        self.env(REBALANCE_MODE="on", REBALANCE_HOT_FLOOR=20000000)
        world = World(hot=3_000_000, lnd=9_000_000)
        self.assertEqual(r.main([], runner=world, now=NOW), 1)
        self.assertEqual(world.sent, [])
        self.assertIn("floor, target and ceiling", self.sent[0])

    def test_a_wrong_cold_address_turns_only_cold_storage_off(self):
        self.env(REBALANCE_MODE="on", REBALANCE_COLD_ADDRESS="bc1pcold",
                 REBALANCE_COLD_ADDRESS_SHA256="0000000000000000")
        world = World(hot=30_000_000, lnd=8_000_000)
        r.main([], runner=world, now=NOW)
        self.assertEqual(world.sent, [])
        self.assertIn("cold storage is off", self.sent[0])
        world = World(hot=3_000_000, lnd=9_000_000)
        r.main([], runner=world, now=NOW)
        self.assertEqual(len(world.sent), 1)

    def test_a_failing_run_is_reported(self):
        self.env(REBALANCE_MODE="on")

        class Broken(World):
            def compose(self, *args, timeout=60):
                raise RuntimeError("docker is down")
        r.main([], runner=Broken(), now=NOW)
        self.assertIn("the run failed: docker is down", self.sent[0])


class SimulationTest(unittest.TestCase):
    def test_it_runs_and_keeps_the_rules(self):
        import io
        import contextlib
        import simulate
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            simulate.main(["--days", "10", "--runs", "2", "--hot", "3000000",
                           "--lnd", "20000000", "--set",
                           "MAX_SWAP_SAT=2000000"])
        text = out.getvalue()
        self.assertIn("2 runs of 10 days", text)
        self.assertIn("moves", text)
        # A starved hot wallet beside a full lnd gets filled
        cfg = r.Config(max_swap=2_000_000)
        demand = simulate.random_demand(random.Random(1), 10, 0, 0.5,
                                        300_000, 2_000_000)
        outcome = simulate.simulate(cfg, demand, 10,
                                    (3_000_000, 20_000_000, 10**8, 10**8), 1)
        self.assertGreaterEqual(outcome.moves, 1)
        self.assertGreaterEqual(outcome.end_hot, CFG.hot_floor)
        self.assertLess(outcome.hot_low, 0.05)

    def test_recorded_demand(self):
        import sqlite3
        import simulate
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "t.db")
            db = sqlite3.connect(path)
            db.execute("CREATE TABLE swap (created INTEGER, type TEXT, "
                       "onchain_amount INTEGER, invoice_amount INTEGER)")
            db.executemany("INSERT INTO swap VALUES (?, ?, ?, ?)", [
                (1_000_000, "reverse", 50_000, 51_000),
                (1_000_000 + 86400 * 2, "submarine", None, 70_000)])
            db.commit()
            db.close()
            demand = simulate.recorded_demand(path, 7)
        kinds = [k for _, k, _ in demand]
        self.assertEqual(demand[0], (0, "reverse", 50_000))
        self.assertIn((96, "submarine", 70_000), demand)
        self.assertGreater(len(kinds), 2)  # repeated to fill the week


class ReportTest(unittest.TestCase):
    def test_moves_fees_and_time_below_floor(self):
        t = NOW.timestamp()
        state = {"moves": [
            {"ts": t - 86400, "amount": 2_000_000, "source": "lnd",
             "dest": "hot", "rule": 1, "fee": 300},
            {"ts": t - 30 * 86400, "amount": 9, "source": "hot",
             "dest": "lnd", "rule": 3, "fee": 9}]}

        class Telemetry:
            class db:
                @staticmethod
                def execute(query, args):
                    class Rows:
                        @staticmethod
                        def fetchall():
                            return [(3_000_000, 500_000, 100_000),
                                    (5_000_000, 250_000, 100_000),
                                    (6_000_000, 350_000, 100_000),
                                    (7_000_000, 500_000, 100_000)]
                    return Rows
        lines = r.report_lines(state, 7, NOW, Telemetry, {})
        text = "\n".join(lines)
        self.assertIn("1 move, 2,000,000 sat, fees 300 sat", text)
        self.assertIn("hot wallet 25% of the time, lnd 25%", text)
        self.assertIn("no moves", "\n".join(r.report_lines({}, 7, NOW)))


if __name__ == "__main__":
    unittest.main()
