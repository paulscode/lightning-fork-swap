"""Tests for lfswap_retention.py: which swaps lose their user data, and
that nothing changes in report mode but the referral."""

import json
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import lfswap_retention as r  # noqa: E402

H = "ab" * 32


def script_of(address):
    """A made-up taproot scriptPubKey, one per address."""
    import hashlib
    return "5120" + hashlib.sha256(address.encode()).hexdigest()


class FakeRunner:
    """Knots: getblockchaininfo and scantxoutset; the addresses in `held`
    still hold a coin."""

    def __init__(self, held=(), synced=True, scan_ok=True):
        self.held = set(held)
        self.synced = synced
        self.scan_ok = scan_ok
        self.scans = []
        self.invalid = set()

    def compose(self, *args, timeout=60):
        args = list(args)
        assert args[:5] == ["exec", "-T", "knots", "bitcoin-cli",
                            "-datadir=/data"], args
        command = args[5]
        if command == "getblockchaininfo":
            return json.dumps({
                "initialblockdownload": not self.synced,
                "blocks": 100, "headers": 100 if self.synced else 120})
        if command == "validateaddress":
            if args[6] in self.invalid:
                return json.dumps({"isvalid": False})
            return json.dumps({"isvalid": True,
                               "scriptPubKey": script_of(args[6])})
        assert command == "scantxoutset" and args[6] == "start"
        assert timeout >= 600
        wanted = json.loads(args[7])
        self.scans.append(wanted)
        # As Knots answers: each coin by the descriptor it infers from the
        # script, not by the addr(...) asked for
        return json.dumps({
            "success": self.scan_ok,
            "unspents": [{"desc": f"rawtr({script_of(a)[4:]})#checksum",
                          "scriptPubKey": script_of(a), "amount": 0.001}
                         for a in self.held if f"addr({a})" in wanted]})


class FakeMonitor:
    """Answers psql by the first matching piece of the query; records
    every query."""

    def __init__(self, answers, held=(), donations=True, synced=True,
                 scan_ok=True):
        self.answers = answers
        self.runner = FakeRunner(held, synced, scan_ok)
        self.queries = []
        self.donations = donations

    def psql(self, query, database="boltz"):
        self.queries.append((database, query))
        for piece, rows in self.answers.items():
            if piece in query:
                return [row.split("|") for row in rows]
        return []

    def donations_on(self):
        return self.donations

    def writes(self):
        return [q for _, q in self.queries
                if q.lstrip().startswith(("UPDATE", "DELETE", "BEGIN"))]


def swaps_answers():
    return {
        "FROM swaps": [
            f"claimed1|{H}|bcrt1qclaimedxxxxxxxxx",
            f"unpaid01|{H}|bcrt1qunpaidxxxxxxxxxx",
            f"refunded|{H}|bcrt1qrefundedxxxxxxxx",
            # The user's lockup is still there, or a second deposit is
            f"holding1|{H}|bcrt1qholdingxxxxxxxxx",
            f"bad'id|{H}|bcrt1qbadidxxxxxxxxxxx",
        ],
        'FROM "reverseSwaps"': [
            "settled1|bcrt1psettledxxxxxxxxx",
            "neverpaid|bcrt1pneverpaidxxxxxxx",
            # Settled, but the user never claimed: their rescue key needs it
            "unclaimed|bcrt1punclaimedxxxxxxx",
            "refundd1|bcrt1prefundedxxxxxxxx",
        ],
    }


HELD = {"bcrt1qholdingxxxxxxxxx", "bcrt1punclaimedxxxxxxx"}


class Swaps(unittest.TestCase):
    def test_a_coin_at_the_lockup_address_keeps_the_swap(self):
        m = FakeMonitor(swaps_answers(), held=HELD)
        sub, rev, kept = r.Retention(m, "report").due()
        self.assertEqual([s for s, _ in sub], ["claimed1", "unpaid01", "refunded"])
        self.assertEqual(rev, ["settled1", "neverpaid", "refundd1"])
        self.assertEqual(kept, (1, 1))
        # One scan for every candidate; an id that does not look like one
        # is never used
        self.assertEqual(len(m.runner.scans), 1)
        self.assertEqual(len(m.runner.scans[0]), 8)
        self.assertNotIn("addr(bcrt1qbadidxxxxxxxxxxx)", m.runner.scans[0])
        query = next(q for _, q in m.queries if "FROM swaps" in q)
        self.assertIn("interval '90 days'", query)
        self.assertNotIn("transaction.mempool", query)

    def test_an_address_knots_cannot_read_keeps_the_swap(self):
        m = FakeMonitor(swaps_answers())
        m.runner.invalid = {"bcrt1qrefundedxxxxxxxx"}
        sub, _, kept = r.Retention(m, "report").due()
        self.assertNotIn("refunded", [s for s, _ in sub])
        self.assertEqual(kept[0], 1)
        self.assertNotIn("addr(bcrt1qrefundedxxxxxxxx)", m.runner.scans[0])

    def test_an_unsynced_node_clears_nothing(self):
        m = FakeMonitor(swaps_answers(), synced=False)
        with self.assertRaises(RuntimeError):
            r.Retention(m, "on").swaps()
        self.assertFalse([q for q in m.writes() if q.startswith("BEGIN")])
        self.assertFalse(m.runner.scans)

    def test_an_incomplete_scan_clears_nothing(self):
        m = FakeMonitor(swaps_answers(), scan_ok=False)
        with self.assertRaises(RuntimeError):
            r.Retention(m, "on").swaps()
        self.assertFalse([q for q in m.writes() if q.startswith("BEGIN")])

    def test_report_mode_changes_no_swap(self):
        m = FakeMonitor(swaps_answers(), held=HELD)
        lines = r.Retention(m, "report").run()
        self.assertFalse([q for q in m.writes() if "invoice" in q])
        self.assertTrue(any("submarine 3 to clear" in line for line in lines),
                        lines)

    def test_report_mode_counts_what_is_overdue(self):
        ret = r.Retention(FakeMonitor(swaps_answers(), held=HELD), "report")
        ret.run()
        self.assertEqual(ret.overdue, 6)
        ret = r.Retention(FakeMonitor(swaps_answers(), held=HELD), "on")
        ret.run()
        self.assertEqual(ret.overdue, 0)

    def test_on_clears_the_user_data_and_keeps_the_numbers(self):
        m = FakeMonitor(swaps_answers(), held=HELD)
        r.Retention(m, "on").run()
        batch = next(q for q in m.writes() if q.startswith("BEGIN"))
        self.assertIn("'claimed1', 'unpaid01', 'refunded'", batch)
        self.assertIn("'settled1', 'neverpaid', 'refundd1'", batch)
        for kept in ("holding1", "unclaimed", "bad'id"):
            self.assertNotIn(kept, batch)
        for cleared in ('"refundPublicKey" = NULL', '"claimAddress" = NULL',
                        "invoice = 'redacted:' || id",
                        'DELETE FROM "lightningPayments"',
                        'DELETE FROM "reverseRoutingHints"'):
            self.assertIn(cleared, batch)
        for kept_column in ('"onchainAmount"', "fee =", "status ="):
            self.assertNotIn(kept_column, batch)
        self.assertTrue(batch.rstrip().endswith("COMMIT;"))

    def test_a_failing_part_does_not_stop_the_others(self):
        m = FakeMonitor({"SET referral": ["1", "UPDATE 1"]}, synced=False)
        ret = r.Retention(m, "on")
        ret.run()
        self.assertEqual(len(ret.errors), 1)
        self.assertTrue(ret.errors[0].startswith("swaps: "))
        self.assertTrue([q for q in m.writes() if "SET referral" in q])
        self.assertTrue([q for d, q in m.queries if d == "donations"])

    def test_nothing_due_means_no_write_and_no_scan(self):
        m = FakeMonitor({})
        r.Retention(m, "on").run()
        self.assertFalse([q for q in m.writes() if q.startswith("BEGIN")])
        self.assertFalse(m.runner.scans)


class Referral(unittest.TestCase):
    def test_made_the_same_in_both_modes(self):
        for mode in ("report", "on"):
            m = FakeMonitor({"SET referral": ["1", "1", "UPDATE 2"]})
            lines = r.Retention(m, mode).run()
            updates = [q for q in m.writes() if "SET referral" in q]
            self.assertEqual(len(updates), 2, mode)
            self.assertIn("lightning_fork_swap_mobile", updates[0])
            # psql's command tag is not a row
            self.assertTrue(any("on 4 swaps" in line for line in lines), lines)


class Donations(unittest.TestCase):
    def test_cleared_in_both_modes(self):
        for mode in ("report", "on"):
            m = FakeMonitor({"SET inputs": ["1", "UPDATE 1"],
                             "SET at_risk_addresses": [],
                             "SET proofs": ["1", "1", "UPDATE 2"]})
            lines = r.Retention(m, mode).run()
            self.assertIn("donations: cleared 1 replay checks' inputs or "
                          "addresses and 2 SHA256 proofs", lines, mode)
        inputs = next(q for q in m.writes() if "SET inputs" in q)
        self.assertIn("verdict = 'protected' AND first_seen < now() - "
                      "interval '30 days'", inputs)
        self.assertIn("verdict = 'at_risk' AND first_seen < now() - "
                      "interval '400 days'", inputs)
        # A replayed donation is kept for its return
        self.assertNotIn("replayed", inputs)
        proofs = next(q for q in m.writes() if "SET proofs" in q)
        # Only once every deposit of the transaction has a decided copy
        self.assertIn("copy_state NOT IN ('already', 'copied', 'not_possible')", proofs)
        self.assertIn("state NOT IN ('gone', 'spent')", proofs)
        self.assertIn("interval '365 days'", proofs)
        # Not counted as overdue: report mode does them
        ret = r.Retention(FakeMonitor({"SET inputs": ["1"]}), "report")
        ret.run()
        self.assertEqual(ret.overdue, 0)

    def test_off_when_donations_are(self):
        m = FakeMonitor({}, donations=False)
        r.Retention(m, "on").run()
        self.assertFalse([q for d, q in m.queries if d == "donations"])


if __name__ == "__main__":
    unittest.main()
