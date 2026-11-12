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
   they were sent, once their copy is decided (relayed, there already,
   impossible, the coin gone or spent, or its request no longer looked
   at). Return
   addresses stay: a later deposit to the same request still needs one.

   lfswap_retention.py              as RETENTION_MODE says
   lfswap_retention.py --report     count only
   lfswap_retention.py --deploy deploy/regtest --mode on \
       --knots-arg=-regtest --knots-arg=-rpcuser=lab --knots-arg=-rpcpassword=lab
                                    the regtest service (rehearsals)
"""

import argparse
import json
import datetime
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
DEPLOY = os.path.dirname(HERE)
sys.path.insert(0, os.path.join(DEPLOY, "monitor"))
import lfswap_monitor  # noqa: E402

SWAP_DAYS = 90
LAST_SUCCESS = "last-success"
REPLAY_DAYS = 30
AT_RISK_DAYS = 400
HINT_DAYS = 90
# A SHA256 donation request is looked at for a year (PollEvery)
POLL_DAYS = 365
REFERRAL = "lightning_fork_swap"
OLD_REFERRALS = ("lightning_fork_swap_mobile", "lightning_fork_swap_desktop")

SUBMARINE_FINAL = ("transaction.claimed", "invoice.failedToPay",
                   "swap.expired", "transaction.lockupFailed",
                   "transaction.refunded")
REVERSE_FINAL = ("invoice.settled", "invoice.expired", "swap.expired",
                 "transaction.failed", "transaction.refunded")

ID = re.compile(r"^[A-Za-z0-9]{6,64}$")
ADDRESS = re.compile(r"^[A-Za-z0-9]{14,100}$")
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

    def synced(self):
        """Whether the node's UTXO set is current: in initial block download,
        or behind its headers, a coin still there may look spent."""
        info = json.loads(self.knots("getblockchaininfo"))
        return (not info.get("initialblockdownload", True)
                and info.get("blocks") == info.get("headers"))

    def knots(self, *args, timeout=60):
        return self.m.runner.compose(
            "exec", "-T", "knots", "bitcoin-cli", "-datadir=/data",
            *self.knots_args, *args, timeout=timeout)

    def addresses_with_coins(self, addresses):
        """The addresses that still hold a coin, by one scan of the UTXO set:
        every coin at the address counts, not only the one the database
        recorded (a second deposit, a late one, one never recorded)."""
        if not addresses:
            return set()
        scan = json.loads(self.knots(
            "scantxoutset", "start",
            json.dumps([f"addr({a})" for a in sorted(addresses)]),
            timeout=1800))
        if scan.get("success") is not True:
            raise RuntimeError("the UTXO set scan did not complete")
        held = set()
        for unspent in scan.get("unspents", []):
            found = re.match(r"^addr\(([^)]+)\)", unspent.get("desc", ""))
            if found:
                held.add(found.group(1))
        return held

    def submarine_candidates(self):
        """Old, final submarine swaps still holding user data: (id,
        preimage hash, lockup address)."""
        rows = self.m.psql(
            'SELECT id, "preimageHash", "lockupAddress" FROM swaps '
            f'WHERE status IN ({quoted(SUBMARINE_FINAL)}) '
            f'AND "updatedAt" < now() - interval \'{SWAP_DAYS} days\' '
            'AND (invoice IS NOT NULL OR "refundPublicKey" IS NOT NULL '
            'OR "refundAddress" IS NOT NULL OR "redeemScript" IS NOT NULL '
            'OR preimage IS NOT NULL)')
        return [row for row in rows if len(row) == 3]

    def reverse_candidates(self):
        """Old, final reverse swaps still holding user data: (id, lockup
        address)."""
        rows = self.m.psql(
            'SELECT id, "lockupAddress" FROM "reverseSwaps" '
            f'WHERE status IN ({quoted(REVERSE_FINAL)}) '
            f'AND "updatedAt" < now() - interval \'{SWAP_DAYS} days\' '
            'AND (invoice NOT LIKE \'redacted:%\' OR "claimPublicKey" IS NOT NULL '
            'OR "claimAddress" IS NOT NULL OR "redeemScript" IS NOT NULL '
            'OR preimage IS NOT NULL OR "minerFeeInvoice" IS NOT NULL)')
        return [row for row in rows if len(row) == 2]

    def due(self):
        """The swaps whose user data may go: final, old, and nothing left at
        their lockup address. A coin still there keeps the swap whole: the
        user may refund it (submarine) or claim it with the rescue key
        (reverse, settled but not claimed). Ids, hashes and addresses that
        do not look like ones are never put into SQL or the scan."""
        submarine = [(i, h, a) for i, h, a in self.submarine_candidates()
                     if ID.match(i) and HEX.match(h) and ADDRESS.match(a)]
        reverse = [(i, a) for i, a in self.reverse_candidates()
                   if ID.match(i) and ADDRESS.match(a)]
        held = self.addresses_with_coins(
            {a for _, _, a in submarine} | {a for _, a in reverse})
        sub_due = [(i, h) for i, h, a in submarine if a not in held]
        rev_due = [i for i, a in reverse if a not in held]
        kept = (len(submarine) - len(sub_due), len(reverse) - len(rev_due))
        return sub_due, rev_due, kept

    def swaps(self):
        if not self.synced():
            raise RuntimeError(
                "the node is not synced (initial block download, or behind "
                "its headers): no swap is cleared")
        submarine, reverse, (sub_kept, rev_kept) = self.due()
        self.say(f"swaps past {SWAP_DAYS} days with user data: submarine "
                 f"{len(submarine)} to clear, {sub_kept} kept (a coin at the "
                 f"lockup address); reverse {len(reverse)} to clear, "
                 f"{rev_kept} kept (a coin at the lockup address)")
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
        # Proofs are needed while a copy of a SHA256-chain deposit of their
        # transaction is undecided: made (or there already), impossible,
        # the deposit gone or spent, or its request no longer looked at (a
        # year). An impossible copy is not tried again once its proofs are
        # gone (the worker's copies()).
        proofs = returned(self.m.psql(
            "UPDATE sha256_hints SET proofs = '[]'::jsonb "
            "WHERE proofs <> '[]'::jsonb "
            f"AND at < now() - interval '{HINT_DAYS} days' "
            "AND NOT EXISTS (SELECT 1 FROM sha256_deposits d "
            "JOIN sha256_requests r ON r.id = d.request_id "
            "WHERE d.request_id = sha256_hints.request_id "
            "AND d.txid = sha256_hints.txid AND d.chain = 'sha256' "
            "AND d.state NOT IN ('gone', 'spent') "
            "AND d.copy_state NOT IN ('already', 'copied', 'not_possible') "
            f"AND r.created_at > now() - interval '{POLL_DAYS} days') "
            "RETURNING 1", "donations"))
        self.say(f"donations: cleared {cleared} replay checks' inputs or "
                 f"addresses and {proofs} SHA256 proofs")

    def run(self):
        """Each part on its own: one that fails (an unsynced node, a
        schema change) does not stop the others. The failures are listed
        in self.errors; nothing is recorded as done if there are any."""
        self.errors = []
        for part in (self.swaps, self.referral, self.donations):
            try:
                part()
            except Exception as error:  # noqa: BLE001
                self.errors.append(f"{part.__name__}: {error}")
                self.say(f"FAILED {part.__name__}: {error}")
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
    if retention.errors:
        return 1
    # For the monitor: a job that stops running, or keeps failing, is
    # noticed (lfswap_monitor.py, check_retention)
    if args.deploy == DEPLOY:
        root = env.get("LFSWAP_ROOT", "/srv/lfswap")
        path = os.path.join(root, "retention", LAST_SUCCESS)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path + ".tmp", "w") as f:
            f.write(stamp + "\n")
        os.replace(path + ".tmp", path)
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
