"""Telemetry for sizing Lightning Fork Swap: balances, channels, swaps,
refusals, forwards, payments, peers, fees, the chain and the host, kept in a
local SQLite database so that later decisions (the hot wallet's size, inbound
against outbound, limits, fees, thresholds) rest on history.

Written by lfswap_monitor.py on every run (snapshots, and the events found
since the last run) and rolled up once a day by the same runs; read by
`lfswap_monitor.py --analysis`, the weekly report's "Sizing" block and
`--export`. Python 3 standard library only.

What it keeps is operational and aggregate: no IP addresses, no user
addresses or invoices. Swap rows carry the swap id, amounts and fees, as the
backend's own database has them. The database is not public: it says where
the liquidity is thin.

Retention: 5-minute snapshots 180 days; events and daily rollups kept.
"""

import csv
import datetime
import json
import os
import re
import sqlite3

SCHEMA_VERSION = 1
SNAPSHOT_DAYS = 180

# The .env keys whose changes are logged as changes of terms
TERMS_KEYS = (
    "SUBMARINE_FEE_PERCENT", "REVERSE_FEE_PERCENT", "MIN_SWAP_SAT",
    "MAX_SWAP_SAT", "MONITOR_MIN_WALLET_SAT", "MONITOR_MIN_OUTBOUND_SAT",
    "MONITOR_MIN_INBOUND_SAT",
)
TERMS_PREFIXES = ("REBALANCE_",)

SCHEMA = """
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE IF NOT EXISTS snapshot (
    ts INTEGER PRIMARY KEY,
    height INTEGER, header_lag INTEGER,
    hot_confirmed INTEGER, hot_unconfirmed INTEGER,
    lnd_confirmed INTEGER, lnd_unconfirmed INTEGER, lnd_reserve INTEGER,
    lnd_locked INTEGER,
    outbound INTEGER, inbound INTEGER, pending_local INTEGER,
    channels_active INTEGER, channels_total INTEGER, peers INTEGER,
    reverse_locked INTEGER, reverse_locked_count INTEGER,
    submarine_pending INTEGER,
    fee_2 REAL, fee_6 REAL, fee_24 REAL,
    mem_available INTEGER, pressure_io REAL, pressure_memory REAL,
    disk_used INTEGER, disk_total INTEGER
);
CREATE TABLE IF NOT EXISTS channel_snapshot (
    ts INTEGER, chan_id TEXT, peer TEXT, capacity INTEGER, local INTEGER,
    remote INTEGER, active INTEGER, htlcs INTEGER, htlc_amount INTEGER,
    sent INTEGER, received INTEGER, uptime INTEGER, lifetime INTEGER,
    base_fee_msat INTEGER, fee_ppm INTEGER,
    PRIMARY KEY (ts, chan_id)
);
CREATE INDEX IF NOT EXISTS channel_snapshot_chan ON channel_snapshot (chan_id, ts);
CREATE TABLE IF NOT EXISTS event (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ts INTEGER NOT NULL, kind TEXT NOT NULL, key TEXT NOT NULL,
    data TEXT NOT NULL,
    UNIQUE (kind, key)
);
CREATE INDEX IF NOT EXISTS event_kind_ts ON event (kind, ts);
CREATE TABLE IF NOT EXISTS swap (
    id TEXT PRIMARY KEY, type TEXT NOT NULL, status TEXT NOT NULL,
    created INTEGER, updated INTEGER,
    invoice_amount INTEGER, onchain_amount INTEGER, fee INTEGER,
    miner_fee INTEGER, routing_fee_msat INTEGER, failure TEXT,
    first_seen INTEGER, statuses TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS swap_created ON swap (created);
CREATE TABLE IF NOT EXISTS terms_change (
    ts INTEGER NOT NULL, key TEXT NOT NULL, old TEXT, new TEXT
);
CREATE TABLE IF NOT EXISTS daily (
    day TEXT NOT NULL, metric TEXT NOT NULL, value REAL,
    PRIMARY KEY (day, metric)
);
"""

SWAP_STATUSES_DONE = {
    "submarine": ("transaction.claimed", "invoice.paid",
                  "transaction.claim.pending"),
    "reverse": ("invoice.settled",),
}

# What the backend's refusals of swap creation say, by kind
REFUSALS = (
    ("over_maximum", re.compile(r"exceeds (the )?maximal")),
    ("under_minimum", re.compile(r"is less than minimal")),
    ("no_liquidity", re.compile(r"insufficient liquidity")),
    ("no_route", re.compile(r"no route|could not find route")),
    ("invoice_expiry", re.compile(r"expiry too short|invoice expired")),
    ("internal", re.compile(r"internal error")),
)
REQUEST_FAILED = re.compile(
    r"Request (POST|GET) (/v2/swap/(submarine|reverse))\b.* failed: (.*)$")
NGINX_LINE = re.compile(
    r'"(?P<method>[A-Z]+) (?P<path>\S+) HTTP/[0-9.]+" (?P<status>\d{3}) ')


def sat(btc):
    return round(float(btc) * 100_000_000)


def percentile(values, fraction):
    if not values:
        return None
    ordered = sorted(values)
    index = min(len(ordered) - 1, max(0, round(fraction * (len(ordered) - 1))))
    return ordered[index]


def classify_refusal(text):
    for kind, pattern in REFUSALS:
        if pattern.search(text):
            return kind
    return "other"


class Telemetry:
    def __init__(self, path):
        self.path = path
        directory = os.path.dirname(path)
        if directory:
            os.makedirs(directory, mode=0o700, exist_ok=True)
        self.db = sqlite3.connect(path)
        self.db.execute("PRAGMA journal_mode=WAL")
        self.db.executescript(SCHEMA)
        self.set_meta("schema_version", SCHEMA_VERSION)
        self.db.commit()
        try:
            os.chmod(path, 0o600)
        except OSError:
            pass

    def close(self):
        self.db.commit()
        self.db.close()

    # --- small helpers ---------------------------------------------------

    def meta(self, key, default=None):
        row = self.db.execute("SELECT value FROM meta WHERE key = ?",
                              (key,)).fetchone()
        return json.loads(row[0]) if row else default

    def set_meta(self, key, value):
        self.db.execute("INSERT OR REPLACE INTO meta VALUES (?, ?)",
                        (key, json.dumps(value)))

    def event(self, ts, kind, key, data):
        """Records an event once: (kind, key) is unique."""
        self.db.execute(
            "INSERT OR IGNORE INTO event (ts, kind, key, data) "
            "VALUES (?, ?, ?, ?)", (int(ts), kind, str(key), json.dumps(data)))

    # --- recording, every monitor run -----------------------------------

    def record(self, monitor, root):
        """One snapshot and the events since the last run. Each source is
        tried on its own: one that fails does not lose the others, and the
        names of the failed ones are returned."""
        ts = int(monitor.now.timestamp())
        failed = []
        snapshot = {"ts": ts}
        for name, step in (
            ("chain", lambda: self._chain(monitor, snapshot, ts)),
            ("wallets", lambda: self._wallets(monitor, snapshot)),
            ("channels", lambda: self._channels(monitor, snapshot, ts)),
            ("peers", lambda: self._peers(monitor, snapshot, ts)),
            ("swaps", lambda: self._swaps(monitor, snapshot, ts)),
            ("fees", lambda: self._fees(monitor, snapshot)),
            ("host", lambda: self._host(monitor, snapshot, root)),
            ("forwards", lambda: self._forwards(monitor, ts)),
            ("payments", lambda: self._payments(monitor, ts)),
            ("closed_channels", lambda: self._closed(monitor, ts)),
            ("transactions", lambda: self._transactions(monitor, ts)),
        ):
            try:
                step()
            except Exception as error:  # noqa: BLE001 (a source may be down)
                failed.append(f"{name}: {str(error)[:120]}")
        columns = ", ".join(snapshot)
        marks = ", ".join("?" for _ in snapshot)
        self.db.execute(
            f"INSERT OR REPLACE INTO snapshot ({columns}) VALUES ({marks})",
            tuple(snapshot.values()))
        self.db.commit()
        return failed

    def _chain(self, m, snap, ts):
        info = json.loads(m.knots("getblockchaininfo"))
        snap["height"] = info["blocks"]
        snap["header_lag"] = info["headers"] - info["blocks"]
        # Lost branches, recorded once each: their depth sizes the
        # confirmations a lockup needs
        for tip in json.loads(m.knots("getchaintips")):
            # Near the tip only: older branches were seen long ago, and their
            # time would be wrong
            if tip["status"] in ("valid-fork", "valid-headers") \
                    and tip["branchlen"] >= 1 \
                    and info["blocks"] - tip["height"] < 1000:
                self.event(ts, "reorg", tip["hash"], {
                    "height": tip["height"], "depth": tip["branchlen"],
                    "status": tip["status"]})

    def _wallets(self, m, snap):
        mine = json.loads(m.knots("-rpcwallet=boltz", "getbalances"))["mine"]
        snap["hot_confirmed"] = sat(mine["trusted"])
        snap["hot_unconfirmed"] = sat(mine.get("untrusted_pending", 0))
        lnd = json.loads(m.lncli("walletbalance"))
        snap["lnd_confirmed"] = int(lnd.get("confirmed_balance", 0))
        snap["lnd_unconfirmed"] = int(lnd.get("unconfirmed_balance", 0))
        snap["lnd_reserve"] = int(lnd.get("reserved_balance_anchor_chan", 0))
        snap["lnd_locked"] = int(lnd.get("locked_balance", 0))

    def _channels(self, m, snap, ts):
        balance = json.loads(m.lncli("channelbalance"))
        snap["outbound"] = int(balance.get("local_balance", {}).get("sat", 0))
        snap["inbound"] = int(balance.get("remote_balance", {}).get("sat", 0))
        snap["pending_local"] = int(
            balance.get("pending_open_local_balance", {}).get("sat", 0))
        policies = {}
        try:
            for fee in json.loads(m.lncli("feereport")).get(
                    "channel_fees", []):
                policies[str(fee.get("chan_id"))] = (
                    int(fee.get("base_fee_msat", 0)),
                    int(fee.get("fee_per_mil", 0)))
        except Exception:  # noqa: BLE001 (policies are a nice-to-have)
            pass
        channels = json.loads(m.lncli("listchannels"))["channels"]
        snap["channels_total"] = len(channels)
        snap["channels_active"] = sum(1 for c in channels if c.get("active"))
        # The first run only learns what exists: those are not openings
        baseline = self.meta("channels") is None
        known = set(self.meta("channels", []))
        seen = []
        for c in channels:
            chan_id = str(c["chan_id"])
            seen.append(chan_id)
            htlcs = c.get("pending_htlcs", [])
            base, ppm = policies.get(chan_id, (None, None))
            self.db.execute(
                "INSERT OR REPLACE INTO channel_snapshot VALUES "
                "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (ts, chan_id, c.get("remote_pubkey"),
                 int(c.get("capacity", 0)), int(c.get("local_balance", 0)),
                 int(c.get("remote_balance", 0)), int(bool(c.get("active"))),
                 len(htlcs), sum(int(h.get("amount", 0)) for h in htlcs),
                 int(c.get("total_satoshis_sent", 0)),
                 int(c.get("total_satoshis_received", 0)),
                 int(c.get("uptime", 0)), int(c.get("lifetime", 0)),
                 base, ppm))
            if chan_id not in known and not baseline:
                self.event(ts, "channel_open", chan_id, {
                    "peer": c.get("remote_pubkey"),
                    "capacity": int(c.get("capacity", 0)),
                    "initiator": bool(c.get("initiator")),
                    "alias": c.get("peer_alias")})
            # A change of our routing policy on a channel
            if base is not None:
                last = self.meta(f"policy:{chan_id}")
                if last != [base, ppm]:
                    if last is not None:
                        self.db.execute(
                            "INSERT INTO terms_change VALUES (?, ?, ?, ?)",
                            (ts, f"policy:{chan_id}", json.dumps(last),
                             json.dumps([base, ppm])))
                    self.set_meta(f"policy:{chan_id}", [base, ppm])
        self.set_meta("channels", sorted(set(seen) | known))

    def _peers(self, m, snap, ts):
        peers = {p["pub_key"] for p in
                 json.loads(m.lncli("listpeers")).get("peers", [])}
        snap["peers"] = len(peers)
        if self.meta("peers") is None:
            # The first run only learns who is connected
            self.set_meta("peers", sorted(peers))
            return
        last = set(self.meta("peers", []))
        for pub in sorted(peers - last):
            self.event(ts, "peer_online", f"{pub}:{ts}", {"peer": pub})
        for pub in sorted(last - peers):
            self.event(ts, "peer_offline", f"{pub}:{ts}", {"peer": pub})
        self.set_meta("peers", sorted(peers))

    def _swaps(self, m, snap, ts):
        # Locked by reverse swaps the user has not claimed yet, and paying
        # submarine swaps: how much is committed at this moment
        locked = m.psql(
            'SELECT count(*), coalesce(sum("onchainAmount"), 0) FROM '
            '"reverseSwaps" WHERE status IN (\'transaction.mempool\', '
            "'transaction.confirmed')")[0]
        snap["reverse_locked_count"] = int(locked[0])
        snap["reverse_locked"] = int(locked[1])
        snap["submarine_pending"] = int(m.psql(
            "SELECT count(*) FROM swaps WHERE status IN ('invoice.set', "
            "'transaction.mempool', 'transaction.confirmed', "
            "'invoice.pending')")[0][0])

        since = self.meta("swaps_since", "1970-01-01 00:00:00+00")
        newest = since
        for kind, table, invoice_col, extra in (
            ("submarine", "swaps", '"invoiceAmount"',
             'coalesce("routingFee", 0)'),
            ("reverse", '"reverseSwaps"', '"invoiceAmount"', "0"),
        ):
            rows = m.psql(
                f'SELECT id, status, extract(epoch FROM "createdAt")::bigint, '
                f'extract(epoch FROM "updatedAt")::bigint, '
                f'coalesce({invoice_col}, 0), coalesce("onchainAmount", 0), '
                f'coalesce(fee, 0), coalesce("minerFee", 0), {extra}, '
                f'replace(coalesce("failureReason", \'\'), \'|\', \'/\'), '
                f'"updatedAt" FROM {table} '
                f"WHERE \"updatedAt\" > '{since}' ORDER BY \"updatedAt\" "
                "LIMIT 5000")
            for row in rows:
                (swap_id, status, created, updated, invoice, onchain, fee,
                 miner, routing, failure, updated_raw) = row
                newest = max(newest, updated_raw)
                old = self.db.execute(
                    "SELECT statuses, first_seen FROM swap WHERE id = ?",
                    (swap_id,)).fetchone()
                statuses = json.loads(old[0]) if old else {}
                # When each status was first seen (the backend keeps only
                # the last one): the run's time, so within 5 minutes
                statuses.setdefault(status, ts)
                self.db.execute(
                    "INSERT OR REPLACE INTO swap VALUES "
                    "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                    (swap_id, kind, status, int(created), int(updated),
                     int(invoice), int(onchain), int(fee), int(miner),
                     int(routing), failure or None,
                     old[1] if old else ts, json.dumps(statuses)))
        self.set_meta("swaps_since", newest)

    def _fees(self, m, snap):
        for target in (2, 6, 24):
            try:
                rate = json.loads(m.knots("estimatesmartfee", str(target)))
                if "feerate" in rate:
                    snap[f"fee_{target}"] = round(rate["feerate"] * 1e5, 2)
            except Exception:  # noqa: BLE001 (no estimate yet)
                continue

    def _host(self, m, snap, root):
        usage = m.runner.disk_usage("/")
        snap["disk_used"] = int(usage.used)
        snap["disk_total"] = int(usage.total)
        try:
            text = m.runner.read("/proc/meminfo")
            found = re.search(r"MemAvailable:\s+(\d+) kB", text)
            if found:
                snap["mem_available"] = int(found.group(1)) * 1024
        except OSError:
            pass
        for kind in ("io", "memory"):
            try:
                text = m.runner.read(f"/proc/pressure/{kind}")
            except OSError:
                continue
            found = re.search(r"full avg10=\S+ avg60=\S+ avg300=(\S+)", text)
            if found:
                snap[f"pressure_{kind}"] = float(found.group(1))

    def _forwards(self, m, ts):
        start = self.meta("forwards_since", int(ts) - 86400)
        out = json.loads(m.lncli(
            "fwdinghistory", "--start_time", str(int(start)),
            "--max_events", "50000"))
        newest = start
        for f in out.get("forwarding_events", []):
            stamp = int(f.get("timestamp_ns", 0)) // 1_000_000_000 or int(
                f.get("timestamp", 0))
            key = (f"{f.get('timestamp_ns') or f.get('timestamp')}:"
                   f"{f.get('chan_id_in')}:{f.get('chan_id_out')}")
            self.event(stamp, "forward", key, {
                "in": str(f.get("chan_id_in")), "out": str(f.get("chan_id_out")),
                "amt_in": int(f.get("amt_in", 0)),
                "amt_out": int(f.get("amt_out", 0)),
                "fee_msat": int(f.get("fee_msat", 0))})
            newest = max(newest, stamp)
        self.set_meta("forwards_since", int(newest))

    def _payments(self, m, ts):
        offset = self.meta("payments_index", 0)
        out = json.loads(m.lncli(
            "listpayments", "--include_incomplete", "--paginate_forwards",
            "--index_offset", str(int(offset)), "--max_payments", "500"))
        newest = offset
        for p in out.get("payments", []):
            index = int(p.get("payment_index", 0))
            if p.get("status") == "IN_FLIGHT":
                # Recorded once it has an outcome; read again next run
                break
            attempts = []
            for h in p.get("htlcs", []):
                hops = h.get("route", {}).get("hops", [])
                attempts.append({
                    "status": h.get("status"),
                    "first_hop": str(hops[0]["chan_id"]) if hops else None,
                    "hops": len(hops),
                    "failure": (h.get("failure") or {}).get("code"),
                    "failure_index": (h.get("failure") or {}).get(
                        "failure_source_index"),
                })
            self.event(
                int(p.get("creation_time_ns", 0)) // 1_000_000_000 or ts,
                "payment", p.get("payment_hash"), {
                    "status": p.get("status"),
                    "value_sat": int(p.get("value_sat", 0)),
                    "fee_sat": int(p.get("fee_sat", 0)),
                    "failure_reason": p.get("failure_reason"),
                    "attempts": attempts})
            newest = max(newest, index)
        self.set_meta("payments_index", int(newest))

    def _closed(self, m, ts):
        for c in json.loads(m.lncli("closedchannels")).get("channels", []):
            self.event(ts, "channel_close", str(c.get("chan_id")), {
                "peer": c.get("remote_pubkey"),
                "capacity": int(c.get("capacity", 0)),
                "type": c.get("close_type"),
                "settled": int(c.get("settled_balance", 0)),
                "height": c.get("close_height")})

    def _transactions(self, m, ts):
        # Our own on-chain transactions: the fee they paid and how long they
        # took to confirm, for the fee floor and ceiling
        since = self.meta("knots_since_block")
        args = ["-rpcwallet=boltz", "listsinceblock"]
        if since:
            args.append(since)
        out = json.loads(m.knots(*args))
        for t in out.get("transactions", []):
            if t.get("category") != "send" or t.get("confirmations", 0) < 1:
                continue
            received = int(t.get("timereceived", t.get("time", 0)))
            self.event(int(t.get("blocktime", ts)), "tx", t["txid"], {
                "wallet": "hot", "fee_sat": -sat(t.get("fee", 0)),
                "amount_sat": -sat(t.get("amount", 0)),
                "confirm_seconds": int(t.get("blocktime", received)) - received,
            })
        # Next time from a few blocks back, so that confirmations of
        # transactions seen unconfirmed now are not missed
        info = json.loads(m.knots("getblockchaininfo"))
        back = max(0, info["blocks"] - 6)
        self.set_meta("knots_since_block", m.knots(
            "getblockhash", str(back)).strip())

        # lnd's: channel opens and closes, sweeps, transfers to the hot wallet
        start = self.meta("lnd_since_height", 0)
        out = json.loads(m.lncli("listchaintxns", "--start_height",
                                 str(int(start))))
        times = {}
        for tx in out.get("transactions", []):
            fee = int(tx.get("total_fees", 0))
            if int(tx.get("num_confirmations", 0)) < 1 or fee <= 0:
                continue
            block = tx.get("block_hash")
            if block not in times:
                times[block] = int(json.loads(m.knots(
                    "getblockheader", block))["time"])
            seen = int(tx.get("time_stamp", times[block]))
            self.event(times[block], "tx", tx["tx_hash"], {
                "wallet": "lnd", "fee_sat": fee,
                "amount_sat": -int(tx.get("amount", 0)),
                "label": tx.get("label") or None,
                "confirm_seconds": max(0, times[block] - seen)})
        self.set_meta("lnd_since_height", back)

    # --- logs: refusals and creations ------------------------------------

    def record_logs(self, monitor, since, nginx_log):
        """Refusals of swap creation in the backend's log since `since`,
        and creations by status from nginx's access log since the last
        offset. Neither keeps addresses: only counts per hour and kind."""
        ts = int(monitor.now.timestamp())
        out = monitor.runner.compose(
            "logs", "--no-color", "--no-log-prefix", "--since", since,
            "boltz", timeout=120)
        for line in out.splitlines():
            found = REQUEST_FAILED.search(line)
            if not found or found.group(1) != "POST":
                continue
            kind = classify_refusal(found.group(4))
            self.count(ts, f"refusal:{found.group(3)}:{kind}")
        self._nginx(monitor, ts, nginx_log)
        self.db.commit()

    def count(self, ts, name, by=1):
        hour = ts - ts % 3600
        key = f"{name}:{hour}"
        row = self.db.execute(
            "SELECT data FROM event WHERE kind = 'count' AND key = ?",
            (key,)).fetchone()
        value = (json.loads(row[0])["n"] if row else 0) + by
        self.db.execute(
            "INSERT OR REPLACE INTO event (ts, kind, key, data) VALUES "
            "(?, 'count', ?, ?)", (hour, key, json.dumps(
                {"name": name, "n": value})))

    def _nginx(self, monitor, ts, path):
        try:
            data = monitor.runner.read_bytes(path)
        except OSError:
            return
        offset = self.meta("nginx_offset", 0)
        # A rotated log is another file: its first bytes differ (or it is
        # shorter than where we were); read it from the beginning
        head = data[:256].hex()
        if offset > len(data) or head[:len(self.meta("nginx_head", ""))] \
                != self.meta("nginx_head", ""):
            offset = 0
        self.set_meta("nginx_head", head)
        for raw in data[offset:].decode(errors="replace").splitlines():
            found = NGINX_LINE.search(raw)
            if not found or found["method"] != "POST":
                continue
            path_only = found["path"].split("?", 1)[0]
            if path_only in ("/v2/swap/submarine", "/v2/swap/reverse"):
                self.count(ts, f"create:{path_only.rsplit('/', 1)[1]}:"
                               f"{found['status']}")
        self.set_meta("nginx_offset", len(data))

    # --- terms -----------------------------------------------------------

    def record_terms(self, env, ts):
        """Logs each change of the tracked .env keys (fees, limits, floors,
        rebalancer bands) since the last time, with the old value."""
        current = {k: env.get(k, "") for k in env
                   if k in TERMS_KEYS or k.startswith(TERMS_PREFIXES)}
        for k in TERMS_KEYS:
            current.setdefault(k, "")
        last = self.meta("terms")
        if last is not None:
            for key in sorted(set(last) | set(current)):
                if last.get(key, "") != current.get(key, ""):
                    self.db.execute(
                        "INSERT INTO terms_change VALUES (?, ?, ?, ?)",
                        (int(ts), key, last.get(key, ""),
                         current.get(key, "")))
        self.set_meta("terms", current)
        self.db.commit()

    # --- daily rollups and retention ---------------------------------------

    def rollup(self, now):
        """Rolls up every whole day not rolled up yet (UTC), then prunes old
        snapshots. Cheap enough for every run; does work once a day."""
        today = now.date()
        last = self.meta("rolled_up_to")
        first = self.db.execute("SELECT min(ts) FROM snapshot").fetchone()[0]
        if first is None:
            return
        start = (datetime.date.fromisoformat(last) + datetime.timedelta(1)
                 if last else datetime.datetime.fromtimestamp(
                     first, datetime.timezone.utc).date())
        # The last two days again: a swap created before midnight may finish
        # after the day was first rolled up
        first_day = datetime.datetime.fromtimestamp(
            first, datetime.timezone.utc).date()
        start = max(first_day, min(start, today - datetime.timedelta(2)))
        day = start
        while day < today:
            self._rollup_day(day)
            self.set_meta("rolled_up_to", day.isoformat())
            day += datetime.timedelta(1)
        cutoff = int(now.timestamp()) - SNAPSHOT_DAYS * 86400
        self.db.execute("DELETE FROM snapshot WHERE ts < ?", (cutoff,))
        self.db.execute("DELETE FROM channel_snapshot WHERE ts < ?", (cutoff,))
        self.db.commit()

    def _rollup_day(self, day):
        begin = int(datetime.datetime.combine(
            day, datetime.time(), datetime.timezone.utc).timestamp())
        end = begin + 86400
        metrics = {}

        def put(name, value):
            if value is not None:
                metrics[name] = value

        rows = self.db.execute(
            "SELECT hot_confirmed, outbound, inbound, reverse_locked, "
            "lnd_confirmed, fee_2, mem_available FROM snapshot "
            "WHERE ts >= ? AND ts < ?", (begin, end)).fetchall()
        for index, name in enumerate(("hot", "outbound", "inbound",
                                      "reverse_locked", "lnd_onchain", "fee_2",
                                      "mem_available")):
            values = [r[index] for r in rows if r[index] is not None]
            put(f"{name}:min", min(values) if values else None)
            put(f"{name}:p50", percentile(values, 0.5))
            put(f"{name}:p95", percentile(values, 0.95))
            put(f"{name}:max", max(values) if values else None)
        put("snapshots", len(rows))

        for kind in ("submarine", "reverse"):
            done = SWAP_STATUSES_DONE[kind]
            marks = ", ".join("?" for _ in done)
            count, volume, fees, miner = self.db.execute(
                "SELECT count(*), coalesce(sum(invoice_amount), 0), "
                "coalesce(sum(fee), 0), coalesce(sum(miner_fee), 0) FROM swap "
                f"WHERE type = ? AND status IN ({marks}) AND created >= ? "
                "AND created < ?", (kind, *done, begin, end)).fetchone()
            put(f"{kind}:count", count)
            put(f"{kind}:volume", volume)
            put(f"{kind}:fees", fees)
            put(f"{kind}:miner_fees", miner)
            failed = self.db.execute(
                "SELECT count(*) FROM swap WHERE type = ? AND created >= ? "
                "AND created < ? AND (status LIKE '%fail%' OR status LIKE "
                "'%expired%' OR status LIKE '%refunded%')",
                (kind, begin, end)).fetchone()[0]
            put(f"{kind}:failed", failed)
        put("net_flow", metrics.get("submarine:volume", 0)
            - metrics.get("reverse:volume", 0))

        for name, n in self.db.execute(
                "SELECT json_extract(data, '$.name'), "
                "sum(json_extract(data, '$.n')) FROM event WHERE kind = "
                "'count' AND ts >= ? AND ts < ? GROUP BY 1", (begin, end)):
            put(name, n)

        forwards = self.db.execute(
            "SELECT count(*), coalesce(sum(json_extract(data, '$.fee_msat')), "
            "0), coalesce(sum(json_extract(data, '$.amt_out')), 0) FROM event "
            "WHERE kind = 'forward' AND ts >= ? AND ts < ?",
            (begin, end)).fetchone()
        put("forwards:count", forwards[0])
        put("forwards:fees_msat", forwards[1])
        put("forwards:volume", forwards[2])

        confirm = [json.loads(d)["confirm_seconds"] for (d,) in self.db.execute(
            "SELECT data FROM event WHERE kind = 'tx' AND ts >= ? AND ts < ?",
            (begin, end))]
        put("tx:confirm_seconds:p50", percentile(confirm, 0.5))
        put("tx:confirm_seconds:p95", percentile(confirm, 0.95))

        # Per channel: how far its local balance moved, and its uptime
        for chan_id, first_local, last_local, up, life in self.db.execute(
                "SELECT chan_id, "
                "(SELECT local FROM channel_snapshot c2 WHERE c2.chan_id = "
                "c.chan_id AND ts >= ? AND ts < ? ORDER BY ts LIMIT 1), "
                "(SELECT local FROM channel_snapshot c2 WHERE c2.chan_id = "
                "c.chan_id AND ts >= ? AND ts < ? ORDER BY ts DESC LIMIT 1), "
                "max(uptime), max(lifetime) FROM channel_snapshot c "
                "WHERE ts >= ? AND ts < ? GROUP BY chan_id",
                (begin, end, begin, end, begin, end)).fetchall():
            put(f"channel:{chan_id}:local_change", last_local - first_local)
            if life:
                put(f"channel:{chan_id}:uptime", round(up / life, 4))

        self.db.executemany(
            "INSERT OR REPLACE INTO daily VALUES (?, ?, ?)",
            [(day.isoformat(), k, v) for k, v in metrics.items()])

    # --- reading ------------------------------------------------------------

    def daily(self, metric, days, now):
        start = (now.date() - datetime.timedelta(days)).isoformat()
        return [v for (v,) in self.db.execute(
            "SELECT value FROM daily WHERE metric = ? AND day >= ? "
            "ORDER BY day", (metric, start))]

    def export(self, table, out):
        if table not in ("snapshot", "channel_snapshot", "event", "swap",
                         "terms_change", "daily"):
            raise ValueError(f"no table {table}")
        cursor = self.db.execute(f"SELECT * FROM {table}")
        writer = csv.writer(out)
        writer.writerow([d[0] for d in cursor.description])
        writer.writerows(cursor)


# --- analysis ---------------------------------------------------------------

def fmt(value):
    return f"{int(value):,} sat"


def analysis(t, env, now, days=30):
    """Plain findings and recommendations from the last `days` days."""
    lines = []
    start = int(now.timestamp()) - days * 86400
    snaps = t.db.execute(
        "SELECT hot_confirmed, outbound, inbound, reverse_locked "
        "FROM snapshot WHERE ts >= ?", (start,)).fetchall()
    if len(snaps) < 12:
        return [f"Not enough history yet ({len(snaps)} snapshots in "
                f"{days} days)."]

    hot = [s[0] for s in snaps if s[0] is not None]
    locked = [s[3] for s in snaps if s[3] is not None]
    inbound = [s[2] for s in snaps if s[2] is not None]
    outbound = [s[1] for s in snaps if s[1] is not None]
    max_swap = int(float(env.get("MAX_SWAP_SAT") or 0))

    lines.append(f"Over the last {days} days ({len(snaps)} snapshots):")
    if locked:
        need = max(percentile(locked, 0.95), max_swap)
        lines.append(
            f"  reverse lockups held at once: p95 {fmt(percentile(locked, 0.95))},"
            f" peak {fmt(max(locked))}")
        if hot:
            lines.append(
                f"  hot wallet: median {fmt(percentile(hot, 0.5))}, lowest "
                f"{fmt(min(hot))}; it held {percentile(hot, 0.5) / need:.1f} "
                "times what reverse swaps needed at once (p95, at least one "
                "maximum swap)")
            target = float(env.get("REBALANCE_HOT_TARGET") or 0)
            if target:
                lines.append(
                    f"  the rebalancer's hot target of {fmt(target)} is "
                    f"{target / need:.1f} times that need")
    if inbound and outbound:
        lines.append(
            f"  inbound: median {fmt(percentile(inbound, 0.5))}, lowest "
            f"{fmt(min(inbound))}; outbound: median "
            f"{fmt(percentile(outbound, 0.5))}, lowest {fmt(min(outbound))}")

    sub = sum(t.daily("submarine:volume", days, now))
    rev = sum(t.daily("reverse:volume", days, now))
    if sub or rev:
        net = sub - rev
        per_day = net / days
        lines.append(
            f"  volume: chain to Lightning {fmt(sub)}, Lightning to chain "
            f"{fmt(rev)}; net {fmt(net)} toward Lightning" if net >= 0 else
            f"  volume: chain to Lightning {fmt(sub)}, Lightning to chain "
            f"{fmt(rev)}; net {fmt(-net)} toward the chain")
        if per_day > 0 and outbound:
            lines.append(
                f"  at that rate outbound ({fmt(outbound[-1])}) lasts about "
                f"{outbound[-1] / per_day:.0f} days")
        elif per_day < 0 and inbound and hot:
            side = min(inbound[-1], hot[-1])
            lines.append(
                f"  at that rate reverse capacity ({fmt(side)}) lasts about "
                f"{side / -per_day:.0f} days")

    refusals = {}
    for (name, n) in t.db.execute(
            "SELECT json_extract(data, '$.name'), sum(json_extract(data, "
            "'$.n')) FROM event WHERE kind = 'count' AND ts >= ? AND "
            "json_extract(data, '$.name') LIKE 'refusal:%' GROUP BY 1",
            (start,)):
        refusals[name.split(":", 1)[1]] = int(n)
    if refusals:
        lines.append("  refused creations: " + ", ".join(
            f"{k.replace(':', ' ')} {v}" for k, v in sorted(refusals.items())))
        if refusals.get("reverse:no_liquidity") or refusals.get(
                "submarine:no_route"):
            lines.append("  -> demand went unserved for lack of liquidity: "
                         "see the sides above")
        if refusals.get("reverse:over_maximum") or refusals.get(
                "submarine:over_maximum"):
            lines.append("  -> people asked for more than the maximum swap")

    # Channels: first hops of our payments and idle ones
    hops = {}
    for (data,) in t.db.execute(
            "SELECT data FROM event WHERE kind = 'payment' AND ts >= ?",
            (start,)):
        for attempt in json.loads(data).get("attempts", []):
            if attempt.get("status") == "SUCCEEDED" and attempt.get(
                    "first_hop"):
                hops[attempt["first_hop"]] = hops.get(
                    attempt["first_hop"], 0) + 1
    channels = [c for (c,) in t.db.execute(
        "SELECT DISTINCT chan_id FROM channel_snapshot WHERE ts >= ?",
        (start,))]
    if channels:
        total = sum(hops.values())
        idle = [c for c in channels if not hops.get(c)]
        busy = sorted(hops.items(), key=lambda kv: -kv[1])[:3]
        if total:
            lines.append("  first hops of our payments: " + ", ".join(
                f"{c} {n * 100 // total} %" for c, n in busy))
        if idle and total:
            lines.append(f"  {len(idle)} of {len(channels)} channels carried "
                         "none of our payments: " + ", ".join(idle[:5]))

    reorgs = [json.loads(d)["depth"] for (d,) in t.db.execute(
        "SELECT data FROM event WHERE kind = 'reorg' AND ts >= ?", (start,))]
    if reorgs:
        lines.append(f"  reorganisations seen: {len(reorgs)}, deepest "
                     f"{max(reorgs)} blocks")

    changes = t.db.execute(
        "SELECT ts, key, old, new FROM terms_change WHERE ts >= ? "
        "ORDER BY ts", (start,)).fetchall()
    if changes:
        lines.append("  changes of terms in this period:")
        for ts, key, old, new in changes[-8:]:
            when = datetime.datetime.fromtimestamp(
                ts, datetime.timezone.utc).strftime("%Y-%m-%d")
            lines.append(f"    {when} {key}: {old or '(unset)'} -> "
                         f"{new or '(unset)'}")
    return lines


def sizing_block(t, env, now):
    """The weekly report's short version."""
    found = analysis(t, env, now, days=7)
    return ["Sizing (last 7 days)"] + [
        "  " + line.strip() for line in found[1:] or found]
