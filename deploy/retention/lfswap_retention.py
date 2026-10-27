#!/usr/bin/env python3
"""What the databases keep about users, and for how long. Daily (cron,
scripts/migrate/lib.sh).

RETENTION_MODE in .env governs the swaps (1): "report" (the default) counts
what it would change, "on" changes it. The referral (2) and the donations
(3) are done in both modes: nothing depends on what they remove.

1. Backend swaps. 90 days after a swap's last change, once nothing is left
   to claim or refund, the user's data in it goes: the invoice (its payment
   hash stays), preimage, the user's public key and address, the redeem
   script; with it the swap's routing hints, metadata and Lightning payment
   attempts. Id, pair, amounts, fees, status and dates stay: the weekly
   report and the accounts need them.
   Nothing left means:
   - a submarine swap: we claimed it, it was never paid, or its lockup
     output is spent (asked of Knots: gettxout);
   - a reverse swap: the user claimed it, we refunded our lockup, or we
     never locked up.
   Any other swap is kept whole, however old: a user may still need it to
   rescue their coins.
2. Every swap: the referral the web app sent (once "..._mobile" or
   "..._desktop") becomes the one it sends now, in both modes; nothing reads
   it.
3. Donations: a protected donation's inputs after 30 days, an at-risk
   one's inputs and at-risk addresses after 400 days (its rechecks end
   after a year; the verdict, txid and amount stay; a replayed one is kept
   for its return); a SHA256-chain donation's BIP322 proofs 90 days after
   their copy was decided (relayed, there already, or impossible). Return
   addresses stay: a later deposit to the same request still needs one.

   lfswap_retention.py              as RETENTION_MODE says
   lfswap_retention.py --report     count only
   lfswap_retention.py --deploy deploy/regtest --mode on \
       --knots-arg=-regtest --knots-arg=-rpcuser=lab --knots-arg=-rpcpassword=lab
                                    the regtest service (rehearsals)
"""

import argparse
import datetime
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
DEPLOY = os.path.dirname(HERE)
sys.path.insert(0, os.path.join(DEPLOY, "monitor"))
import lfswap_monitor  # noqa: E402

SWAP_DAYS = 90
REPLAY_DAYS = 30
AT_RISK_DAYS = 400
HINT_DAYS = 90
REFERRAL = "lightning_fork_swap"
OLD_REFERRALS = ("lightning_fork_swap_mobile", "lightning_fork_swap_desktop")

SUBMARINE_FINAL = ("transaction.claimed", "invoice.failedToPay",
                   "swap.expired", "transaction.lockupFailed",
                   "transaction.refunded")
REVERSE_FINAL = ("invoice.settled", "invoice.expired", "swap.expired",
                 "transaction.failed", "transaction.refunded")

ID = re.compile(r"^[A-Za-z0-9]{6,64}$")
HEX = re.compile(r"^[0-9a-f]{64}$")


def quoted(values):
    return ", ".join(f"'{v}'" for v in values)


def counted(rows):
    """The number a SELECT count(*) answered."""
    return int(rows[0][0]) if rows and rows[0] and rows[0][0].isdigit() else 0


def returned(rows):
    """Rows of an UPDATE ... RETURNING 1 (psql also prints the command's
    tag, "UPDATE 3")."""
    return sum(1 for row in rows if row == ["1"])


class Retention:
    def __init__(self, monitor, mode, knots_args=()):
        self.m = monitor
        self.mode = mode
        # Before the command: what another node needs (regtest's login)
        self.knots_args = tuple(knots_args)
        self.lines = []
        # What report mode left in place that is past its time
        self.overdue = 0

    def say(self, text):
        self.lines.append(text)

    # --- 1. backend swaps -------------------------------------------------

    def submarine_due(self):
        """Old submarine swaps still holding user data, and whether each
        has nothing left: (id, preimage hash) of those that may go, and
        how many are kept for an unspent lockup."""
        rows = self.m.psql(
            'SELECT id, status, "preimageHash", '
            'coalesce("lockupTransactionId", \'\'), '
            'coalesce("lockupTransactionVout", -1) FROM swaps '
            f'WHERE status IN ({quoted(SUBMARINE_FINAL)}) '
            f'AND "updatedAt" < now() - interval \'{SWAP_DAYS} days\' '
            'AND (invoice IS NOT NULL OR "refundPublicKey" IS NOT NULL '
            'OR "refundAddress" IS NOT NULL OR "redeemScript" IS NOT NULL '
            'OR preimage IS NOT NULL)')
        due, kept = [], 0
        for swap_id, status, preimage_hash, txid, vout in rows:
            if not ID.match(swap_id) or not HEX.match(preimage_hash):
                kept += 1
                continue
            if status == "transaction.claimed" or not txid:
                due.append((swap_id, preimage_hash))
                continue
            # The user's lockup: gone from the UTXO set means spent (claimed
            # or refunded); still there means they may still refund it
            if HEX.match(txid) and int(vout) >= 0 and \
                    not self.m.knots(*self.knots_args, "gettxout", txid,
                                     str(vout)).strip():
                due.append((swap_id, preimage_hash))
            else:
                kept += 1
        return due, kept

    def reverse_due(self):
        rows = self.m.psql(
            'SELECT id, status, coalesce("transactionId", \'\') '
            'FROM "reverseSwaps" '
            f'WHERE status IN ({quoted(REVERSE_FINAL)}) '
            f'AND "updatedAt" < now() - interval \'{SWAP_DAYS} days\' '
            'AND (invoice NOT LIKE \'redacted:%\' OR "claimPublicKey" IS NOT NULL '
            'OR "claimAddress" IS NOT NULL OR "redeemScript" IS NOT NULL '
            'OR preimage IS NOT NULL OR "minerFeeInvoice" IS NOT NULL)')
        due, kept = [], 0
        for swap_id, status, txid in rows:
            if not ID.match(swap_id):
                kept += 1
            elif status in ("invoice.settled", "transaction.refunded") \
                    or not txid:
                due.append(swap_id)
            else:
                # Our lockup, neither claimed nor refunded as far as the
                # status says: keep it for whoever sorts it out
                kept += 1
        return due, kept

    def swaps(self):
        submarine, sub_kept = self.submarine_due()
        reverse, rev_kept = self.reverse_due()
        self.say(f"swaps past {SWAP_DAYS} days with user data: submarine "
                 f"{len(submarine)} to clear, {sub_kept} kept (lockup "
                 f"unspent or unreadable); reverse {len(reverse)} to clear, "
                 f"{rev_kept} kept (our lockup open)")
        if self.mode != "on":
            self.overdue += len(submarine) + len(reverse)
        if self.mode != "on" or not (submarine or reverse):
            return
        sql = ["BEGIN;"]
        if submarine:
            ids = quoted(s for s, _ in submarine)
            hashes = quoted(h for _, h in submarine)
            sql += [
                'UPDATE swaps SET invoice = NULL, preimage = NULL, '
                '"refundPublicKey" = NULL, "refundAddress" = NULL, '
                f'"redeemScript" = NULL WHERE id IN ({ids});',
                f'DELETE FROM "lightningPayments" WHERE "preimageHash" IN ({hashes});',
                f'DELETE FROM swap_metadata WHERE swap_id IN ({ids});',
            ]
        if reverse:
            ids = quoted(reverse)
            sql += [
                # The invoice may not be NULL, and is unique: a placeholder
                # of the swap's own
                'UPDATE "reverseSwaps" SET invoice = \'redacted:\' || id, '
                'preimage = NULL, '
                '"claimPublicKey" = NULL, "claimAddress" = NULL, '
                '"redeemScript" = NULL, "minerFeeInvoice" = NULL, '
                f'"minerFeeInvoicePreimage" = NULL WHERE id IN ({ids});',
                f'DELETE FROM "reverseRoutingHints" WHERE "swapId" IN ({ids});',
                f'DELETE FROM swap_metadata WHERE swap_id IN ({ids});',
            ]
        sql.append("COMMIT;")
        self.m.psql("\n".join(sql))
        self.say(f"cleared {len(submarine)} submarine and {len(reverse)} "
                 "reverse swaps")

    # --- 2. the referral --------------------------------------------------

    def referral(self):
        n = 0
        for table in ("swaps", '"reverseSwaps"'):
            rows = self.m.psql(
                f"UPDATE {table} SET referral = '{REFERRAL}' "
                f"WHERE referral IN ({quoted(OLD_REFERRALS)}) RETURNING 1")
            n += returned(rows)
        if n:
            self.say(f"referral made the same on {n} swaps")

    # --- 3. donations -----------------------------------------------------

    def donations(self):
        if not self.m.donations_on():
            return
        cleared = returned(self.m.psql(
            "UPDATE replay_checks SET inputs = '[]'::jsonb "
            "WHERE inputs <> '[]'::jsonb AND ("
            "(verdict = 'protected' "
            f"AND first_seen < now() - interval '{REPLAY_DAYS} days') OR "
            "(verdict = 'at_risk' "
            f"AND first_seen < now() - interval '{AT_RISK_DAYS} days')) "
            "RETURNING 1", "donations"))
        cleared += returned(self.m.psql(
            "UPDATE replay_checks SET at_risk_addresses = '{}' "
            "WHERE verdict = 'at_risk' AND at_risk_addresses <> '{}' "
            f"AND first_seen < now() - interval '{AT_RISK_DAYS} days' "
            "RETURNING 1", "donations"))
        # Proofs are needed until every SHA256-chain deposit of their
        # transaction has a decided copy
        proofs = returned(self.m.psql(
            "UPDATE sha256_hints SET proofs = '[]'::jsonb "
            "WHERE proofs <> '[]'::jsonb "
            f"AND at < now() - interval '{HINT_DAYS} days' "
            "AND NOT EXISTS (SELECT 1 FROM sha256_deposits d "
            "WHERE d.request_id = sha256_hints.request_id "
            "AND d.txid = sha256_hints.txid AND d.chain = 'sha256' "
            "AND d.copy_state NOT IN ('already', 'copied', 'not_possible')) "
            "RETURNING 1", "donations"))
        self.say(f"donations: cleared {cleared} replay checks' inputs or "
                 f"addresses and {proofs} SHA256 proofs")

    def run(self):
        self.swaps()
        self.referral()
        self.donations()
        return self.lines


def main(argv=None, runner=None, now=None):
    parser = argparse.ArgumentParser()
    parser.add_argument("--report", action="store_true",
                        help="count only, whatever RETENTION_MODE says")
    parser.add_argument("--deploy", default=DEPLOY,
                        help="the compose project's folder (with its .env)")
    parser.add_argument("--mode", choices=("report", "on"),
                        help="instead of RETENTION_MODE")
    parser.add_argument("--knots-arg", action="append", default=[],
                        help="an argument for bitcoin-cli before the command")
    args = parser.parse_args(argv)
    env = lfswap_monitor.load_env(os.path.join(args.deploy, ".env"))
    mode = "report" if args.report else (
        args.mode or env.get("RETENTION_MODE", "report"))
    if mode not in ("report", "on"):
        print(f"RETENTION_MODE must be report or on, not {mode}",
              file=sys.stderr)
        return 1
    now = now or datetime.datetime.now(datetime.timezone.utc)
    monitor = lfswap_monitor.Monitor(
        runner or lfswap_monitor.Runner(args.deploy), env, now)
    stamp = now.strftime("%Y-%m-%dT%H:%M:%SZ")
    retention = Retention(monitor, mode, args.knots_arg)
    for line in retention.run():
        print(f"{stamp} [{mode}] {line}")
    # The privacy page says these go: report mode must not keep them quietly
    if retention.overdue and not args.report:
        lfswap_monitor.send(
            env, "Lightning Fork Swap: retention",
            f"{retention.overdue} records are past the time the privacy page "
            "gives them, and RETENTION_MODE is report: rehearse on regtest, "
            "then set it to on (OPERATING.md, Logs and users' privacy)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
