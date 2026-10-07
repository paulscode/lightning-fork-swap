#!/usr/bin/env python3
"""Watches Lightning Fork Swap and sends an alert when something needs the
operator. Run every few minutes by cron (/etc/cron.d/lfswap-monitor); safe to
run by hand. Python 3 standard library only.

Configuration, from deploy/.env (all optional):

  ALERT_WEBHOOK_URL       where alerts go; without it they are only printed
  ALERT_WEBHOOK_KIND      discord, slack (also Mattermost), telegram, ntfy or
                          json (default json: {"title", "text", "alerts"})
  ALERT_TELEGRAM_CHAT_ID  the chat, for telegram (the URL carries the token:
                          https://api.telegram.org/bot<token>/sendMessage)
  MONITOR_MIN_WALLET_SAT  alert when the on-chain wallet holds less
  MONITOR_MIN_PEERS       alert when lnd has fewer peers (default 1)
  MONITOR_MAX_TIP_AGE_MIN alert when the newest block is older (default 120)
  MONITOR_REPEAT_HOURS    repeat an alert that is still true (default 6)
  MONITOR_HEARTBEAT_HOURS a message that the monitor runs (default 24, 0 off)

State (what was sent, where the logs were read up to) is kept in
$LFSWAP_ROOT/monitor/state.json.

  lfswap_monitor.py           check once, send what is new
  lfswap_monitor.py --test    send a test alert and exit
  lfswap_monitor.py --dry-run check once, print, send nothing, keep no state
"""

import datetime
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import urllib.request

DEPLOY = os.environ.get("LFSWAP_DEPLOY", "/opt/lfswap/deploy")

SERVICES = ["knots", "shim", "tor", "lnd", "postgres", "boltz"]

# Lines in the backend's log that need a person. Each is reported once, when
# it first appears.
LOG_PATTERNS = [
    # A lockup may be on chain while the swap is not recorded as locked up
    r"may have been broadcast before this error",
    r"Not locking up .* could not check whether",
    r"was already locked up in .* recording it",
    # Reverse swaps: an HTLC that would not outlast the lockup, a late lockup
    r"Cancelling hold invoice of Reverse Swap .* its HTLC expires",
    r"Not acting on confirmed server lockup transaction .* because it is",
    r"Could not settle invoice of",
    # Submarine swaps
    r"was paid but not claimed",
    r"Claim of .* failed",
    r"Batch claim .* failed",
    r"Abandoning Swap",
    r"may still have it in flight",
    r"Prevented .* from paying an invoice because it already signed a refund",
    r"Prevented .* lockup",
    r"Not rolling back Swap",
    r"refused a coinbase|coinbase lockups are not accepted",
    # The chain and the node
    r"Stopping:",
    r"chain identity",
    r"lagged behind",
    r"has no subscriber",
    r"Could not initialize Boltz",
]

# The same for the txindex shim
SHIM_LOG_PATTERNS = [
    r"index has not synced",
]

ANSI = re.compile(r"\x1b\[[0-9;]*m")


def load_env(path):
    env = {}
    try:
        with open(path) as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith("#") or "=" not in line:
                    continue
                key, value = line.split("=", 1)
                env[key.strip()] = value.strip().strip("'\"")
    except FileNotFoundError:
        pass
    return env


def number(env, key, default):
    value = env.get(key, "")
    if value == "":
        return default
    try:
        return float(value)
    except ValueError:
        return default


class Alert:
    """One thing that is wrong. `key` identifies it from run to run."""

    def __init__(self, key, text, event=False):
        self.key = key
        self.text = text
        # An event is reported once; a condition until it clears
        self.event = event

    def __repr__(self):
        return f"Alert({self.key!r}, {self.text!r})"


class Runner:
    """Runs commands; replaced by a fake in the tests."""

    def __init__(self, deploy):
        self.deploy = deploy

    def run(self, args, timeout=60):
        result = subprocess.run(
            args,
            cwd=self.deploy,
            stdin=subprocess.DEVNULL,
            capture_output=True,
            text=True,
            timeout=timeout,
        )
        if result.returncode != 0:
            raise RuntimeError(
                f"{' '.join(args[:6])}: exit {result.returncode}: "
                f"{(result.stderr or result.stdout).strip()[:300]}"
            )
        return result.stdout

    def compose(self, *args, timeout=60):
        return self.run(["docker", "compose", *args], timeout=timeout)

    def read(self, path):
        with open(path) as f:
            return f.read()

    def read_bytes(self, path):
        with open(path, "rb") as f:
            return f.read()

    def mtime(self, path):
        return os.stat(path).st_mtime

    def disk_usage(self, path):
        return shutil.disk_usage(path)


class Monitor:
    def __init__(self, runner, env, now, network="mainnet"):
        self.runner = runner
        self.env = env
        self.now = now
        self.network = network
        self.alerts = []
        self.height = None

    def add(self, key, text, event=False):
        self.alerts.append(Alert(key, text, event))

    def knots(self, *args):
        return self.runner.compose(
            "exec", "-T", "knots", "bitcoin-cli", "-datadir=/data", *args
        )

    def lncli(self, *args):
        return self.runner.compose(
            "exec", "-T", "lnd", "lncli", f"--network={self.network}", *args
        )

    def psql(self, query):
        out = self.runner.compose(
            "exec", "-T", "postgres", "psql", "-U", "boltz", "-d", "boltz",
            "-At", "-F", "|", "-c", query,
        )
        return [line.split("|") for line in out.splitlines() if line]

    # --- checks -----------------------------------------------------------

    def check_containers(self):
        out = self.runner.compose("ps", "--all", "--format", "json")
        # One object per line, or one array, depending on the compose version
        rows = []
        for line in out.splitlines():
            line = line.strip()
            if not line:
                continue
            parsed = json.loads(line)
            rows.extend(parsed if isinstance(parsed, list) else [parsed])
        state = {row.get("Service"): row.get("State") for row in rows}
        for service in SERVICES:
            if state.get(service) != "running":
                self.add(
                    f"container:{service}",
                    f"Container {service} is {state.get(service) or 'missing'}",
                )

    def check_chain(self):
        info = json.loads(self.knots("getblockchaininfo"))
        self.height = info["blocks"]
        if info["headers"] - info["blocks"] > 2:
            self.add(
                "chain:behind",
                f"Knots is {info['headers'] - info['blocks']} blocks behind "
                f"the headers it knows (at {info['blocks']})",
            )
        header = json.loads(self.knots("getblockheader", info["bestblockhash"]))
        age_min = (self.now.timestamp() - header["time"]) / 60
        max_age = number(self.env, "MONITOR_MAX_TIP_AGE_MIN", 120)
        if age_min > max_age:
            self.add(
                "chain:stale",
                f"No new block for {age_min:.0f} minutes (tip {info['blocks']})",
            )

        for tip in json.loads(self.knots("getchaintips")):
            # A branch of two or more blocks that lost: a reorganisation as
            # deep as the confirmations a lockup needs comes close to them
            if tip["status"] in ("valid-fork", "valid-headers") and tip[
                "branchlen"
            ] >= 2 and self.height - tip["height"] < 1000:
                self.add(
                    f"chain:fork:{tip['hash']}",
                    f"A {tip['branchlen']}-block branch at height "
                    f"{tip['height']} lost to the active chain "
                    f"(status {tip['status']}): check the swaps of those blocks",
                    event=True,
                )

    def check_wallet(self):
        floor = number(self.env, "MONITOR_MIN_WALLET_SAT", None)
        balances = json.loads(self.knots("-rpcwallet=boltz", "getbalances"))
        trusted = round(balances["mine"]["trusted"] * 100_000_000)
        if floor is not None and trusted < floor:
            self.add(
                "wallet:low",
                f"On-chain wallet holds {trusted:,} sat, below {int(floor):,}",
            )

    def check_lnd(self):
        info = json.loads(self.lncli("getinfo"))
        if not info.get("synced_to_chain"):
            self.add("lnd:unsynced", "lnd is not synced to the chain")
        min_peers = number(self.env, "MONITOR_MIN_PEERS", 1)
        if info.get("num_peers", 0) < min_peers:
            self.add(
                "lnd:peers",
                f"lnd has {info.get('num_peers', 0)} peers "
                f"(alert below {int(min_peers)})",
            )
        height = info.get("block_height") or self.height
        channels = json.loads(self.lncli("listchannels"))["channels"]
        for channel in channels:
            for htlc in channel.get("pending_htlcs", []):
                left = int(htlc["expiration_height"]) - int(height)
                # lnd cancels a held invoice 18 blocks before; an HTLC this
                # close was not cancelled, or is an outgoing one stuck
                if left <= 24:
                    self.add(
                        f"lnd:htlc:{htlc['hash_lock']}",
                        f"{'Incoming' if htlc['incoming'] else 'Outgoing'} "
                        f"HTLC {htlc['hash_lock'][:16]}... on channel "
                        f"{channel['chan_id']} expires in {left} blocks",
                    )

    def check_swaps(self):
        height = self.height
        if height is not None:
            for swap_id, status, timeout in self.psql(
                'SELECT id, status, "timeoutBlockHeight" FROM "reverseSwaps" '
                "WHERE status IN ('transaction.mempool', "
                "'transaction.confirmed') "
                f'AND "timeoutBlockHeight" - {int(height)} <= 30'
            ):
                self.add(
                    f"reverse:near-timeout:{swap_id}",
                    f"Reverse swap {swap_id} is {status} with "
                    f"{int(timeout) - height} blocks to its timeout and the "
                    "user has not claimed",
                )

        for swap_id, age in self.psql(
            "SELECT id, round(extract(epoch FROM now() - \"updatedAt\") / 60) "
            "FROM swaps WHERE status = 'invoice.pending' "
            "AND \"updatedAt\" < now() - interval '1 hour'"
        ):
            self.add(
                f"submarine:pending:{swap_id}",
                f"Submarine swap {swap_id} has been paying its invoice for "
                f"{age} minutes",
            )

        for table in ("swaps", '"reverseSwaps"'):
            for swap_id, age in self.psql(
                "SELECT id, round(extract(epoch FROM now() - \"updatedAt\") "
                f"/ 60) FROM {table} WHERE status = "
                "'transaction.claim.pending' "
                "AND \"updatedAt\" < now() - interval '30 minutes'"
            ):
                self.add(
                    f"claim:pending:{swap_id}",
                    f"Swap {swap_id} has waited {age} minutes for its claim",
                )

        for swap_id, status in self.psql(
            'SELECT s.id, s.status FROM "lightningPayments" p '
            'JOIN swaps s ON s."preimageHash" = p."preimageHash" '
            "WHERE p.status = 1 AND s.status NOT IN ('invoice.paid', "
            "'transaction.claim.pending', 'transaction.claimed')"
        ):
            self.add(
                f"submarine:paid-not-claimed:{swap_id}",
                f"Submarine swap {swap_id} was paid but is {status}: claim "
                "its lockup",
            )

    def check_logs(self, since):
        for service, label, patterns in (
            ("boltz", "Backend", LOG_PATTERNS),
            ("shim", "Shim", SHIM_LOG_PATTERNS),
        ):
            out = self.runner.compose(
                "logs", "--no-color", "--no-log-prefix", "--since", since,
                service, timeout=120,
            )
            compiled = [re.compile(p, re.IGNORECASE) for p in patterns]
            for line in out.splitlines():
                line = ANSI.sub("", line).strip()
                if any(p.search(line) for p in compiled):
                    digest = hashlib.sha256(line.encode()).hexdigest()[:16]
                    self.add(f"log:{digest}", f"{label}: {line[:400]}",
                             event=True)

    def check_host(self, root):
        usage = self.runner.disk_usage("/")
        used = usage.used / usage.total
        if used > 0.9:
            self.add(
                "host:disk",
                f"Disk {used:.0%} full ({usage.free / 2**30:.1f} GiB free)",
            )
        for kind in ("io", "memory"):
            try:
                text = self.runner.read(f"/proc/pressure/{kind}")
            except OSError:
                continue
            match = re.search(r"full avg10=\S+ avg60=\S+ avg300=(\S+)", text)
            if match and float(match.group(1)) > 10:
                self.add(
                    f"host:pressure:{kind}",
                    f"The host stalls on {kind}: {match.group(1)}% of the last "
                    "5 minutes with every task waiting (Knots, lnd and the "
                    "backend slow down or time out)",
                )

    def check_backup(self, root):
        scb = f"{root}/lnd/data/chain/bitcoin/{self.network}/channel.backup"
        copy = f"{root}/secrets/channel.backup"
        try:
            live = self.runner.read_bytes(scb)
        except OSError:
            return
        try:
            saved = self.runner.read_bytes(copy)
        except OSError:
            saved = None
        if live != saved:
            # The hourly copy runs at minute 17; give it two chances
            mtime = self.runner.mtime(scb)
            if self.now.timestamp() - mtime > 2.5 * 3600:
                self.add(
                    "backup:channel",
                    "lnd's channel.backup changed more than two hours ago and "
                    "the copy in secrets/ is not the same: check "
                    "/etc/cron.d/lfswap-backup",
                )

    def check_certificate(self, domain):
        if not domain:
            return
        path = f"/etc/letsencrypt/live/{domain}/fullchain.pem"
        try:
            out = self.runner.run(
                ["openssl", "x509", "-enddate", "-noout", "-in", path]
            )
        except (OSError, RuntimeError):
            return
        end = datetime.datetime.strptime(
            out.strip().split("=", 1)[1], "%b %d %H:%M:%S %Y %Z"
        ).replace(tzinfo=datetime.timezone.utc)
        days = (end - self.now).total_seconds() / 86400
        if days < 14:
            self.add(
                "host:certificate",
                f"The TLS certificate for {domain} expires in {days:.0f} days "
                "(certbot renews at 30)",
            )

    def run_all(self, since, root, domain):
        checks = [
            ("containers", self.check_containers),
            ("chain", self.check_chain),
            ("wallet", self.check_wallet),
            ("lnd", self.check_lnd),
            ("swaps", self.check_swaps),
            ("logs", lambda: self.check_logs(since)),
            ("host", lambda: self.check_host(root)),
            ("backup", lambda: self.check_backup(root)),
            ("certificate", lambda: self.check_certificate(domain)),
        ]
        for name, check in checks:
            try:
                check()
            except Exception as error:  # a check that cannot run is an alert
                self.add(
                    f"check:{name}",
                    f"Could not check {name}: {str(error)[:300]}",
                )
        return self.alerts


# --- deciding what to send ------------------------------------------------


def plan(alerts, state, now, repeat_hours, heartbeat_hours):
    """What to send this run, and the state to keep. Conditions are sent when
    they appear, again every `repeat_hours` while they hold, and once more
    when they clear; events once."""
    t = now.timestamp()
    active = state.get("active", {})
    seen_events = state.get("events", {})
    new, repeated, cleared = [], [], []

    current = {}
    for alert in alerts:
        if alert.event:
            if alert.key not in seen_events:
                new.append(alert)
            seen_events[alert.key] = t
            continue
        current[alert.key] = alert.text
        previous = active.get(alert.key)
        if previous is None:
            new.append(alert)
            active[alert.key] = {"since": t, "sent": t, "text": alert.text}
        else:
            previous["text"] = alert.text
            if t - previous["sent"] >= repeat_hours * 3600:
                repeated.append(alert)
                previous["sent"] = t

    for key in list(active):
        if key not in current:
            cleared.append(Alert(key, active.pop(key)["text"]))

    # Forget events after a week; their log lines are long gone by then
    seen_events = {k: v for k, v in seen_events.items() if t - v < 7 * 86400}

    heartbeat = False
    if heartbeat_hours > 0 and t - state.get("heartbeat", 0) >= (
        heartbeat_hours * 3600
    ):
        heartbeat = True

    return (
        {"new": new, "repeated": repeated, "cleared": cleared,
         "heartbeat": heartbeat},
        {**state, "active": active, "events": seen_events},
    )


def message(outcome, active_count):
    lines = []
    for alert in outcome["new"]:
        lines.append(f"NEW {alert.text}")
    for alert in outcome["repeated"]:
        lines.append(f"STILL {alert.text}")
    for alert in outcome["cleared"]:
        lines.append(f"CLEARED {alert.text}")
    if not lines:
        if outcome["heartbeat"]:
            return "Lightning Fork Swap monitor", (
                f"Monitor running; {active_count} open alert(s)."
            )
        return None
    title = f"Lightning Fork Swap: {len(outcome['new'])} new, " + (
        f"{len(outcome['repeated'])} still open, "
        f"{len(outcome['cleared'])} cleared"
    )
    return title, "\n".join(lines)


def payload(kind, title, text, env):
    """URL-independent body and headers for each kind of webhook."""
    if kind == "discord":
        body = {"content": f"**{title}**\n{text}"[:1990]}
    elif kind == "slack":
        body = {"text": f"*{title}*\n{text}"}
    elif kind == "telegram":
        body = {
            "chat_id": env.get("ALERT_TELEGRAM_CHAT_ID", ""),
            "text": f"{title}\n{text}"[:4000],
            "disable_web_page_preview": True,
        }
    elif kind == "ntfy":
        return text.encode(), {"Title": title, "Content-Type": "text/plain"}
    else:
        body = {"title": title, "text": text}
    return json.dumps(body).encode(), {"Content-Type": "application/json"}


def send(env, title, text):
    url = env.get("ALERT_WEBHOOK_URL", "")
    if not url:
        return False
    data, headers = payload(env.get("ALERT_WEBHOOK_KIND", "json"), title, text,
                            env)
    request = urllib.request.Request(url, data=data, headers=headers,
                                     method="POST")
    with urllib.request.urlopen(request, timeout=20) as response:
        if response.status >= 300:
            raise RuntimeError(f"webhook answered {response.status}")
    return True


def main(argv):
    env = load_env(os.path.join(DEPLOY, ".env"))
    root = env.get("LFSWAP_ROOT", "/srv/lfswap")
    network = env.get("NETWORK", "mainnet") or "mainnet"
    now = datetime.datetime.now(datetime.timezone.utc)
    stamp = now.strftime("%Y-%m-%dT%H:%M:%SZ")

    if "--test" in argv:
        sent = send(env, "Lightning Fork Swap: test alert",
                    f"Sent by lfswap_monitor.py --test at {stamp}.")
        print("sent" if sent else "no ALERT_WEBHOOK_URL in .env")
        return 0

    dry = "--dry-run" in argv
    state_dir = os.path.join(root, "monitor")
    state_path = os.path.join(state_dir, "state.json")
    try:
        with open(state_path) as f:
            state = json.load(f)
    except (FileNotFoundError, json.JSONDecodeError):
        state = {}

    if os.path.exists(os.path.join(root, "MIGRATED")):
        print(f"{stamp} this host was migrated away; nothing to watch")
        return 0

    since = state.get("logs_since") or (now - datetime.timedelta(
        minutes=15)).strftime("%Y-%m-%dT%H:%M:%SZ")
    monitor = Monitor(Runner(DEPLOY), env, now, network)
    alerts = monitor.run_all(since, root, env.get("DOMAIN", ""))

    outcome, new_state = plan(
        alerts, state, now,
        number(env, "MONITOR_REPEAT_HOURS", 6),
        number(env, "MONITOR_HEARTBEAT_HOURS", 24),
    )
    msg = message(outcome, len(new_state["active"]))
    if dry:
        for alert in alerts:
            print(f"{stamp} {alert.key}: {alert.text}")
        if msg:
            print(f"--- would send ---\n{msg[0]}\n{msg[1]}")
        return 0

    if msg:
        print(f"{stamp} {msg[0]}\n{msg[1]}")
        try:
            if send(env, *msg):
                new_state["heartbeat"] = now.timestamp()
        except Exception as error:
            # Keep the old state so the same alerts are sent next run
            print(f"{stamp} could not send the alert: {error}", file=sys.stderr)
            return 1

    new_state["logs_since"] = stamp
    os.makedirs(state_dir, mode=0o700, exist_ok=True)
    tmp = state_path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(new_state, f)
    os.replace(tmp, state_path)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
