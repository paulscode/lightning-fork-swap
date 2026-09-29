# Lightning Fork Swap

Non-custodial swaps between the Bitcoin BLAKE2b chain and its Lightning
network: submarine swaps (on-chain → Lightning) and reverse swaps
(Lightning → on-chain), built on [Boltz](https://github.com/BoltzExchange)
and [Lightning Fork](https://github.com/paulscode/lightning-fork).

This repository holds what runs the service:

| Path | What |
| --- | --- |
| `shim/` | `txindex-shim`: a JSON-RPC proxy that lets the swap backend run against a pruned Bitcoin Knots node (see below) |
| `deploy/` | Docker Compose file, config templates, nginx site, firewall and setup scripts for one Debian host |

The backend and the web app are forks kept in their own repositories:
`lightning-fork-swap-backend` (from `BoltzExchange/boltz-backend` by way of
`SwapMarket/boltz-backend`) and `lightning-fork-swap-webapp` (from
`BoltzExchange/boltz-web-app`).

## What the Bitcoin BLAKE2b chain changes

The chain keeps Bitcoin's genesis block, `bc1` addresses, transaction format
and key derivation, so swap scripts and signing are unchanged. Three things
needed work:

- **Block headers.** From height 961640 a header may be 164 bytes, and its
  block id is a BLAKE2b construction. The backend's sidecar decodes raw
  blocks itself; it now reads both header forms.
- **Invoices.** Lightning nodes on this chain mark invoices with the required
  feature bit 512 (`option_blake2b`). The backend accepts that bit and refuses
  invoices without it, since an invoice from a SHA256-chain node looks the
  same otherwise and could not be paid.
- **Chain identity.** The backend refuses to run unless block 961640 is the
  BLAKE2b chain's activation block
  (`0000000000000050c1e5f69672f459293be14f46e5a494e7a8c8541396f18eeb`).

## Why the shim

Boltz looks up transactions by id (`getrawtransaction <txid>`), which needs
`-txindex`, and Knots cannot combine `-txindex` with pruning. The shim sits
between the backend and the node, forwards every request, and when the node
cannot find a confirmed transaction it retries with the block hash from its
own index of recent blocks (4032 by default, about four weeks). It also
relays the node's ZMQ ports, because the backend connects to ZMQ at its RPC
host.

## Running it

See `deploy/README.md`.

## License

AGPL-3.0, as the Boltz code it builds on.
