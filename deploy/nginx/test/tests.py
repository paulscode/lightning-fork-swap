"""What nginx lets through and what it refuses, against the production
config rendered by run.sh. Run by run.sh inside the test network."""

import base64
import http.client
import json
import os
import socket
import ssl
import sys
import time
import unittest

NGINX = os.environ.get("LFSWAP_NGINX", "nginx")
UNTRUSTED = "nginx-untrusted"
AUTH = "Basic " + base64.b64encode(b"preview:test").decode()


class SNIConnection(http.client.HTTPSConnection):
    """HTTPS to a container, presenting the site's name in the handshake."""

    def __init__(self, *args, sni="lightningfork.com", **kwargs):
        super().__init__(*args, **kwargs)
        self.sni = sni

    def connect(self):
        sock = socket.create_connection((self.host, self.port), self.timeout)
        self.sock = self._context.wrap_socket(sock, server_hostname=self.sni)


def request(method, path, headers=None, body=None, host=NGINX, port=443,
            auth=True, sni="lightningfork.com", host_header="lightningfork.com"):
    context = ssl._create_unverified_context()
    conn = (SNIConnection(host, port, context=context, timeout=10, sni=sni)
            if port == 443 else http.client.HTTPConnection(host, port,
                                                           timeout=10))
    all_headers = {"Host": host_header}
    if auth:
        all_headers["Authorization"] = AUTH
    all_headers.update(headers or {})
    # putrequest sends the path exactly as given: no normalisation
    conn.putrequest(method, path, skip_host=True, skip_accept_encoding=True)
    for name, value in all_headers.items():
        conn.putheader(name, value)
    if body is not None:
        data = body.encode()
        conn.putheader("Content-Length", str(len(data)))
        conn.endheaders(data)
    else:
        conn.endheaders()
    response = conn.getresponse()
    raw = response.read()
    headers = response.getheaders()
    conn.close()
    return response.status, headers, raw


def header(headers, name):
    values = [v for k, v in headers if k.lower() == name.lower()]
    return values


def backend_saw(raw):
    return json.loads(raw)


class Allowlist(unittest.TestCase):
    def test_the_routes_the_app_uses_reach_the_backend_unchanged(self):
        for method, path, port in [
            ("GET", "/v2/swap/submarine", 9001),
            ("POST", "/v2/swap/reverse", 9001),
            ("GET", "/v2/swap/status?id=AbCdEf123456", 9001),
            ("GET", "/v2/swap/AbCdEf123456", 9001),
            ("GET", "/v2/swap/submarine/AbCdEf123456/transaction", 9001),
            ("POST", "/v2/swap/submarine/AbCdEf123456/refund", 9001),
            ("POST", "/v2/swap/reverse/AbCdEf123456/claim", 9001),
            ("GET", "/v2/chain/fees", 9001),
            ("POST", "/v2/chain/BTC/transaction", 9001),
            ("POST", "/v2/swap/restore", 9005),
            ("GET", "/donate/v1/health", 9010),
            ("GET", "/donate/v1/replay/" + "ab" * 32, 9010),
            ("GET", "/donate/v1/info", 9010),
            ("POST", "/donate/v1/channel-orders", 9010),
            ("GET", "/donate/v1/channel-orders/AbCdEfGhIjKlMnOpQrSt_-", 9010),
            ("PATCH", "/donate/v1/channel-orders/AbCdEfGhIjKlMnOpQrSt_-/node", 9010),
            ("GET", "/donate/v1/sha256/info", 9010),
            ("POST", "/donate/v1/sha256", 9010),
            ("GET", "/donate/v1/sha256/AbCdEfGhIjKlMnOpQrSt_-", 9010),
            ("POST", "/donate/v1/sha256/AbCdEfGhIjKlMnOpQrSt_-/tx", 9010),
        ]:
            with self.subTest(method=method, path=path):
                status, _, raw = request(method, path, body="{}"
                                         if method == "POST" else None)
                self.assertEqual(status, 200)
                saw = backend_saw(raw)
                self.assertEqual((saw["method"], saw["path"], saw["port"]),
                                 (method, path, port))

    def test_routes_the_app_does_not_use_are_not_found(self):
        for method, path in [
            ("GET", "/v2/nodes"),
            ("GET", "/v2/version"),
            ("GET", "/v2/swap/chain"),
            ("POST", "/v2/swap/chain"),
            ("POST", "/v2/swap/submarine/AbCdEf123456/invoice"),
            ("PATCH", "/v2/swap/AbCdEf123456"),
            ("POST", "/v2/swap/rescue"),
            ("POST", "/v2/swap/restore/index"),
            ("GET", "/v2/lightning/BTC/node/02ab"),
            ("GET", "/v2/swap/status/extra"),
            ("GET", "/v2/referral"),
            ("POST", "/donate/v1/replay/" + "ab" * 32),
            ("GET", "/donate/v1/replay/" + "AB" * 32),
            ("GET", "/donate/v1/replay/ab"),
            ("GET", "/donate/v1/replays"),
            ("GET", "/donate/v1/"),
            ("GET", "/donate/v1/channel-orders"),
            ("DELETE", "/donate/v1/channel-orders/AbCdEfGhIjKlMnOpQrSt_-"),
            ("POST", "/donate/v1/channel-orders/AbCdEfGhIjKlMnOpQrSt_-"),
            ("PATCH", "/donate/v1/channel-orders/AbCdEfGhIjKlMnOpQrSt_-"),
            ("GET", "/donate/v1/channel-orders/short"),
            ("GET", "/donate/v1/sha256"),
            ("DELETE", "/donate/v1/sha256/AbCdEfGhIjKlMnOpQrSt_-"),
            ("GET", "/donate/v1/sha256/AbCdEfGhIjKlMnOpQrSt_-/tx"),
            ("POST", "/donate/v1/sha256/short/tx"),
            ("PUT", "/donate/v1/channel-orders/AbCdEfGhIjKlMnOpQrSt_-/node"),
        ]:
            with self.subTest(method=method, path=path):
                status, _, _ = request(method, path)
                self.assertEqual(status, 404)

    def test_the_donate_page_is_the_app(self):
        status, headers, raw = request("GET", "/donate")
        self.assertEqual(status, 200)
        self.assertIn(b"<title>app</title>", raw)

    def test_restore_is_post_only(self):
        status, _, _ = request("GET", "/v2/swap/restore")
        self.assertEqual(status, 403)


class PathGuard(unittest.TestCase):
    def test_paths_that_normalise_to_something_else_are_refused(self):
        for path in [
            "//v2/nodes/../swap/status",
            "/./v2/nodes/../swap/status",
            "/x/../v2/swap/AbCdEf123456",
            "/v2%2fnodes%2f..%2fswap%2fstatus",
            "/v2/swap/AbCdEf123456%0a",
            "/v2/swap/./status",
            "/v2//swap/status",
            "/v2/swap/%41bCdEf123456",
        ]:
            with self.subTest(path=path):
                status, _, raw = request("GET", path)
                self.assertEqual(status, 400, raw[:200])

    def test_a_query_may_carry_encoded_bytes(self):
        status, _, _ = request("GET", "/swap?destination=lnbc%201")
        self.assertEqual(status, 200)
        status, _, raw = request("GET", "/v2/swap/status?id=Ab%43d")
        self.assertEqual(status, 200)
        self.assertEqual(backend_saw(raw)["path"], "/v2/swap/status?id=Ab%43d")


class Headers(unittest.TestCase):
    def test_the_app_is_served_with_a_strict_policy(self):
        for path in ["/", "/swap/AbCdEf123456", "/v2/swap/submarine"]:
            with self.subTest(path=path):
                status, headers, _ = request("GET", path)
                self.assertEqual(status, 200)
                csp = header(headers, "Content-Security-Policy")
                self.assertEqual(len(csp), 1)
                self.assertIn("script-src 'self';", csp[0])
                self.assertNotIn("eval", csp[0])
                self.assertIn("frame-ancestors 'none'", csp[0])
                self.assertNotIn("unsafe-inline';", csp[0].split(
                    "style-src-attr")[0])
                self.assertEqual(header(headers, "X-Frame-Options"), ["DENY"])
                self.assertEqual(header(headers, "Strict-Transport-Security"),
                                 ["max-age=31536000; includeSubDomains"])

    def test_no_service_behind_nginx_sees_the_client_address(self):
        me = my_address()
        for path in ["/v2/swap/submarine", "/v2/ws", "/donate/v1/health"]:
            with self.subTest(path=path):
                status, _, raw = request(
                    "GET", path, headers={"X-Forwarded-For": "6.6.6.6",
                                          "X-Real-IP": "6.6.6.6"})
                self.assertEqual(status, 200)
                saw = backend_saw(raw)["headers"]
                for name in ("x-forwarded-for", "x-real-ip"):
                    self.assertEqual(saw.get(name, ""), "", name)
                self.assertNotIn(me, json.dumps(saw))
                self.assertEqual(saw.get("host"), "lightningfork.com")

    def test_the_preview_password_is_required(self):
        status, _, _ = request("GET", "/", auth=False)
        self.assertEqual(status, 401)
        # Only the manifest, which browsers fetch without credentials
        status, _, _ = request("GET", "/manifest.json", auth=False)
        self.assertEqual(status, 200)
        status, _, _ = request("GET", "/v2/swap/submarine", auth=False)
        self.assertEqual(status, 401)

    def test_other_names_get_nothing(self):
        # No certificate for a name that is not the site's
        with self.assertRaises(ssl.SSLError):
            request("GET", "/", sni="example.com")
        with self.assertRaises(ssl.SSLError):
            request("GET", "/", sni=None)
        # A forged Host on the site's handshake: closed without an answer
        with self.assertRaises(http.client.RemoteDisconnected):
            request("GET", "/", host_header="example.com")
        with self.assertRaises(http.client.RemoteDisconnected):
            request("GET", "/", port=80, auth=False,
                    host_header="example.com")

    def test_redirects(self):
        status, headers, _ = request("GET", "/x", headers={
            "Host": "www.lightningfork.com"})
        self.assertEqual(status, 301)
        self.assertEqual(header(headers, "Location"),
                         ["https://lightningfork.com/x"])
        status, headers, _ = request("GET", "/x?y=1", port=80, auth=False)
        self.assertEqual(status, 301)
        self.assertEqual(header(headers, "Location"),
                         ["https://lightningfork.com/x?y=1"])


def my_address():
    """This client's address as nginx sees it (one network, no NAT)."""
    probe = socket.create_connection((NGINX, 443), 5)
    try:
        return probe.getsockname()[0]
    finally:
        probe.close()


def read_log(name):
    with open(os.path.join("/logs", name), encoding="utf-8") as f:
        return f.read()


def new_lines(name, before, at_least):
    """The log's lines after offset `before`, once there are `at_least`:
    nginx writes a line just after the answer has gone out."""
    for _ in range(50):
        lines = read_log(name)[before:]
        if len(lines.splitlines()) >= at_least:
            return lines
        time.sleep(0.05)
    return lines


class Logs(unittest.TestCase):
    SWAP = "AbCdEfGhIjKl"
    TXID = "ab" * 32
    ADDRESS = "bc1qprivacytestaddressxxxxxxxxxxxxxxxxxxxx"
    ORDER = "AbCdEfGhIjKlMnOpQrSt_-"

    def test_the_access_log_keeps_no_address_and_no_identifiers(self):
        if not os.path.isdir("/logs"):
            self.skipTest("run by run.sh with the logs mounted")
        before = len(read_log("access.log"))
        for path in [
            f"/v2/swap/{self.SWAP}",
            f"/v2/swap/submarine/{self.SWAP}/transaction",
            f"/v2/swap/reverse/lnbc{'1' * 40}/bip21",
            f"/explorer/api/address/{self.ADDRESS}/utxo",
            f"/explorer/api/tx/{self.TXID}/outspend/1",
            f"/donate/v1/replay/{self.TXID}",
            f"/donate/v1/sha256/{self.ORDER}",
            f"/swap/{self.SWAP}?secret=1",
        ]:
            request("GET", path, headers={"User-Agent": "Mozilla/5.0 (X11) Firefox/140.0"})
        lines = new_lines("access.log", before, 8)
        self.assertTrue(lines.strip())
        for secret in (my_address(), self.SWAP, self.TXID, self.ADDRESS,
                       self.ORDER, "lnbc111", "secret", "X11"):
            self.assertNotIn(secret, lines)
        for redacted in ("/v2/swap/:id ", "/v2/swap/submarine/:id/transaction",
                         "/v2/swap/reverse/:invoice/bip21",
                         "/explorer/api/address/:address/utxo",
                         "/explorer/api/tx/:txid/outspend/1",
                         "/donate/v1/replay/:txid", "/donate/v1/sha256/:id",
                         "/swap/* "):
            self.assertIn(redacted, lines)
        # One client, one tag (12 hex characters), and the agent's kind
        tags = {line.split()[1] for line in lines.splitlines()}
        self.assertEqual(len(tags), 1)
        self.assertRegex(tags.pop(), r"^[0-9a-f]{12}$")
        self.assertIn(" firefox", lines)
        # The monitor's parser still finds method, path and status
        self.assertRegex(lines, r'"GET /v2/swap/:id HTTP/[0-9.]+" 200 ')

    def test_the_threat_log_keeps_the_hostile_by_address(self):
        if not os.path.isdir("/logs"):
            self.skipTest("run by run.sh with the logs mounted")
        before = len(read_log("threat.log"))
        me = my_address()
        request("GET", "/wp-login.php")
        request("GET", "/v2/swap/chain")
        request("GET", "/v2/swap/../../etc/passwd")
        # Another name: nginx closes the connection without an answer (444)
        with self.assertRaises(http.client.RemoteDisconnected):
            request("GET", "/", port=80, auth=False,
                    host_header="198.51.100.7")
        # Normal use is not a threat
        request("GET", "/v2/swap/submarine")
        request("GET", "/network")
        lines = new_lines("threat.log", before, 4).splitlines()
        self.assertEqual(len(lines), 4, lines)
        for line, path in zip(lines, ["/wp-login.php", "/v2/swap/chain",
                                      "/v2/swap/../../etc/passwd", "/"]):
            self.assertEqual(line.split()[1], me)
            self.assertIn(f" {path} ", line)

    def test_the_too_fast_are_kept_by_tag_only(self):
        if not os.path.isdir("/logs"):
            self.skipTest("run by run.sh with the logs mounted")
        before = len(read_log("threat.log"))
        statuses = [request("GET", "/explorer/api/blocks/tip/height")[0]
                    for _ in range(40)]
        self.assertIn(503, statuses)
        lines = read_log("threat.log")[before:].splitlines()
        self.assertTrue(lines)
        for line in lines:
            self.assertRegex(line.split()[1], r"^[0-9a-f]{12}$")
            self.assertNotIn(my_address(), line)
            self.assertIn(" 503 ", line)


class Explorer(unittest.TestCase):
    def test_its_answer_is_inert_on_this_origin(self):
        status, headers, raw = request(
            "GET", "/explorer/api/tx/abababababababababababababababababababababababababababababababab/hex",
            headers={"Cookie": "a=b", "X-Forwarded-For": "6.6.6.6"})
        self.assertEqual(status, 200)
        self.assertEqual(header(headers, "Content-Type"),
                         ["text/plain; charset=utf-8"])
        self.assertEqual(header(headers, "Content-Security-Policy"),
                         ["default-src 'none'; sandbox"])
        for name in ("Set-Cookie", "Service-Worker-Allowed",
                     "Access-Control-Allow-Origin"):
            self.assertEqual(header(headers, name), [], name)
        self.assertEqual(header(headers, "X-Content-Type-Options"),
                         ["nosniff"])
        self.assertEqual(header(headers, "X-Explorer-Path"),
                         ["/api/tx/abababababababababababababababababababababababababababababababab/hex"])
        self.assertEqual(header(headers, "X-Seen-Cookie"), [""])
        self.assertEqual(header(headers, "X-Seen-Forwarded"), [""])
        self.assertIn(b"<script>", raw)  # served, but as text

    def test_only_the_paths_the_app_uses(self):
        for method, path in [
            ("GET", "/explorer/api/address/bc1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh/utxo"),
            ("GET", "/explorer/api/address/bc1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh"),
            ("GET", "/explorer/api/address/bc1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh/txs/mempool"),
            ("GET", "/explorer/api/tx/abababababababababababababababababababababababababababababababab/hex"),
            ("GET", "/explorer/api/tx/abababababababababababababababababababababababababababababababab/status"),
            ("GET", "/explorer/api/tx/abababababababababababababababababababababababababababababababab/outspend/1"),
            ("GET", "/explorer/api/blocks/tip/height"),
            ("GET", "/explorer/api/fee-estimates"),
            ("GET", "/explorer/api/v1/fees/recommended"),
            ("POST", "/explorer/api/tx"),
        ]:
            with self.subTest(method=method, path=path):
                status, _, _ = request(method, path, body="x"
                                       if method == "POST" else None)
                self.assertEqual(status, 200)
        for method, path in [
            ("PUT", "/explorer/api/tx"),
            ("GET", "/explorer/api/tx"),
            ("POST", "/explorer/api/tx/abababababababababababababababababababababababababababababababab/hex"),
            ("GET", "/explorer/api/mempool/recent"),
            ("GET", "/explorer/api/v1/mining/pools/1w"),
            ("GET", "/explorer/api/tx/ABABABABABABABABABABABABABABABABABABABABABABABABABABABABABABABAB/hex"),
            ("GET", "/explorer/api/tx/ab/hex"),
            ("GET", "/explorer/api/address/x/txs"),
            ("GET", "/explorer/api/address/bc1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh/txs"),
            ("GET", "/explorer/api/address/bc1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh/txs/chain"),
            ("POST", "/explorer/api/address/bc1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh"),
            ("GET", "/explorer/api/"),
        ]:
            with self.subTest(method=method, path=path):
                status, _, _ = request(method, path, body="x"
                                       if method in ("POST", "PUT") else None)
                self.assertEqual(status, 404)

    def test_an_upstream_with_an_untrusted_certificate_is_refused(self):
        status, _, raw = request("GET", "/explorer/api/blocks/tip/height",
                                 host=UNTRUSTED)
        self.assertEqual(status, 502, raw[:200])


class Graph(unittest.TestCase):
    def test_the_sky_files_are_served_cached_right(self):
        status, headers, raw = request("GET", "/graph/meta.json")
        self.assertEqual((status, raw.strip()), (200, b'{"version": 3}'))
        self.assertEqual(header(headers, "Cache-Control"), ["no-cache"])
        status, headers, _ = request("GET", "/graph/v3/overview.json")
        self.assertEqual(status, 200)
        self.assertEqual(header(headers, "Cache-Control"),
                         ["public, max-age=31536000, immutable"])
        # The server's headers are still there
        self.assertEqual(len(header(headers, "Content-Security-Policy")), 1)
        self.assertEqual(header(headers, "X-Content-Type-Options"), ["nosniff"])

    def test_the_gzip_copy(self):
        status, headers, raw = request("GET", "/graph/v3/overview.json",
                                       headers={"Accept-Encoding": "gzip"})
        self.assertEqual(status, 200)
        self.assertEqual(header(headers, "Content-Encoding"), ["gzip"])
        self.assertEqual(raw[:2], b"\x1f\x8b")

    def test_nothing_else(self):
        for method, path, want in [
            ("GET", "/graph/v3/missing.json", 404),
            ("GET", "/graph/", 404),
            ("GET", "/graph/v3/node/", 404),
            ("POST", "/graph/meta.json", 403),
            ("PUT", "/graph/v4/overview.json", 403),
        ]:
            with self.subTest(method=method, path=path):
                status, _, raw = request(method, path, body="x"
                                         if method in ("POST", "PUT") else None)
                self.assertEqual(status, want)
                self.assertNotIn(b"<title>app</title>", raw)


class RateLimits(unittest.TestCase):
    def test_creating_swaps_is_limited_and_reading_them_is_not(self):
        statuses = [request("GET", "/v2/swap/reverse")[0] for _ in range(12)]
        self.assertEqual(set(statuses), {200})
        statuses = [request("POST", "/v2/swap/reverse", body="{}")[0]
                    for _ in range(8)]
        self.assertIn(503, statuses)
        self.assertEqual(statuses[0], 200)
        # Creating a channel donation counts against the same limit
        status, _, _ = request("POST", "/donate/v1/channel-orders", body="{}")
        self.assertEqual(status, 503)
        # So does a SHA256-chain donation request
        status, _, _ = request("POST", "/donate/v1/sha256", body="{}")
        self.assertEqual(status, 503)


    def test_a_big_body_is_refused(self):
        status, _, _ = request("PATCH",
                               "/donate/v1/channel-orders/AbCdEfGhIjKlMnOpQrSt_-/node",
                               body="x" * 17000)
        self.assertEqual(status, 413)
        # Creating takes little; a hint with its proofs takes more
        for path in ("/donate/v1/channel-orders", "/donate/v1/sha256"):
            status, _, _ = request("POST", path, body="x" * 5000)
            self.assertEqual(status, 413, path)
        status, _, _ = request("POST", "/donate/v1/sha256/AbCdEfGhIjKlMnOpQrSt_-/tx",
                               body="x" * 10000)
        self.assertNotEqual(status, 413)

    def test_the_donations_api_is_limited(self):
        # Earlier tests used part of the burst: let it refill (2 a second)
        time.sleep(6)
        path = "/donate/v1/replay/" + "cd" * 32
        statuses = [request("GET", path)[0] for _ in range(20)]
        self.assertEqual(statuses[0], 200)
        self.assertIn(503, statuses)


class SharedPrefix(unittest.TestCase):
    """Run by run.sh from two addresses in one IPv6 /64, one after the other
    (LFSWAP_PHASE=first, then second): together they get one client's
    creation limit, not one each."""

    def test_one_64_is_one_client(self):
        phase = os.environ.get("LFSWAP_PHASE")
        if phase is None:
            self.skipTest("run by run.sh in the IPv6 phase")
        if phase == "first":
            statuses = [request("POST", "/v2/swap/submarine", body="{}")[0]
                        for _ in range(6)]
            self.assertEqual(set(statuses), {200})
        else:
            status, _, _ = request("POST", "/v2/swap/submarine", body="{}")
            self.assertEqual(status, 503)


if __name__ == "__main__":
    only = os.environ.get("LFSWAP_ONLY")
    if only:
        suite = unittest.defaultTestLoader.loadTestsFromName(
            only, sys.modules[__name__])
        ok = unittest.TextTestRunner(verbosity=2).run(suite).wasSuccessful()
        sys.exit(0 if ok else 1)
    result = unittest.main(exit=False, verbosity=2).result
    sys.exit(0 if result.wasSuccessful() else 1)
