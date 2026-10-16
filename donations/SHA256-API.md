# SHA256-chain donations API

Lightning Fork Swap accepts donations on the SHA256 chain through this API.
It needs no account and no key: any tool can use it. It is served at
`https://lightningfork.com/donate/v1/sha256` once the operator has turned it
on.

## What happens to a donation

1. You create a **request**. You get an address of the service's, used by
   this request only. The same address exists on both chains.
2. You send coins to it **on the SHA256 chain**. They are the donation; the
   service keeps them.
3. Whatever reaches the same address **on the BLAKE2b chain** is passed on
   once it is 6 blocks deep:
   - to the **return address** you gave, minus the miner fee;
   - with no return address, to the swap service's own wallet, as a
     donation to its liquidity.

   Coins reach the BLAKE2b chain in three ways:
   - **By themselves, usually.** A transaction valid on both chains is
     relayed on both, since the chains share peers. It is valid on both when
     every coin it spends exists on both chains and none of its signatures
     opts out of the other chain.
   - **Copied by the service.** This happens only if you ask for it and
     prove you own the coins (see "Asking for a copy").
   - **Sent separately.** You can send BLAKE2b coins to the address
     yourself, for example because your coins were already protected and
     nothing reached the BLAKE2b chain. They are passed on the same way.

The service signs everything it passes on so that only the BLAKE2b chain
accepts it (with `SIGHASH_UNIFIED`). Otherwise the same transaction would
also be valid on the SHA256 chain, where it would spend the donation.

Amounts too small to cover the fee and still leave 1,000 sat
(`minForwardSat`) wait at the address until more arrives.

## Endpoints

All bodies are JSON. Errors come as `{"error": "...", "code": "..."}` with
an HTTP status; the codes are listed under each call.

### `GET /donate/v1/sha256/info`

```json
{
  "available": true,
  "powBits": 18,
  "challenge": "AAAAAGn...",
  "minForwardSat": 1000,
  "confirmations": {"sha256": 3, "blake2b": 6},
  "proofMessage": "Lightning Fork Swap: copy <txid> to the BLAKE2b chain for request <id>, return to <returnAddress>"
}
```

- `available`: false while the service is paused. Requests are refused
  then.
- `challenge` and `powBits`: a fresh proof-of-work challenge, valid for 10
  minutes and good for one request (see "Proof of work").
- `confirmations`: when a SHA256-chain deposit counts, and when BLAKE2b
  coins are passed on.

### `POST /donate/v1/sha256`

Creates a request.

```json
{
  "returnAddress": "bc1q...",
  "replay": false,
  "challenge": "AAAAAGn...",
  "nonce": "48213"
}
```

- `returnAddress` (optional): a mainnet address. BLAKE2b coins go there.
  Leave it empty to donate them too.
- `replay` (optional, needs `returnAddress`): ask the service to copy the
  donation to the BLAKE2b chain if it does not get there by itself.
- `challenge`, `nonce`: the proof of work.

The answer (`201`) is the request, as `GET` gives it below. `address` is
normally there at once. If `pending` is true, ask again in a second.

Error codes:
- `bad_request` (400);
- `work` (400): the challenge expired, was used, or the nonce falls short;
- `bad_address` (422);
- `replay_needs_address` (422);
- `unavailable` (503): paused;
- `budget` (503): too many requests today; try later.

### `GET /donate/v1/sha256/{id}`

```json
{
  "id": "Q3x...22 characters",
  "returnAddress": "bc1q...",
  "replay": true,
  "createdAt": "2026-10-16T01:02:03Z",
  "address": "bc1p...",
  "pending": false,
  "sha256": [
    {"chain": "sha256", "txid": "ab...", "vout": 1, "valueSat": 50000,
     "confirmations": 4, "state": "confirmed",
     "copy": "already", "firstSeen": "2026-10-16T01:05:00Z"}
  ],
  "blake2b": [
    {"chain": "blake2b", "txid": "ab...", "vout": 1, "valueSat": 50000,
     "confirmations": 7, "state": "forwarded", "firstSeen": "2026-10-16T01:05:00Z"}
  ],
  "forwards": [
    {"txid": "cd...", "to": "bc1q...", "amountSat": 49700, "feeSat": 300,
     "state": "confirmed", "confirmations": 6, "createdAt": "2026-10-16T02:10:00Z"}
  ]
}
```

States of a SHA256-chain deposit:
- `seen`: not yet 3 deep;
- `confirmed`: the donation counts;
- `gone`: an unconfirmed payment that was replaced or dropped.

States of a BLAKE2b-chain deposit:
- `seen`: not yet 6 deep;
- `ready`: deep enough, about to be passed on, or waiting for more if too
  small;
- `forwarding`: in a forward that is not yet 6 deep;
- `forwarded`: done;
- `gone`: it left the chain before it was deep enough.

`copy`, on SHA256 deposits of requests with `replay`:
- `already`: it reached the BLAKE2b chain by itself;
- `copied`: the service relayed it there;
- `no_proof`: waiting for proofs (`copyReason` names the addresses);
- `not_possible`: the BLAKE2b chain refuses it, and `copyReason` says why.
  The usual reason is that its coins don't exist there unspent. In that
  case your coins were already protected; send the BLAKE2b coins yourself if
  you want them passed on.

Error codes: `not_found` (404).

### `POST /donate/v1/sha256/{id}/tx`

Tells the service your transaction, so it looks at once instead of at its
next poll. Add proofs to ask for a copy.

```json
{
  "txid": "ab...",
  "proofs": [
    {"address": "bc1q...input address", "signature": "base64 BIP322 signature"}
  ]
}
```

The answer is `202` `{"accepted": true}`.

Error codes:
- `bad_txid` (422);
- `bad_proof` (422): a signature does not sign the message for its address;
- `no_replay` (422): proofs for a request that did not ask for a copy;
- `too_many_proofs` (422): more than 20;
- `too_many_hints` (422): more than 20 for one request;
- `not_found` (404).

## Asking for a copy

A copy moves the donor's own coins on the BLAKE2b chain, so the service
makes one only for a request created with `"replay": true` and a return
address, and only with a proof from **every** address the donation spends
from. Each proof is a BIP322 signature (the "simple" form for SegWit and
taproot addresses, or the classic signed message for legacy `1...`
addresses) of exactly this message:

```
Lightning Fork Swap: copy <txid> to the BLAKE2b chain for request <id>, return to <returnAddress>
```

The placeholders are the donation's txid, the request's id, and its return
address as the request shows it.

What the service does:
- It copies only the donation itself, never an earlier transaction, because
  copying one would move someone else's coins.
- It waits for the donation's first SHA256-chain confirmation.
- It checks that the donation isn't on the BLAKE2b chain already, as it
  usually is.

## Proof of work

`challenge` comes from `info`. Find a `nonce`, a string of up to 32
characters such as a decimal counter, for which `SHA-256("<challenge>:<nonce>")`
starts with at least `powBits` zero bits. At 18 bits that takes a fraction
of a second to a few seconds. Each challenge serves one request and expires
after 10 minutes.

```python
import hashlib, itertools
def work(challenge, bits):
    for n in itertools.count():
        h = hashlib.sha256(f"{challenge}:{n}".encode()).digest()
        if int.from_bytes(h, "big") >> (256 - bits) == 0:
            return str(n)
```

## Limits

- Requests a day, all callers together: 300 by default.
- A request is looked at every 30 seconds during its first day. After
  that it is looked at less often: every 10 minutes for a week, hourly
  for a month, daily for a year. A hint (`/tx`) makes the service look at
  once.
- Requests per client are rate limited at the web server.
