// Swaps for migration rehearsals (deploy/rehearsal): start a swap against
// the service on one host, move the service, finish the swap on the other.
//
//   API=http://127.0.0.1:19101 USER_VM=192.168.122.15 node rehearsal.mjs <command> [STATE]
//
//   reverse         a whole reverse swap (pay, claim)
//   submarine       a whole submarine swap (lock up, mine, get paid)
//   reverse-start   pay a reverse swap and wait for its lockup; do not claim
//   reverse-finish  claim it (cooperatively, through whichever host serves now)
//   sub-start       lock up a submarine swap and leave it in the mempool
//   sub-finish      mine, and wait for the service to pay and claim
//
// The user's side (Lightning node, mining) is on USER_VM, run over SSH. The
// API is reached through an SSH tunnel to whichever host runs the service.
import zkpModule from '@vulpemventures/secp256k1-zkp';
import axios from 'axios';
import bolt11 from 'bolt11';
import {
  Musig,
  OutputType,
  SwapTreeSerializer,
  TaprootUtils,
  constructClaimTransaction,
  detectSwap,
  targetFee,
} from 'boltz-core';
import { Transaction, address, crypto, initEccLib, networks } from 'bitcoinjs-lib';
import { execFileSync, spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { readFileSync, writeFileSync } from 'node:fs';
import { ECPairFactory } from 'ecpair';
import * as ecc from 'tiny-secp256k1';

const zkpInit = zkpModule.default ?? zkpModule;
initEccLib(ecc);
const ECPair = ECPairFactory(ecc);
const zkp = await zkpInit();
const network = networks.regtest;

const API = process.env.API ?? 'http://127.0.0.1:19101';
const USER_VM = process.env.USER_VM ?? '192.168.122.15';
const USER_DIR = '/opt/lfswap/deploy/rehearsal/user';

// Throwaway rehearsal VMs whose addresses are reused with new host keys.
const SSH_OPTS = ['-o', 'StrictHostKeyChecking=no', '-o', 'UserKnownHostsFile=/dev/null', '-o', 'LogLevel=ERROR'];
const onUser = (command) =>
  execFileSync('ssh', [...SSH_OPTS, `root@${USER_VM}`, `cd ${USER_DIR} && ${command}`], {
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
  }).trim();
const lncli = (...args) => JSON.parse(onUser(`docker compose exec -T lnd-user lncli --network=regtest ${args.join(' ')}`));
const knots = (...args) => onUser(`docker compose exec -T knots bitcoin-cli -regtest -rpcuser=lab -rpcpassword=lab ${args.join(' ')}`);
const mine = (n = 1) => knots('generatetoaddress', String(n), knots('-rpcwallet=miner', 'getnewaddress'));
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const status = async (id) => (await axios.get(`${API}/v2/swap/${id}`)).data.status;

const waitStatus = async (id, wanted, { timeoutMs = 180_000, mineEvery = 0 } = {}) => {
  const set = new Set([wanted].flat());
  const start = Date.now();
  let n = 0;
  while (Date.now() - start < timeoutMs) {
    let s;
    try {
      s = await status(id);
    } catch {
      s = 'unreachable';
    }
    if (set.has(s)) return s;
    if (mineEvery && ++n % mineEvery === 0) mine(1);
    await sleep(2000);
  }
  throw new Error(`swap ${id} did not reach ${[...set].join('|')} (last: ${await status(id).catch(() => '?')})`);
};

const paymentHash = (invoice) => bolt11.decode(invoice).tags.find((t) => t.tagName === 'payment_hash').data;

// --- reverse ---------------------------------------------------------------

const reverseStart = async (amount = 40_000) => {
  const preimage = randomBytes(32);
  const keys = ECPair.makeRandom();
  const swap = (
    await axios.post(`${API}/v2/swap/reverse`, {
      invoiceAmount: amount, from: 'BTC', to: 'BTC',
      claimPublicKey: keys.publicKey.toString('hex'),
      preimageHash: crypto.sha256(preimage).toString('hex'),
    })
  ).data;
  // The hold invoice settles only when the service learns the preimage, so
  // the payment runs detached on the user VM.
  spawn('ssh', [...SSH_OPTS, `root@${USER_VM}`, `cd ${USER_DIR} && nohup docker compose exec -T lnd-user lncli --network=regtest payinvoice --force --timeout 1800s ${swap.invoice} >/tmp/pay-${swap.id}.log 2>&1 &`], { stdio: 'ignore' }).unref();
  await waitStatus(swap.id, ['transaction.mempool', 'transaction.confirmed']);
  return { kind: 'reverse', swap, preimage: preimage.toString('hex'), key: keys.privateKey.toString('hex') };
};

const reverseFinish = async (state) => {
  const { swap } = state;
  const preimage = Buffer.from(state.preimage, 'hex');
  const keys = ECPair.fromPrivateKey(Buffer.from(state.key, 'hex'));
  const lockupHex = (await axios.get(`${API}/v2/swap/reverse/${swap.id}/transaction`)).data.hex;
  const lockupTx = Transaction.fromHex(lockupHex);
  const boltzPublicKey = Buffer.from(swap.refundPublicKey, 'hex');
  const musig = new Musig(zkp, keys, randomBytes(32), [boltzPublicKey, keys.publicKey]);
  const tweakedKey = TaprootUtils.tweakMusig(musig, SwapTreeSerializer.deserializeSwapTree(swap.swapTree).tree);
  const swapOutput = detectSwap(tweakedKey, lockupTx);
  if (!swapOutput) throw new Error('no swap output in the lockup');
  const destination = lncli('newaddress', 'p2tr').address;
  const claimTx = targetFee(2, (fee) =>
    constructClaimTransaction(
      [{ ...swapOutput, keys, preimage, cooperative: true, type: OutputType.Taproot, txHash: lockupTx.getHash() }],
      address.toOutputScript(destination, network), fee));
  const sig = (
    await axios.post(`${API}/v2/swap/reverse/${swap.id}/claim`, {
      index: 0, transaction: claimTx.toHex(), preimage: state.preimage,
      pubNonce: Buffer.from(musig.getPublicNonce()).toString('hex'),
    })
  ).data;
  musig.aggregateNonces([[boltzPublicKey, Buffer.from(sig.pubNonce, 'hex')]]);
  musig.initializeSession(claimTx.hashForWitnessV1(0, [swapOutput.script], [swapOutput.value], Transaction.SIGHASH_DEFAULT));
  musig.addPartial(boltzPublicKey, Buffer.from(sig.partialSignature, 'hex'));
  musig.signPartial();
  claimTx.ins[0].witness = [musig.aggregatePartials()];
  const { id } = (await axios.post(`${API}/v2/chain/BTC/transaction`, { hex: claimTx.toHex() })).data;
  mine(1);
  await waitStatus(swap.id, 'invoice.settled');
  return `reverse ${swap.id}: claimed ${swapOutput.value} sat in ${id.slice(0, 16)}…, invoice settled`;
};

// --- submarine -------------------------------------------------------------

const subStart = async (amount = 30_000) => {
  const invoice = lncli('addinvoice', '--amt', String(amount)).payment_request;
  const keys = ECPair.makeRandom();
  const swap = (
    await axios.post(`${API}/v2/swap/submarine`, {
      invoice, from: 'BTC', to: 'BTC', refundPublicKey: keys.publicKey.toString('hex'),
    })
  ).data;
  const txid = lncli('sendcoins', '--addr', swap.address, '--amt', String(swap.expectedAmount), '--sat_per_vbyte', '2').txid;
  return { kind: 'submarine', swap, invoice, key: keys.privateKey.toString('hex'), lockup: txid };
};

const subFinish = async (state) => {
  const { swap, invoice } = state;
  const keys = ECPair.fromPrivateKey(Buffer.from(state.key, 'hex'));
  mine(1);
  const s = await waitStatus(swap.id, ['transaction.claim.pending', 'transaction.claimed', 'invoice.paid'], { mineEvery: 5 });
  if (s === 'transaction.claim.pending') {
    const details = (await axios.get(`${API}/v2/swap/submarine/${swap.id}/claim`)).data;
    const boltzPublicKey = Buffer.from(swap.claimPublicKey, 'hex');
    const musig = new Musig(zkp, keys, randomBytes(32), [boltzPublicKey, keys.publicKey]);
    TaprootUtils.tweakMusig(musig, SwapTreeSerializer.deserializeSwapTree(swap.swapTree).tree);
    musig.aggregateNonces([[boltzPublicKey, Buffer.from(details.pubNonce, 'hex')]]);
    musig.initializeSession(Buffer.from(details.transactionHash, 'hex'));
    await axios.post(`${API}/v2/swap/submarine/${swap.id}/claim`, {
      pubNonce: Buffer.from(musig.getPublicNonce()).toString('hex'),
      partialSignature: Buffer.from(musig.signPartial()).toString('hex'),
    });
  }
  await waitStatus(swap.id, 'transaction.claimed', { mineEvery: 5 });
  const state_ = lncli('lookupinvoice', paymentHash(invoice)).state;
  if (state_ !== 'SETTLED') throw new Error(`invoice ${state_}`);
  return `submarine ${swap.id}: invoice paid, lockup ${state.lockup.slice(0, 16)}… claimed`;
};

// --- main ------------------------------------------------------------------

const [command, stateFile] = process.argv.slice(2);
const save = (s) => writeFileSync(stateFile, JSON.stringify(s, null, 1), { mode: 0o600 });
const load = () => JSON.parse(readFileSync(stateFile, 'utf8'));

try {
  switch (command) {
    case 'reverse': console.log(await reverseFinish(await reverseStart())); break;
    case 'submarine': console.log(await subFinish(await subStart())); break;
    case 'reverse-start': { const s = await reverseStart(); save(s); console.log(`reverse ${s.swap.id} paid and locked up, not claimed; state in ${stateFile}`); break; }
    case 'reverse-finish': console.log(await reverseFinish(load())); break;
    case 'sub-start': { const s = await subStart(); save(s); console.log(`submarine ${s.swap.id} locked up in ${s.lockup.slice(0, 16)}…, unconfirmed; state in ${stateFile}`); break; }
    case 'sub-finish': console.log(await subFinish(load())); break;
    default: throw new Error('usage: rehearsal.mjs reverse|submarine|reverse-start|reverse-finish|sub-start|sub-finish [STATE]');
  }
} catch (err) {
  console.error('FAIL', err.response?.data ? JSON.stringify(err.response.data) : err.stack ?? String(err));
  process.exit(1);
}
