"""Tests for lfswap_monitor.py: python3 -m unittest discover deploy/monitor"""

import collections
import datetime
import json
import os
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(__file__))
import lfswap_monitor as m  # noqa: E402

NOW = datetime.datetime(2026, 10, 7, 12, 0, tzinfo=datetime.timezone.utc)
Usage = collections.namedtuple("Usage", "total used free")


def healthy():
    """Command outputs of a healthy service at height 976000."""
    return {
        ("ps",): "\n".join(
            json.dumps({"Service": s, "State": "running"})
            for s in m.SERVICES + ["guard"]
        ).replace('"guard", "State": "running"', '"guard", "State": "exited"'),
        ("knots", "getblockchaininfo"): json.dumps(
            {"blocks": 976000, "headers": 976000, "bestblockhash": "tip"}
        ),
        ("knots", "getblockheader"): json.dumps(
            {"time": NOW.timestamp() - 600}
        ),
        ("knots", "getchaintips"): json.dumps(
            [{"height": 976000, "hash": "tip", "branchlen": 0,
              "status": "active"},
             {"height": 975990, "hash": "stale1", "branchlen": 1,
              "status": "valid-fork"}]
        ),
        ("knots", "-rpcwallet=boltz"): json.dumps(
            {"mine": {"trusted": 0.3, "untrusted_pending": 0, "immature": 0}}
        ),
        ("lnd", "getinfo"): json.dumps(
            {"synced_to_chain": True, "num_peers": 3, "block_height": 976000}
        ),
        ("lnd", "listchannels"): json.dumps(
            {"channels": [{"chan_id": "1", "pending_htlcs": [
                {"incoming": True, "hash_lock": "ab" * 32,
                 "expiration_height": 976200}]}]}
        ),
        ("postgres",): "",
        ("logs",): "",
    }


class FakeRunner:
    def __init__(self, outputs, files=None, disk=(100, 50, 50)):
        self.outputs = outputs
        self.files = files or {}
        self.disk = Usage(*disk)
        self.queries = []
        self.calls = []

    def compose(self, *args, timeout=60):
        self.calls.append(args)
        if args[0] == "ps":
            return self.lookup(("ps",))
        if args[0] == "logs":
            return self.lookup(("logs",))
        service = args[2]
        if service == "postgres":
            query = args[-1]
            self.queries.append(query)
            for fragment, out in self.outputs.get("sql", {}).items():
                if fragment in query:
                    return out
            return ""
        # exec -T <service> <cli> <its first option> <command> ...
        return self.lookup((service, args[5]))

    def lookup(self, key):
        value = self.outputs[key]
        if isinstance(value, Exception):
            raise value
        return value

    def run(self, args, timeout=60):
        if args[0] == "openssl":
            return self.outputs.get("openssl", "notAfter=Jan  5 12:00:00 2027 GMT")
        raise RuntimeError("unexpected")

    def read(self, path):
        if path in self.files:
            return self.files[path]
        raise OSError(path)

    def read_bytes(self, path):
        if path in self.files:
            return self.files[path]
        raise OSError(path)

    def mtime(self, path):
        return self.files.get(path + ":mtime", NOW.timestamp())

    def disk_usage(self, path):
        return self.disk


def run(outputs=None, env=None, **kwargs):
    runner = FakeRunner(outputs or healthy(), **kwargs)
    monitor = m.Monitor(runner, env or {}, NOW)
    alerts = monitor.run_all("2026-10-07T11:55:00Z", "/srv/lfswap",
                             "lightningfork.com")
    return {a.key: a for a in alerts}, runner


class ChecksTest(unittest.TestCase):
    def test_a_healthy_service_raises_nothing(self):
        alerts, _ = run()
        self.assertEqual(alerts, {})

    def test_a_stopped_container(self):
        outputs = healthy()
        outputs[("ps",)] = outputs[("ps",)].replace(
            '"lnd", "State": "running"', '"lnd", "State": "restarting"')
        alerts, _ = run(outputs)
        self.assertIn("container:lnd", alerts)
        self.assertIn("restarting", alerts["container:lnd"].text)

    def test_compose_ps_as_one_array(self):
        outputs = healthy()
        outputs[("ps",)] = json.dumps(
            [{"Service": s, "State": "running"} for s in m.SERVICES])
        alerts, _ = run(outputs)
        self.assertNotIn("container:knots", alerts)

    def test_a_stale_tip_and_a_node_behind(self):
        outputs = healthy()
        outputs[("knots", "getblockheader")] = json.dumps(
            {"time": NOW.timestamp() - 3 * 3600})
        outputs[("knots", "getblockchaininfo")] = json.dumps(
            {"blocks": 976000, "headers": 976010, "bestblockhash": "tip"})
        alerts, _ = run(outputs)
        self.assertIn("chain:stale", alerts)
        self.assertIn("180 minutes", alerts["chain:stale"].text)
        self.assertIn("chain:behind", alerts)

    def test_the_tip_age_threshold_is_configurable(self):
        outputs = healthy()
        outputs[("knots", "getblockheader")] = json.dumps(
            {"time": NOW.timestamp() - 3 * 3600})
        alerts, _ = run(outputs, env={"MONITOR_MAX_TIP_AGE_MIN": "240"})
        self.assertNotIn("chain:stale", alerts)

    def test_a_lost_branch_of_two_blocks_is_an_event(self):
        outputs = healthy()
        outputs[("knots", "getchaintips")] = json.dumps(
            [{"height": 976000, "hash": "tip", "branchlen": 0,
              "status": "active"},
             {"height": 975999, "hash": "lost", "branchlen": 2,
              "status": "valid-fork"},
             {"height": 960000, "hash": "ancient", "branchlen": 3,
              "status": "valid-fork"},
             {"height": 975998, "hash": "headers", "branchlen": 4,
              "status": "headers-only"}])
        alerts, _ = run(outputs)
        self.assertIn("chain:fork:lost", alerts)
        self.assertTrue(alerts["chain:fork:lost"].event)
        self.assertNotIn("chain:fork:ancient", alerts)
        self.assertNotIn("chain:fork:headers", alerts)

    def test_a_wallet_below_its_floor(self):
        alerts, _ = run(env={"MONITOR_MIN_WALLET_SAT": "50000000"})
        self.assertIn("wallet:low", alerts)
        self.assertIn("30,000,000 sat", alerts["wallet:low"].text)
        alerts, _ = run(env={"MONITOR_MIN_WALLET_SAT": "10000000"})
        self.assertNotIn("wallet:low", alerts)

    def test_lnd_without_peers_or_unsynced(self):
        outputs = healthy()
        outputs[("lnd", "getinfo")] = json.dumps(
            {"synced_to_chain": False, "num_peers": 0, "block_height": 976000})
        alerts, _ = run(outputs)
        self.assertIn("lnd:unsynced", alerts)
        self.assertIn("lnd:peers", alerts)
        alerts, _ = run(outputs, env={"MONITOR_MIN_PEERS": "0"})
        self.assertNotIn("lnd:peers", alerts)

    def test_an_htlc_close_to_its_expiry(self):
        outputs = healthy()
        outputs[("lnd", "listchannels")] = json.dumps({"channels": [
            {"chan_id": "7", "pending_htlcs": [
                {"incoming": True, "hash_lock": "cd" * 32,
                 "expiration_height": 976020},
                {"incoming": False, "hash_lock": "ef" * 32,
                 "expiration_height": 976025}]}]})
        alerts, _ = run(outputs)
        self.assertIn("lnd:htlc:" + "cd" * 32, alerts)
        self.assertIn("expires in 20 blocks",
                      alerts["lnd:htlc:" + "cd" * 32].text)
        self.assertNotIn("lnd:htlc:" + "ef" * 32, alerts)

    def test_swap_states(self):
        outputs = healthy()
        outputs["sql"] = {
            '"reverseSwaps" WHERE status IN': "rev1|transaction.confirmed|976020\n",
            "status = 'invoice.pending'": "sub1|75\n",
            "FROM swaps WHERE status = 'transaction.claim.pending'": "sub2|45\n",
            '"lightningPayments"': "sub3|invoice.failedToPay\n",
        }
        alerts, runner = run(outputs)
        self.assertIn("reverse:near-timeout:rev1", alerts)
        self.assertIn("20 blocks", alerts["reverse:near-timeout:rev1"].text)
        self.assertIn("submarine:pending:sub1", alerts)
        self.assertIn("claim:pending:sub2", alerts)
        self.assertIn("submarine:paid-not-claimed:sub3", alerts)
        self.assertTrue(any("- 976000 <= 30" in q for q in runner.queries))

    def test_log_lines_that_need_a_person(self):
        outputs = healthy()
        outputs[("logs",)] = "\n".join([
            "07/10/2026 11:56:00:000 info: Locked up 100000 BTC",
            "07/10/2026 11:57:00:000 error: Lockup of Reverse Swap abc may "
            "have been broadcast before this error; not failing the swap",
            "\x1b[2m2026-10-07T11:58:00Z\x1b[0m \x1b[33m WARN\x1b[0m UTXO "
            "nursery BTC block stream lagged behind by 3 messages",
            "07/10/2026 11:59:00:000 warn: Not locking up Reverse Swap xyz: "
            "could not check whether the BTC wallet already sent",
        ])
        alerts, runner = run(outputs)
        texts = [a.text for a in alerts.values() if a.key.startswith("log:")]
        self.assertEqual(len(texts), 3)
        self.assertTrue(all(a.event for a in alerts.values()))
        self.assertFalse(any("\x1b" in t for t in texts))
        logs_call = [c for c in runner.calls if c[0] == "logs"][0]
        self.assertIn("2026-10-07T11:55:00Z", logs_call)

    def test_disk_and_pressure(self):
        files = {
            "/proc/pressure/io": "some avg10=40.00 avg60=35.00 avg300=30.00 "
            "total=1\nfull avg10=20.00 avg60=18.00 avg300=15.50 total=1\n",
            "/proc/pressure/memory": "some avg10=1.00 avg60=1.00 avg300=1.00 "
            "total=1\nfull avg10=0.50 avg60=0.40 avg300=0.30 total=1\n",
        }
        alerts, _ = run(files=files, disk=(100, 95, 5))
        self.assertIn("host:disk", alerts)
        self.assertIn("host:pressure:io", alerts)
        self.assertNotIn("host:pressure:memory", alerts)

    def test_a_channel_backup_that_was_not_copied(self):
        scb = "/srv/lfswap/lnd/data/chain/bitcoin/mainnet/channel.backup"
        copy = "/srv/lfswap/secrets/channel.backup"
        files = {scb: b"new", copy: b"old",
                 scb + ":mtime": NOW.timestamp() - 3 * 3600}
        alerts, _ = run(files=files)
        self.assertIn("backup:channel", alerts)
        # Changed recently: the hourly copy has not run yet
        files[scb + ":mtime"] = NOW.timestamp() - 1800
        alerts, _ = run(files=files)
        self.assertNotIn("backup:channel", alerts)
        files[copy] = b"new"
        files[scb + ":mtime"] = NOW.timestamp() - 3 * 3600
        alerts, _ = run(files=files)
        self.assertNotIn("backup:channel", alerts)

    def test_a_certificate_about_to_expire(self):
        outputs = healthy()
        outputs["openssl"] = "notAfter=Oct 15 12:00:00 2026 GMT\n"
        alerts, _ = run(outputs)
        self.assertIn("host:certificate", alerts)
        self.assertIn("8 days", alerts["host:certificate"].text)

    def test_a_check_that_fails_is_an_alert_and_the_others_still_run(self):
        outputs = healthy()
        outputs[("lnd", "getinfo")] = RuntimeError("lnd: exit 1: wallet locked")
        outputs[("knots", "-rpcwallet=boltz")] = "not json"
        alerts, _ = run(outputs, env={"MONITOR_MIN_WALLET_SAT": "1"})
        self.assertIn("check:lnd", alerts)
        self.assertIn("wallet locked", alerts["check:lnd"].text)
        self.assertIn("check:wallet", alerts)
        self.assertNotIn("check:chain", alerts)


class PlanTest(unittest.TestCase):
    def test_new_repeated_and_cleared(self):
        t0 = NOW
        cond = m.Alert("lnd:peers", "lnd has 0 peers")
        outcome, state = m.plan([cond], {}, t0, 6, 0)
        self.assertEqual([a.key for a in outcome["new"]], ["lnd:peers"])

        outcome, state = m.plan([cond], state, t0 + datetime.timedelta(
            hours=1), 6, 0)
        self.assertEqual(outcome["new"] + outcome["repeated"], [])
        self.assertIsNone(m.message(outcome, 1))

        outcome, state = m.plan([cond], state, t0 + datetime.timedelta(
            hours=6, minutes=5), 6, 0)
        self.assertEqual([a.key for a in outcome["repeated"]], ["lnd:peers"])

        outcome, state = m.plan([], state, t0 + datetime.timedelta(hours=7),
                                6, 0)
        self.assertEqual([a.key for a in outcome["cleared"]], ["lnd:peers"])
        self.assertEqual(state["active"], {})
        title, text = m.message(outcome, 0)
        self.assertIn("1 cleared", title)
        self.assertIn("CLEARED lnd has 0 peers", text)

    def test_events_are_sent_once(self):
        event = m.Alert("log:abc", "Backend: Abandoning Swap x", event=True)
        outcome, state = m.plan([event], {}, NOW, 6, 0)
        self.assertEqual(len(outcome["new"]), 1)
        outcome, state = m.plan([event], state, NOW + datetime.timedelta(
            hours=10), 6, 0)
        self.assertEqual(outcome["new"], [])
        # Forgotten after a week
        _, state = m.plan([], state, NOW + datetime.timedelta(days=8), 6, 0)
        self.assertEqual(state["events"], {})

    def test_heartbeat(self):
        outcome, _ = m.plan([], {"heartbeat": NOW.timestamp() - 25 * 3600},
                            NOW, 6, 24)
        self.assertTrue(outcome["heartbeat"])
        self.assertIn("Monitor running", m.message(outcome, 0)[1])
        outcome, _ = m.plan([], {"heartbeat": NOW.timestamp() - 3600}, NOW,
                            6, 24)
        self.assertIsNone(m.message(outcome, 0))
        outcome, _ = m.plan([], {}, NOW, 6, 0)
        self.assertFalse(outcome["heartbeat"])


class PayloadTest(unittest.TestCase):
    def test_kinds(self):
        body, headers = m.payload("discord", "T", "x", {})
        self.assertEqual(json.loads(body), {"content": "**T**\nx"})
        body, _ = m.payload("slack", "T", "x", {})
        self.assertEqual(json.loads(body), {"text": "*T*\nx"})
        body, _ = m.payload("telegram", "T", "x",
                            {"ALERT_TELEGRAM_CHAT_ID": "42"})
        self.assertEqual(json.loads(body)["chat_id"], "42")
        body, headers = m.payload("ntfy", "T", "x", {})
        self.assertEqual((body, headers["Title"]), (b"x", "T"))
        body, headers = m.payload("json", "T", "x", {})
        self.assertEqual(json.loads(body), {"title": "T", "text": "x"})
        self.assertEqual(headers["Content-Type"], "application/json")

    def test_discord_is_cut_to_its_limit(self):
        body, _ = m.payload("discord", "T", "x" * 5000, {})
        self.assertLessEqual(len(json.loads(body)["content"]), 2000)


class MainTest(unittest.TestCase):
    """main() end to end with a fake runner and a temporary state dir."""

    def setUp(self):
        self.dir = tempfile.mkdtemp()
        self.deploy = os.path.join(self.dir, "deploy")
        self.root = os.path.join(self.dir, "srv")
        os.makedirs(self.deploy)
        os.makedirs(self.root)
        self.write_env("")
        self.outputs = healthy()
        self.sent = []

    def write_env(self, extra):
        with open(os.path.join(self.deploy, ".env"), "w") as f:
            f.write(f"LFSWAP_ROOT={self.root}\nNETWORK=mainnet\n"
                    "MONITOR_HEARTBEAT_HOURS=0\n" + extra)

    def main(self, *argv, fail_send=False):
        outputs = self.outputs

        def fake_send(env, title, text):
            if fail_send:
                raise RuntimeError("webhook down")
            if not env.get("ALERT_WEBHOOK_URL"):
                return False
            self.sent.append((title, text))
            return True

        with mock.patch.object(m, "DEPLOY", self.deploy), \
                mock.patch.object(m, "Runner",
                                  lambda deploy: FakeRunner(outputs)), \
                mock.patch.object(m, "send", fake_send), \
                mock.patch("sys.stdout"), mock.patch("sys.stderr"):
            return m.main(list(argv))

    def state(self):
        with open(os.path.join(self.root, "monitor", "state.json")) as f:
            return json.load(f)

    def test_alerts_are_sent_once_and_state_kept(self):
        self.write_env("ALERT_WEBHOOK_URL=https://example.invalid/hook\n")
        self.outputs[("lnd", "getinfo")] = json.dumps(
            {"synced_to_chain": True, "num_peers": 0, "block_height": 976000})
        self.assertEqual(self.main(), 0)
        self.assertEqual(len(self.sent), 1)
        self.assertIn("lnd has 0 peers", self.sent[0][1])
        self.assertIn("lnd:peers", self.state()["active"])
        self.assertEqual(self.main(), 0)
        self.assertEqual(len(self.sent), 1)

    def test_a_failed_send_is_retried_next_run(self):
        self.write_env("ALERT_WEBHOOK_URL=https://example.invalid/hook\n")
        self.outputs[("lnd", "getinfo")] = json.dumps(
            {"synced_to_chain": True, "num_peers": 0, "block_height": 976000})
        self.assertEqual(self.main(fail_send=True), 1)
        self.assertFalse(os.path.exists(
            os.path.join(self.root, "monitor", "state.json")))
        self.assertEqual(self.main(), 0)
        self.assertEqual(len(self.sent), 1)

    def test_dry_run_keeps_no_state(self):
        self.assertEqual(self.main("--dry-run"), 0)
        self.assertFalse(os.path.exists(os.path.join(self.root, "monitor")))

    def test_a_migrated_host_is_not_watched(self):
        with open(os.path.join(self.root, "MIGRATED"), "w") as f:
            f.write("moved")
        self.write_env("ALERT_WEBHOOK_URL=https://example.invalid/hook\n")
        self.outputs[("ps",)] = ""
        self.assertEqual(self.main(), 0)
        self.assertEqual(self.sent, [])

    def test_the_state_dir_is_private(self):
        self.main()
        mode = os.stat(os.path.join(self.root, "monitor")).st_mode & 0o777
        self.assertEqual(mode, 0o700)


class SendTest(unittest.TestCase):
    def test_send_posts_to_the_webhook(self):
        import http.server
        import threading

        received = []

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                length = int(self.headers["Content-Length"])
                received.append((self.headers["Content-Type"],
                                 self.rfile.read(length)))
                self.send_response(204)
                self.end_headers()

            def log_message(self, *args):
                pass

        server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.handle_request)
        thread.start()
        try:
            env = {"ALERT_WEBHOOK_URL":
                   f"http://127.0.0.1:{server.server_port}/hook",
                   "ALERT_WEBHOOK_KIND": "slack"}
            self.assertTrue(m.send(env, "T", "x"))
        finally:
            thread.join(5)
            server.server_close()
        self.assertEqual(received[0][0], "application/json")
        self.assertEqual(json.loads(received[0][1]), {"text": "*T*\nx"})

    def test_no_url_sends_nothing(self):
        self.assertFalse(m.send({}, "T", "x"))


if __name__ == "__main__":
    unittest.main()
