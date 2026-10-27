"""Tests for lfswap_retention.py: which swaps lose their user data, and
that nothing changes in report mode but the referral."""

import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import lfswap_retention as r  # noqa: E402

H = "ab" * 32


class FakeMonitor:
    """Answers psql by the first matching piece of the query; records
    every query; Knots knows which lockups are unspent."""

    def __init__(self, answers, unspent=(), donations=True):
        self.answers = answers
        self.unspent = set(unspent)
        self.queries = []
        self.donations = donations

    def psql(self, query, database="boltz"):
        self.queries.append((database, query))
        for piece, rows in self.answers.items():
            if piece in query:
                return [row.split("|") for row in rows]
        return []

    def knots(self, *args):
        assert args[0] == "gettxout"
        return '{"value": 0.001}\n' if args[1] in self.unspent else ""

    def donations_on(self):
        return self.donations

    def writes(self):
        return [q for _, q in self.queries
                if q.lstrip().startswith(("UPDATE", "DELETE", "BEGIN"))]


def swaps_answers():
    return {
        "FROM swaps": [
            f"claimed1|transaction.claimed|{H}|{'01' * 32}|0",
            f"unpaid01|swap.expired|{H}||-1",
            f"refunded|swap.expired|{H}|{'02' * 32}|1",
            f"holding1|invoice.failedToPay|{H}|{'03' * 32}|0",
            f"bad'id|transaction.claimed|{H}||-1",
        ],
        'FROM "reverseSwaps"': [
            "settled1|invoice.settled|" + "04" * 32,
            "neverpaid|swap.expired|",
            "ourlockup|transaction.failed|" + "05" * 32,
            "refundd1|transaction.refunded|" + "06" * 32,
        ],
    }


class Swaps(unittest.TestCase):
    def test_only_swaps_with_nothing_left_are_due(self):
        m = FakeMonitor(swaps_answers(), unspent={"03" * 32})
        ret = r.Retention(m, "report")
        due, kept = ret.submarine_due()
        self.assertEqual([s for s, _ in due], ["claimed1", "unpaid01", "refunded"])
        # The user's lockup is still there: they may still refund it; and
        # an id that does not look like one is never put into SQL
        self.assertEqual(kept, 2)
        due, kept = ret.reverse_due()
        self.assertEqual(due, ["settled1", "neverpaid", "refundd1"])
        self.assertEqual(kept, 1)
        # Only old, final swaps that still hold user data are asked for
        query = next(q for _, q in m.queries if "FROM swaps" in q)
        self.assertIn("interval '90 days'", query)
        self.assertIn("transaction.claimed", query)
        self.assertNotIn("transaction.mempool", query)

    def test_report_mode_changes_no_swap(self):
        m = FakeMonitor(swaps_answers())
        lines = r.Retention(m, "report").run()
        self.assertFalse([q for q in m.writes() if "invoice" in q])
        self.assertTrue(any("submarine 4 to clear" in line for line in lines),
                        lines)

    def test_on_clears_the_user_data_and_keeps_the_numbers(self):
        m = FakeMonitor(swaps_answers(), unspent={"03" * 32})
        r.Retention(m, "on").run()
        batch = next(q for q in m.writes() if q.startswith("BEGIN"))
        self.assertIn("'claimed1', 'unpaid01', 'refunded'", batch)
        self.assertIn("'settled1', 'neverpaid', 'refundd1'", batch)
        for kept in ("holding1", "ourlockup", "bad'id"):
            self.assertNotIn(kept, batch)
        for cleared in ('"refundPublicKey" = NULL', '"claimAddress" = NULL',
                        "invoice = ''", 'DELETE FROM "lightningPayments"',
                        'DELETE FROM "reverseRoutingHints"'):
            self.assertIn(cleared, batch)
        for kept_column in ('"onchainAmount"', "fee =", "status ="):
            self.assertNotIn(kept_column, batch)
        self.assertTrue(batch.rstrip().endswith("COMMIT;"))

    def test_nothing_due_means_no_write(self):
        m = FakeMonitor({})
        r.Retention(m, "on").run()
        self.assertFalse([q for q in m.writes() if q.startswith("BEGIN")])


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
    def test_report_counts_and_on_clears(self):
        m = FakeMonitor({"count(*) FROM replay_checks": ["3"],
                         "count(*) FROM sha256_hints": ["1"]})
        lines = r.Retention(m, "report").run()
        self.assertIn("donations: 3 replay checks' inputs and 1 SHA256 "
                      "proofs to clear", lines)
        self.assertFalse([q for q in m.writes() if "replay_checks" in q])

        m = FakeMonitor({"UPDATE replay_checks": ["1", "UPDATE 1"]})
        lines = r.Retention(m, "on").run()
        update = next(q for q in m.writes() if "UPDATE replay_checks" in q)
        self.assertIn("verdict = 'protected'", update)
        self.assertIn("interval '30 days'", update)
        proofs = next(q for q in m.writes() if "UPDATE sha256_hints" in q)
        # Only once every deposit of the transaction has a decided copy
        self.assertIn("NOT IN ('already', 'copied', 'not_possible')", proofs)
        self.assertIn("donations: 1 replay checks' inputs and 0 SHA256 "
                      "proofs cleared", lines)

    def test_off_when_donations_are(self):
        m = FakeMonitor({}, donations=False)
        r.Retention(m, "on").run()
        self.assertFalse([q for d, q in m.queries if d == "donations"])


if __name__ == "__main__":
    unittest.main()
