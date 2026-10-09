"""Tests for lfswap_telemetry.py: python3 -m unittest discover deploy/monitor"""

import collections
import datetime
import io
import json
import os
import sqlite3
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(__file__))
import lfswap_monitor as m  # noqa: E402
import lfswap_telemetry as t  # noqa: E402

NOW = datetime.datetime(2026, 10, 9, 12, 0, tzinfo=datetime.timezone.utc)
Usage = collections.namedtuple("Usage", "total used free")


class Fake:
    """Answers `docker compose` calls by the command after the CLI's first
    option (knots and lnd), SQL by a fragment, and logs by service."""

    def __init__(self):
        self.cli = {}
        self.sql = []
        self.logs = {"boltz": ""}
        self.files = {}
        self.calls = []

    def compose(self, *args, timeout=60):
        self.calls.append(args)
        if args[0] == "logs":
            return self.logs.get(args[-1], "")
        service = args[2]
        if service == "postgres":
            query = args[-1]
            for fragment, out in self.sql:
                if fragment in query:
                    return out(query) if callable(out) else out
            return ""
        command = args[5:]
        for key in sorted(self.cli, key=len, reverse=True):
            if tuple(command[:len(key)]) == key:
                value = self.cli[key]
                if isinstance(value, Exception):
                    raise value
                return value(command) if callable(value) else value
        raise RuntimeError(f"no fake for {service} {command}")

    def read(self, path):
        if path in self.files:
            return self.files[path]
        raise OSError(path)

    def read_bytes(self, path):
        if path in self.files:
            value = self.files[path]
            return value if isinstance(value, bytes) else value.encode()
        raise OSError(path)

    def disk_usage(self, path):
        return Usage(1000, 600, 400)


def service_fake(local=500_000, remote=300_000, fee_ppm=500, peers=("p1",
                                                                    "p2")):
    f = Fake()
    f.cli = {
        ("getblockchaininfo",): json.dumps(
            {"blocks": 976000, "headers": 976001, "bestblockhash": "tip"}),
        ("getchaintips",): json.dumps([
            {"height": 976000, "hash": "tip", "branchlen": 0,
             "status": "active"},
            {"height": 975990, "hash": "stale1", "branchlen": 2,
             "status": "valid-fork"},
            {"height": 975000, "hash": "bad", "branchlen": 1,
             "status": "invalid"},
            {"height": 970000, "hash": "ancient", "branchlen": 1,
             "status": "valid-fork"}]),
        ("-rpcwallet=boltz", "getbalances"): json.dumps(
            {"mine": {"trusted": 0.3, "untrusted_pending": 0.001}}),
        ("-rpcwallet=boltz", "listsinceblock"): json.dumps({"transactions": [
            {"txid": "aa" * 32, "category": "send", "amount": -0.0002,
             "fee": -0.00000154, "confirmations": 3, "blocktime": 1000600,
             "timereceived": 1000000},
            {"txid": "bb" * 32, "category": "receive", "amount": 0.01,
             "confirmations": 3, "blocktime": 1000600},
            {"txid": "cc" * 32, "category": "send", "amount": -0.01,
             "fee": -0.000001, "confirmations": 0, "timereceived": 1000700},
        ]}),
        ("getblockhash",): "hash-at-975994\n",
        ("getblockheader",): json.dumps({"time": 2000900}),
        ("listchaintxns",): json.dumps({"transactions": [
            {"tx_hash": "dd" * 32, "amount": "-1000154", "total_fees": "154",
             "num_confirmations": 2, "block_hash": "b1", "time_stamp":
             "2000000", "label": "0:openchannel:shortchanid-1"},
            {"tx_hash": "ee" * 32, "amount": "500000", "total_fees": "0",
             "num_confirmations": 2, "block_hash": "b1"},
            {"tx_hash": "ff" * 32, "amount": "-2000", "total_fees": "100",
             "num_confirmations": 0, "time_stamp": "2000950"}]}),
        ("estimatesmartfee", "2"): json.dumps({"feerate": 0.00002}),
        ("estimatesmartfee", "6"): json.dumps({"feerate": 0.00001}),
        ("estimatesmartfee", "24"): json.dumps({"errors": ["none"]}),
        ("walletbalance",): json.dumps({
            "confirmed_balance": "1000000", "unconfirmed_balance": "0",
            "reserved_balance_anchor_chan": "100000", "locked_balance": "0"}),
        ("channelbalance",): json.dumps({
            "local_balance": {"sat": str(local)},
            "remote_balance": {"sat": str(remote)},
            "pending_open_local_balance": {"sat": "0"}}),
        ("feereport",): json.dumps({"channel_fees": [
            {"chan_id": "11", "base_fee_msat": "1000",
             "fee_per_mil": str(fee_ppm)}]}),
        ("listchannels",): json.dumps({"channels": [
            {"chan_id": "11", "remote_pubkey": "p1", "capacity": "2000000",
             "local_balance": str(local), "remote_balance": str(remote),
             "active": True, "initiator": True, "peer_alias": "One",
             "total_satoshis_sent": "1000", "total_satoshis_received": "50",
             "uptime": "90", "lifetime": "100",
             "pending_htlcs": [{"amount": "20000"}]},
            {"chan_id": "22", "remote_pubkey": "p2", "capacity": "1000000",
             "local_balance": "0", "remote_balance": "990000",
             "active": False, "pending_htlcs": []}]}),
        ("listpeers",): json.dumps(
            {"peers": [{"pub_key": p} for p in peers]}),
        ("fwdinghistory",): json.dumps({"forwarding_events": [
            {"timestamp_ns": "1700000000000000000", "chan_id_in": "11",
             "chan_id_out": "22", "amt_in": "10010", "amt_out": "10000",
             "fee_msat": "10000"}]}),
        ("listpayments",): json.dumps({"payments": [
            {"payment_hash": "h1", "payment_index": "1", "status": "SUCCEEDED",
             "value_sat": "19829", "fee_sat": "1",
             "creation_time_ns": "1700000100000000000",
             "htlcs": [{"status": "FAILED", "route": {"hops": [
                 {"chan_id": "22"}, {"chan_id": "33"}]},
                 "failure": {"code": "TEMPORARY_CHANNEL_FAILURE",
                             "failure_source_index": 1}},
                 {"status": "SUCCEEDED", "route": {"hops": [
                     {"chan_id": "11"}]}}]},
            {"payment_hash": "h2", "payment_index": "2",
             "status": "IN_FLIGHT", "htlcs": []},
            {"payment_hash": "h3", "payment_index": "3", "status": "FAILED",
             "failure_reason": "FAILURE_REASON_NO_ROUTE", "htlcs": []}]}),
        ("closedchannels",): json.dumps({"channels": [
            {"chan_id": "9", "remote_pubkey": "p9", "capacity": "500000",
             "close_type": "REMOTE_FORCE_CLOSE", "settled_balance": "400000",
             "close_height": 975000}]}),
    }
    f.sql = [
        ("count(*), coalesce(sum(\"onchainAmount\"), 0) FROM \"reverseSwaps\"",
         "2|39000\n"),
        ("count(*) FROM swaps WHERE status IN ('invoice.set'", "1\n"),
        ('FROM swaps WHERE "updatedAt"',
         "s1|transaction.claimed|1000|1100|19829|20000|20|152|1000|"
         "|2026-10-09 10:00:00.1+00\n"),
        ('FROM "reverseSwaps" WHERE "updatedAt"',
         "r1|invoice.settled|1000|1200|20000|19746|100|146|0|"
         "bad/reason|2026-10-09 10:05:00.1+00\n"),
    ]
    f.files["/proc/meminfo"] = "MemTotal: 2000 kB\nMemAvailable:  512 kB\n"
    f.files["/proc/pressure/io"] = (
        "some avg10=0 avg60=0 avg300=0 total=0\n"
        "full avg10=0.5 avg60=1.00 avg300=2.50 total=0\n")
    return f


class Base(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.path = os.path.join(self.dir.name, "telemetry", "telemetry.db")
        self.t = t.Telemetry(self.path)

    def tearDown(self):
        self.t.close()
        self.dir.cleanup()

    def monitor(self, fake, now=NOW):
        return m.Monitor(fake, {}, now)

    def rows(self, query, *args):
        return self.t.db.execute(query, args).fetchall()


class RecordTest(Base):
    def test_a_snapshot_has_every_figure(self):
        failed = self.t.record(self.monitor(service_fake()), "/srv")
        self.assertEqual(failed, [])
        snap = dict(zip(
            [c[1] for c in self.rows("PRAGMA table_info(snapshot)")],
            self.rows("SELECT * FROM snapshot")[0]))
        self.assertEqual(snap["ts"], int(NOW.timestamp()))
        self.assertEqual(snap["height"], 976000)
        self.assertEqual(snap["header_lag"], 1)
        self.assertEqual(snap["hot_confirmed"], 30_000_000)
        self.assertEqual(snap["hot_unconfirmed"], 100_000)
        self.assertEqual(snap["lnd_confirmed"], 1_000_000)
        self.assertEqual(snap["lnd_reserve"], 100_000)
        self.assertEqual((snap["outbound"], snap["inbound"]),
                         (500_000, 300_000))
        self.assertEqual((snap["channels_active"], snap["channels_total"]),
                         (1, 2))
        self.assertEqual(snap["peers"], 2)
        self.assertEqual((snap["reverse_locked"],
                          snap["reverse_locked_count"]), (39_000, 2))
        self.assertEqual(snap["submarine_pending"], 1)
        self.assertEqual((snap["fee_2"], snap["fee_6"], snap["fee_24"]),
                         (2.0, 1.0, None))
        self.assertEqual(snap["mem_available"], 512 * 1024)
        self.assertEqual(snap["pressure_io"], 2.5)
        self.assertEqual((snap["disk_used"], snap["disk_total"]), (600, 1000))

    def test_channels_are_recorded_with_their_policy(self):
        # The first run is a baseline: channels it finds were not opened then
        self.t.record(self.monitor(service_fake()), "/srv")
        self.assertEqual(self.rows(
            "SELECT * FROM event WHERE kind = 'channel_open'"), [])
        fake = service_fake()
        listed = json.loads(fake.cli[("listchannels",)])
        listed["channels"].append({"chan_id": "33", "remote_pubkey": "p3",
                                   "capacity": "5000000", "active": True,
                                   "initiator": False})
        fake.cli[("listchannels",)] = json.dumps(listed)
        self.t.record(self.monitor(fake, NOW + datetime.timedelta(
            minutes=5)), "/srv")
        rows = self.rows("SELECT chan_id, peer, local, remote, active, htlcs, "
                         "htlc_amount, uptime, lifetime, base_fee_msat, "
                         "fee_ppm FROM channel_snapshot WHERE ts = ? "
                         "ORDER BY chan_id", int(NOW.timestamp()))
        self.assertEqual(rows[0], ("11", "p1", 500000, 300000, 1, 1, 20000,
                                   90, 100, 1000, 500))
        self.assertEqual(rows[1][:5], ("22", "p2", 0, 990000, 0))
        opens = self.rows("SELECT key, data FROM event WHERE kind = "
                          "'channel_open'")
        self.assertEqual([k for k, _ in opens], ["33"])
        self.assertFalse(json.loads(opens[0][1])["initiator"])

    def test_a_policy_change_is_a_change_of_terms(self):
        self.t.record(self.monitor(service_fake(fee_ppm=500)), "/srv")
        self.assertEqual(self.rows("SELECT * FROM terms_change"), [])
        later = NOW + datetime.timedelta(minutes=5)
        self.t.record(self.monitor(service_fake(fee_ppm=800), later), "/srv")
        self.assertEqual(
            self.rows("SELECT key, old, new FROM terms_change"),
            [("policy:11", "[1000, 500]", "[1000, 800]")])
        # Known channels: no open event on later runs either
        self.assertEqual(self.rows(
            "SELECT * FROM event WHERE kind = 'channel_open'"), [])

    def test_peers_coming_and_going(self):
        self.t.record(self.monitor(service_fake(peers=("p1", "p2"))), "/srv")
        self.assertEqual(self.rows(
            "SELECT * FROM event WHERE kind LIKE 'peer_%'"), [])
        later = NOW + datetime.timedelta(minutes=5)
        self.t.record(self.monitor(service_fake(peers=("p1", "p3")), later),
                      "/srv")
        kinds = sorted((k, json.loads(d)["peer"]) for k, d in self.rows(
            "SELECT kind, data FROM event WHERE kind LIKE 'peer_%' AND ts = ?",
            int(later.timestamp())))
        self.assertEqual(kinds, [("peer_offline", "p2"), ("peer_online", "p3")])

    def test_swaps_and_when_each_status_was_seen(self):
        fake = service_fake()
        self.t.record(self.monitor(fake), "/srv")
        rows = self.rows("SELECT id, type, status, invoice_amount, "
                         "onchain_amount, fee, miner_fee, routing_fee_msat, "
                         "failure FROM swap ORDER BY id")
        self.assertEqual(rows[0], ("r1", "reverse", "invoice.settled", 20000,
                                   19746, 100, 146, 0, "bad/reason"))
        self.assertEqual(rows[1], ("s1", "submarine", "transaction.claimed",
                                   19829, 20000, 20, 152, 1000, None))
        # The cursor moved to the newest update
        self.assertEqual(self.t.meta("swaps_since"),
                         "2026-10-09 10:05:00.1+00")
        # A later status adds to the record, the first one's time stays
        fake.sql[2] = ('FROM swaps WHERE "updatedAt"',
                       "s1|swap.expired|1000|1300|19829|20000|20|152|1000|"
                       "|2026-10-09 11:00:00.1+00\n")
        later = NOW + datetime.timedelta(minutes=5)
        self.t.record(self.monitor(fake, later), "/srv")
        statuses = json.loads(self.rows(
            "SELECT statuses FROM swap WHERE id = 's1'")[0][0])
        self.assertEqual(statuses, {
            "transaction.claimed": int(NOW.timestamp()),
            "swap.expired": int(later.timestamp())})
        query = [c for c in fake.calls if c[2] == "postgres"
                 and '"updatedAt" >' in c[-1]][-1][-1]
        self.assertIn("'2026-10-09 10:05:00.1+00'", query)

    def test_forwards_and_payments_once_each(self):
        fake = service_fake()
        for minutes in (0, 5):
            self.t.record(self.monitor(
                fake, NOW + datetime.timedelta(minutes=minutes)), "/srv")
        self.assertEqual(len(self.rows(
            "SELECT * FROM event WHERE kind = 'forward'")), 1)
        payments = {k: json.loads(d) for k, d in self.rows(
            "SELECT key, data FROM event WHERE kind = 'payment'")}
        # The one in flight stops the reading: h3 waits until h2 is done
        self.assertEqual(sorted(payments), ["h1"])
        self.assertEqual(payments["h1"]["attempts"][0]["first_hop"], "22")
        self.assertEqual(payments["h1"]["attempts"][0]["failure"],
                         "TEMPORARY_CHANNEL_FAILURE")
        self.assertEqual(payments["h1"]["attempts"][1]["first_hop"], "11")
        self.assertEqual(self.t.meta("payments_index"), 1)
        offsets = [c[c.index("--index_offset") + 1] for c in fake.calls
                   if "listpayments" in c]
        self.assertEqual(offsets, ["0", "1"])

    def test_our_transactions_and_lost_branches(self):
        fake = service_fake()
        self.t.record(self.monitor(fake), "/srv")
        txs = {k: json.loads(d) for k, d in self.rows(
            "SELECT key, data FROM event WHERE kind = 'tx'")}
        self.assertEqual(sorted(txs), ["aa" * 32, "dd" * 32])
        self.assertEqual(txs["aa" * 32], {
            "wallet": "hot", "fee_sat": 154, "amount_sat": 20000,
            "confirm_seconds": 600})
        self.assertEqual(txs["dd" * 32], {
            "wallet": "lnd", "fee_sat": 154, "amount_sat": 1000154,
            "label": "0:openchannel:shortchanid-1", "confirm_seconds": 900})
        self.assertEqual(self.t.meta("knots_since_block"), "hash-at-975994")
        self.assertEqual(self.t.meta("lnd_since_height"), 975994)
        reorgs = self.rows("SELECT key, data FROM event WHERE kind = 'reorg'")
        self.assertEqual([k for k, _ in reorgs], ["stale1"])
        self.assertEqual(json.loads(reorgs[0][1])["depth"], 2)
        closes = self.rows("SELECT key FROM event WHERE kind = "
                           "'channel_close'")
        self.assertEqual(closes, [("9",)])

    def test_one_failing_source_does_not_lose_the_others(self):
        fake = service_fake()
        fake.cli[("listpayments",)] = RuntimeError("lnd: exit 1")
        fake.cli[("estimatesmartfee", "2")] = RuntimeError("no estimate")
        failed = self.t.record(self.monitor(fake), "/srv")
        self.assertEqual(len(failed), 1)
        self.assertTrue(failed[0].startswith("payments:"))
        snap = self.rows("SELECT hot_confirmed, fee_2, fee_6 FROM snapshot")
        self.assertEqual(snap, [(30_000_000, None, 1.0)])


class LogsTest(Base):
    def test_refusals_by_kind_and_creations_by_status_without_addresses(self):
        fake = service_fake()
        fake.logs["boltz"] = "\n".join([
            'warn: Request POST /v2/swap/reverse {"invoiceAmount":5000000} '
            'failed: {"error":"5000000 exceeds maximal of 2000000"}',
            'warn: Request POST /v2/swap/reverse {} failed: '
            '{"error":"insufficient liquidity"}',
            'warn: Request POST /v2/swap/submarine {} failed: '
            '{"error":"could not find route to pay invoice"}',
            'warn: Request POST /v2/swap/submarine failed: internal error',
            'warn: Request GET /v2/swap/status failed: {"error":"x"}',
            "info: something else",
        ])
        log = ("1.2.3.4 - - [09/Oct/2026:07:00:00 -0400] \"POST "
               "/v2/swap/reverse HTTP/2.0\" 201 300 \"-\" \"ua\"\n"
               "1.2.3.4 - - [09/Oct/2026:07:00:01 -0400] \"POST "
               "/v2/swap/reverse HTTP/2.0\" 400 30 \"-\" \"ua\"\n"
               "5.6.7.8 - - [09/Oct/2026:07:00:02 -0400] \"GET "
               "/v2/swap/reverse HTTP/2.0\" 200 30 \"-\" \"ua\"\n")
        fake.files["/var/log/nginx/access.log"] = log
        self.t.record_logs(self.monitor(fake), "2026-10-09T11:55:00Z",
                           "/var/log/nginx/access.log")
        counts = {json.loads(d)["name"]: json.loads(d)["n"] for (d,) in
                  self.rows("SELECT data FROM event WHERE kind = 'count'")}
        self.assertEqual(counts, {
            "refusal:reverse:over_maximum": 1,
            "refusal:reverse:no_liquidity": 1,
            "refusal:submarine:no_route": 1,
            "refusal:submarine:internal": 1,
            "create:reverse:201": 1,
            "create:reverse:400": 1,
        })
        dump = "\n".join(self.t.db.iterdump())
        self.assertNotIn("1.2.3.4", dump)
        self.assertNotIn("5.6.7.8", dump)

        # The next run reads only what was added
        fake.files["/var/log/nginx/access.log"] = log + (
            "1.2.3.4 - - [09/Oct/2026:07:05:00 -0400] \"POST "
            "/v2/swap/submarine HTTP/2.0\" 201 300 \"-\" \"ua\"\n")
        fake.logs["boltz"] = ""
        self.t.record_logs(self.monitor(fake), "2026-10-09T12:00:00Z",
                           "/var/log/nginx/access.log")
        counts = {json.loads(d)["name"]: json.loads(d)["n"] for (d,) in
                  self.rows("SELECT data FROM event WHERE kind = 'count'")}
        self.assertEqual(counts["create:reverse:201"], 1)
        self.assertEqual(counts["create:submarine:201"], 1)

    def test_a_rotated_access_log_is_read_from_its_start(self):
        fake = service_fake()
        line = ("9.9.9.9 - - [x] \"POST /v2/swap/reverse HTTP/2.0\" 201 1 "
                "\"-\" \"ua\"\n")
        fake.files["/var/log/nginx/access.log"] = "A" * 300 + "\n" + line * 3
        self.t.record_logs(self.monitor(fake), "x", "/var/log/nginx/access.log")
        # Rotated: another file, longer than the offset but different
        fake.files["/var/log/nginx/access.log"] = "B" * 300 + "\n" + line * 5
        self.t.record_logs(self.monitor(fake), "x", "/var/log/nginx/access.log")
        total = sum(json.loads(d)["n"] for (d,) in self.rows(
            "SELECT data FROM event WHERE kind = 'count'"))
        self.assertEqual(total, 8)

    def test_classify(self):
        self.assertEqual(t.classify_refusal("10 is less than minimal of 100"),
                         "under_minimum")
        self.assertEqual(t.classify_refusal("invoice expiry too short"),
                         "invoice_expiry")
        self.assertEqual(t.classify_refusal("something new"), "other")


class TermsTest(Base):
    def test_changes_of_terms_with_old_and_new(self):
        env = {"SUBMARINE_FEE_PERCENT": "0.1", "MAX_SWAP_SAT": "2000000",
               "REBALANCE_HOT_TARGET": "10000000", "POSTGRES_PASSWORD": "x"}
        self.t.record_terms(env, 100)
        self.assertEqual(self.rows("SELECT * FROM terms_change"), [])
        env = dict(env, SUBMARINE_FEE_PERCENT="0.2", REBALANCE_MODE="dry-run")
        self.t.record_terms(env, 200)
        self.assertEqual(sorted(self.rows("SELECT * FROM terms_change")), [
            (200, "REBALANCE_MODE", "", "dry-run"),
            (200, "SUBMARINE_FEE_PERCENT", "0.1", "0.2")])
        # Secrets are never part of the terms
        self.assertNotIn("POSTGRES_PASSWORD", json.dumps(self.t.meta("terms")))


class RollupTest(Base):
    def snapshots(self, day, values):
        begin = int(datetime.datetime.combine(
            day, datetime.time(), datetime.timezone.utc).timestamp())
        for i, (hot, locked) in enumerate(values):
            self.t.db.execute(
                "INSERT INTO snapshot (ts, hot_confirmed, outbound, inbound, "
                "reverse_locked) VALUES (?, ?, ?, ?, ?)",
                (begin + i * 300, hot, 1000, 2000, locked))

    def test_days_are_rolled_up_once_complete_and_again_for_two_days(self):
        d1 = datetime.date(2026, 10, 6)
        self.snapshots(d1, [(100, 0), (200, 10), (300, 50)])
        self.snapshots(d1 + datetime.timedelta(1), [(400, 5)])
        self.t.db.execute(
            "INSERT INTO swap VALUES ('s1', 'submarine', 'transaction.claimed',"
            " ?, ?, 20000, 20200, 20, 150, 0, NULL, 0, '{}')",
            (int(datetime.datetime(2026, 10, 6, 23, 0,
                                   tzinfo=datetime.timezone.utc).timestamp()),
             0))
        self.t.db.execute(
            "INSERT INTO swap VALUES ('r1', 'reverse', 'invoice.settled', ?, ?,"
            " 5000, 4900, 25, 140, 0, NULL, 0, '{}')",
            (int(datetime.datetime(2026, 10, 6, 1, 0,
                                   tzinfo=datetime.timezone.utc).timestamp()),
             0))
        self.t.rollup(datetime.datetime(2026, 10, 8, 0, 5,
                                        tzinfo=datetime.timezone.utc))
        day = dict(self.rows("SELECT metric, value FROM daily WHERE day = ?",
                             "2026-10-06"))
        self.assertEqual(day["hot:min"], 100)
        self.assertEqual(day["hot:max"], 300)
        self.assertEqual(day["hot:p50"], 200)
        self.assertEqual(day["reverse_locked:p95"], 50)
        self.assertEqual(day["snapshots"], 3)
        self.assertEqual(day["submarine:volume"], 20000)
        self.assertEqual(day["reverse:volume"], 5000)
        self.assertEqual(day["net_flow"], 15000)
        self.assertEqual(self.t.meta("rolled_up_to"), "2026-10-07")
        # Today is never rolled up
        self.assertEqual(self.rows(
            "SELECT count(*) FROM daily WHERE day = '2026-10-08'"), [(0,)])

        # A swap of the 7th that finished late is counted on the next run
        self.t.db.execute(
            "INSERT INTO swap VALUES ('s2', 'submarine', 'transaction.claimed',"
            " ?, ?, 7000, 7100, 7, 150, 0, NULL, 0, '{}')",
            (int(datetime.datetime(2026, 10, 7, 23, 50,
                                   tzinfo=datetime.timezone.utc).timestamp()),
             0))
        self.t.rollup(datetime.datetime(2026, 10, 8, 0, 10,
                                        tzinfo=datetime.timezone.utc))
        self.assertEqual(self.rows(
            "SELECT value FROM daily WHERE day = '2026-10-07' AND metric = "
            "'submarine:volume'"), [(7000,)])

    def test_old_snapshots_are_pruned_and_rollups_kept(self):
        old = NOW - datetime.timedelta(days=t.SNAPSHOT_DAYS + 2)
        self.snapshots(old.date(), [(1, 0)])
        self.snapshots(NOW.date(), [(2, 0)])
        self.t.rollup(NOW)
        self.assertEqual(self.rows("SELECT hot_confirmed FROM snapshot"),
                         [(2,)])
        self.assertTrue(self.rows("SELECT * FROM daily WHERE day = ?",
                                  old.date().isoformat()))

    def test_percentile(self):
        self.assertIsNone(t.percentile([], 0.5))
        self.assertEqual(t.percentile([5], 0.95), 5)
        self.assertEqual(t.percentile(list(range(101)), 0.95), 95)


class AnalysisTest(Base):
    def test_not_enough_history(self):
        self.assertIn("Not enough history",
                      t.analysis(self.t, {}, NOW)[0])

    def test_findings(self):
        start = int(NOW.timestamp()) - 3 * 86400
        for i in range(48):
            self.t.db.execute(
                "INSERT INTO snapshot (ts, hot_confirmed, outbound, inbound, "
                "reverse_locked) VALUES (?, ?, ?, ?, ?)",
                (start + i * 3600, 30_000_000, 80_000_000 - i * 100_000,
                 10_000_000, 4_000_000 if i % 10 == 0 else 0))
            self.t.db.execute(
                "INSERT INTO channel_snapshot (ts, chan_id, local) VALUES "
                "(?, '11', 1), (?, '22', 1)", (start + i * 3600,
                                                start + i * 3600))
        for day, sub, rev in (("2026-10-07", 3_000_000, 1_000_000),
                              ("2026-10-08", 2_000_000, 0)):
            self.t.db.execute("INSERT INTO daily VALUES (?, "
                              "'submarine:volume', ?)", (day, sub))
            self.t.db.execute("INSERT INTO daily VALUES (?, "
                              "'reverse:volume', ?)", (day, rev))
        self.t.count(start, "refusal:reverse:no_liquidity", 3)
        self.t.count(start, "refusal:submarine:over_maximum")
        self.t.event(start, "payment", "h1", {"attempts": [
            {"status": "SUCCEEDED", "first_hop": "11"}]})
        self.t.event(start, "reorg", "x", {"depth": 2})
        self.t.db.execute("INSERT INTO terms_change VALUES (?, "
                          "'SUBMARINE_FEE_PERCENT', '0.1', '0.2')", (start,))
        text = "\n".join(t.analysis(
            self.t, {"MAX_SWAP_SAT": "2000000",
                     "REBALANCE_HOT_TARGET": "10000000"}, NOW, days=7))
        self.assertIn("p95 4,000,000 sat, peak 4,000,000 sat", text)
        self.assertIn("held 7.5 times what reverse swaps needed", text)
        self.assertIn("hot target of 10,000,000 sat is 2.5 times", text)
        self.assertIn("net 4,000,000 sat toward Lightning", text)
        self.assertIn("outbound (75,300,000 sat) lasts about 132 days", text)
        self.assertIn("reverse no_liquidity 3", text)
        self.assertIn("demand went unserved", text)
        self.assertIn("more than the maximum swap", text)
        self.assertIn("first hops of our payments: 11 100 %", text)
        self.assertIn("1 of 2 channels carried none of our payments: 22", text)
        self.assertIn("deepest 2 blocks", text)
        self.assertIn("SUBMARINE_FEE_PERCENT: 0.1 -> 0.2", text)
        block = t.sizing_block(self.t, {"MAX_SWAP_SAT": "2000000"}, NOW)
        self.assertEqual(block[0], "Sizing (last 7 days)")
        self.assertTrue(all(line.startswith("  ") for line in block[1:]))


class ExportTest(Base):
    def test_csv_and_unknown_tables(self):
        self.t.record_terms({"MAX_SWAP_SAT": "1"}, 1)
        self.t.record_terms({"MAX_SWAP_SAT": "2"}, 2)
        out = io.StringIO()
        self.t.export("terms_change", out)
        self.assertEqual(out.getvalue().splitlines(),
                         ["ts,key,old,new", "2,MAX_SWAP_SAT,1,2"])
        with self.assertRaises(ValueError):
            self.t.export("meta; DROP TABLE swap", io.StringIO())


class MonitorIntegrationTest(unittest.TestCase):
    def test_a_failing_source_is_an_alert_and_off_writes_nothing(self):
        with tempfile.TemporaryDirectory() as root:
            fake = service_fake()
            fake.cli[("closedchannels",)] = RuntimeError("down")
            monitor = m.Monitor(fake, {}, NOW)
            m.record_telemetry(monitor, {}, root, "x")
            alerts = {a.key: a.text for a in monitor.alerts}
            self.assertIn("check:telemetry", alerts)
            self.assertIn("closed_channels", alerts["check:telemetry"])
            self.assertTrue(os.path.exists(m.telemetry_path(root)))
            mode = os.stat(m.telemetry_path(root)).st_mode & 0o777
            self.assertEqual(mode, 0o600)

        with tempfile.TemporaryDirectory() as root:
            monitor = m.Monitor(service_fake(), {}, NOW)
            m.record_telemetry(monitor, {"MONITOR_TELEMETRY": "off"}, root, "x")
            self.assertFalse(os.path.exists(m.telemetry_path(root)))
            self.assertEqual(monitor.alerts, [])

    def test_an_unwritable_database_is_an_alert(self):
        with tempfile.TemporaryDirectory() as root:
            os.makedirs(os.path.join(root, "telemetry", "telemetry.db"))
            monitor = m.Monitor(service_fake(), {}, NOW)
            m.record_telemetry(monitor, {}, root, "x")
            self.assertIn("database", {a.key: a.text for a in
                                       monitor.alerts}["check:telemetry"])


if __name__ == "__main__":
    unittest.main()
