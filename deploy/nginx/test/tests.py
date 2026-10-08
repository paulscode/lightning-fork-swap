"""What nginx lets through and what it refuses, against the production
config rendered by run.sh. Run by run.sh inside the test network."""

import base64
import http.client
import json
import ssl
import sys
import unittest

NGINX = "nginx"
UNTRUSTED = "nginx-untrusted"
AUTH = "Basic " + base64.b64encode(b"preview:test").decode()


def request(method, path, headers=None, body=None, host=NGINX, port=443,
            auth=True):
    context = ssl._create_unverified_context()
    conn = (http.client.HTTPSConnection(host, port, context=context,
                                        timeout=10)
            if port == 443 else http.client.HTTPConnection(host, port,
                                                           timeout=10))
    all_headers = {"Host": "lightningfork.com"}
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
        ]:
            with self.subTest(method=method, path=path):
                status, _, _ = request(method, path)
                self.assertEqual(status, 404)

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
                self.assertIn("script-src 'self' 'wasm-unsafe-eval';", csp[0])
                self.assertIn("frame-ancestors 'none'", csp[0])
                self.assertNotIn("unsafe-inline';", csp[0].split(
                    "style-src-attr")[0])
                self.assertEqual(header(headers, "X-Frame-Options"), ["DENY"])
                self.assertEqual(len(header(headers,
                                            "Strict-Transport-Security")), 1)

    def test_the_backend_sees_the_real_client_not_a_forged_one(self):
        for path in ["/v2/swap/submarine", "/v2/ws"]:
            with self.subTest(path=path):
                status, _, raw = request(
                    "GET", path, headers={"X-Forwarded-For": "6.6.6.6",
                                          "X-Real-IP": "6.6.6.6"})
                self.assertEqual(status, 200)
                saw = backend_saw(raw)["headers"]
                self.assertNotIn("6.6.6.6", saw.get("x-forwarded-for", ""))
                self.assertNotEqual(saw.get("x-real-ip"), "6.6.6.6")
                self.assertEqual(saw.get("host"), "lightningfork.com")

    def test_the_preview_password_is_required(self):
        status, _, _ = request("GET", "/", auth=False)
        self.assertEqual(status, 401)
        # Only the manifest, which browsers fetch without credentials
        status, _, _ = request("GET", "/manifest.json", auth=False)
        self.assertEqual(status, 200)
        status, _, _ = request("GET", "/v2/swap/submarine", auth=False)
        self.assertEqual(status, 401)

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


class Explorer(unittest.TestCase):
    def test_its_answer_is_inert_on_this_origin(self):
        status, headers, raw = request(
            "GET", "/explorer/api/tx/ab/hex",
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
                         ["/api/tx/ab/hex"])
        self.assertEqual(header(headers, "X-Seen-Cookie"), [""])
        self.assertEqual(header(headers, "X-Seen-Forwarded"), [""])
        self.assertIn(b"<script>", raw)  # served, but as text

    def test_only_get_and_post(self):
        status, _, _ = request("PUT", "/explorer/api/tx", body="x")
        self.assertEqual(status, 403)

    def test_an_upstream_with_an_untrusted_certificate_is_refused(self):
        status, _, raw = request("GET", "/explorer/api/blocks/tip/height",
                                 host=UNTRUSTED)
        self.assertEqual(status, 502, raw[:200])


class RateLimits(unittest.TestCase):
    def test_creating_swaps_is_limited_and_reading_them_is_not(self):
        statuses = [request("GET", "/v2/swap/reverse")[0] for _ in range(12)]
        self.assertEqual(set(statuses), {200})
        statuses = [request("POST", "/v2/swap/reverse", body="{}")[0]
                    for _ in range(8)]
        self.assertIn(503, statuses)
        self.assertEqual(statuses[0], 200)


if __name__ == "__main__":
    result = unittest.main(exit=False, verbosity=2).result
    sys.exit(0 if result.wasSuccessful() else 1)
