// End-to-end swaps against the regtest service in deploy/regtest.
//
// The client side follows Boltz's own API examples (boltz-core 2, bitcoinjs),
// so it exercises the backend the way a third-party client would. The user's
// Lightning Fork node (lnd-user) pays and receives on Lightning and funds
// lockups on chain; blocks are mined on demand.
//
//   node run.mjs            all scenarios
//   node run.mjs reverse    one scenario by name

import zkpModule from '@vulpemventures/secp256k1-zkp';

// A CommonJS module: its default export arrives wrapped once more.
const zkpInit = zkpModule.default ?? zkpModule;
import axios from 'axios';
import bolt11 from 'bolt11';
import {
  Musig,
  OutputType,
  SwapTreeSerializer,
  TaprootUtils,
  constructClaimTransaction,
  constructRefundTransaction,
  detectSwap,
  targetFee,
} from 'boltz-core';
import { Transaction, address, crypto, initEccLib, networks, payments, script as bscript } from 'bitcoinjs-lib';
import { execFileSync, spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { ECPairFactory } from 'ecpair';
import * as ecc from 'tiny-secp256k1';

initEccLib(ecc);
const ECPair = ECPairFactory(ecc);
const zkp = await zkpInit();
const network = networks.regtest;

const API = process.env.API ?? 'http://127.0.0.1:19001';
const COMPOSE_DIR = join(dirname(fileURLToPath(import.meta.url)), '..', 'deploy', 'regtest');

// A regtest invoice from a stock lnd on the SHA256 chain: no feature bit 512.
// deploy/regtest/boltz.conf: requiredConfirmations
const LOCKUP_CONFIRMATIONS = 2;

const SHA256_CHAIN_INVOICE =
  'lnbcrt12340n1p4tkt6jpp59qm9ghv5e3302uk3z4l5c7mz7cg8x6zpmz7uyc2yt92ttf8qtd8sdqqcqzzsxqyz5vqsp5vt9hhkrtnlhewm00wt0m6curu4ak5k5ky66zwh99urdfata77xfs9qxpqysgqxfq2taljltqeky5nhy34l3ak7mmrvzsgd8m5y93v4m4str6wsw63rhvqaqfgu42uw0j9hfwq0r25njxktpy2eq2alxja2y7ngdhvz8gq4hwatk';

// --- the regtest environment ---------------------------------------------

const compose = (...args) =>
  execFileSync('docker', ['compose', ...args], { cwd: COMPOSE_DIR, encoding: 'utf8' }).trim();
const knots = (...args) =>
  compose('exec', '-T', 'knots', 'bitcoin-cli', '-regtest', '-rpcuser=lab', '-rpcpassword=lab', ...args);
const lncli = (node, ...args) => {
  const out = compose('exec', '-T', node, 'lncli', '--network=regtest', ...args);
  try {
    return JSON.parse(out);
  } catch {
    return out;
  }
};
const mine = (n = 1) => {
  knots('generatetoaddress', String(n), knots('-rpcwallet=boltz', 'getnewaddress'));
};
const height = () => Number(knots('getblockcount'));
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const status = async (id) => (await axios.get(`${API}/v2/swap/${id}`)).data;

const waitFor = async (what, fn, { timeoutMs = 120_000, mineEvery = 0 } = {}) => {
  const start = Date.now();
  let last;
  while (Date.now() - start < timeoutMs) {
    last = await fn();
    if (last) return last;
    if (mineEvery && (Date.now() - start) % mineEvery < 1500) mine(1);
    await sleep(1500);
  }
  throw new Error(`timed out waiting for ${what}`);
};

const waitStatus = async (id, wanted, opts) => {
  const set = new Set([wanted].flat());
  let seen;
  await waitFor(`swap ${id} to reach ${[...set].join('|')}`, async () => {
    seen = (await status(id)).status;
    return set.has(seen);
  }, opts);
  return seen;
};

const hasBlake2bBit = (invoice) => {
  const decoded = bolt11.decode(invoice);
  const features = decoded.tags.find((t) => t.tagName === 'feature_bits')?.data;
  const bits = features?.extra_bits?.bits ?? [];
  // bolt11 lists feature bits from 20 on in extra_bits; 512 is index 492.
  return Boolean(bits[512 - 20] || bits[513 - 20]);
};

const sweep = () =>
  compose(
    'exec', '-T', 'boltz', 'boltzr-cli',
    '--grpc-certificates', '/boltz/certificates',
    '--jwt-file', '/boltz/certificates/admin.jwt',
    'swap', 'sweep',
  );

// --- scenarios -------------------------------------------------------------

const results = [];
const scenario = (name, fn) => ({ name, fn });

const reverse = scenario('reverse', async () => {
  const amount = 50_000;
  const preimage = randomBytes(32);
  const keys = ECPair.makeRandom();

  const swap = (
    await axios.post(`${API}/v2/swap/reverse`, {
      invoiceAmount: amount,
      from: 'BTC',
      to: 'BTC',
      claimPublicKey: keys.publicKey.toString('hex'),
      preimageHash: crypto.sha256(preimage).toString('hex'),
    })
  ).data;
  if (!hasBlake2bBit(swap.invoice)) throw new Error('the hold invoice lacks feature bit 512');

  // The hold invoice settles only once we claim, so pay it in the background.
  const payer = spawn('docker', ['compose', 'exec', '-T', 'lnd-user', 'lncli', '--network=regtest',
    'payinvoice', '--force', '--timeout', '300s', swap.invoice], { cwd: COMPOSE_DIR, stdio: 'ignore' });

  await waitStatus(swap.id, ['transaction.mempool', 'transaction.confirmed']);
  const lockup = (await axios.get(`${API}/v2/swap/reverse/${swap.id}/transaction`)).data;
  const lockupTx = Transaction.fromHex(lockup.hex);

  const boltzPublicKey = Buffer.from(swap.refundPublicKey, 'hex');
  const musig = new Musig(zkp, keys, randomBytes(32), [boltzPublicKey, keys.publicKey]);
  const tweakedKey = TaprootUtils.tweakMusig(
    musig,
    SwapTreeSerializer.deserializeSwapTree(swap.swapTree).tree,
  );
  const swapOutput = detectSwap(tweakedKey, lockupTx);
  if (!swapOutput) throw new Error('no swap output in the lockup transaction');

  const destination = lncli('lnd-user', 'newaddress', 'p2tr').address;
  const claimTx = targetFee(2, (fee) =>
    constructClaimTransaction(
      [{ ...swapOutput, keys, preimage, cooperative: true, type: OutputType.Taproot, txHash: lockupTx.getHash() }],
      address.toOutputScript(destination, network),
      fee,
    ),
  );

  const boltzSig = (
    await axios.post(`${API}/v2/swap/reverse/${swap.id}/claim`, {
      index: 0,
      transaction: claimTx.toHex(),
      preimage: preimage.toString('hex'),
      pubNonce: Buffer.from(musig.getPublicNonce()).toString('hex'),
    })
  ).data;
  musig.aggregateNonces([[boltzPublicKey, Buffer.from(boltzSig.pubNonce, 'hex')]]);
  musig.initializeSession(
    claimTx.hashForWitnessV1(0, [swapOutput.script], [swapOutput.value], Transaction.SIGHASH_DEFAULT),
  );
  musig.addPartial(boltzPublicKey, Buffer.from(boltzSig.partialSignature, 'hex'));
  musig.signPartial();
  claimTx.ins[0].witness = [musig.aggregatePartials()];

  const { id: claimTxId } = (await axios.post(`${API}/v2/chain/BTC/transaction`, { hex: claimTx.toHex() })).data;
  mine(1);
  await waitStatus(swap.id, 'invoice.settled');
  payer.kill();

  // Looked up by id alone: the node is pruned without -txindex, so this
  // answer comes by way of the shim's index.
  const tx = (await axios.get(`${API}/v2/chain/BTC/transaction/${claimTxId}`)).data;
  if (!tx.hex) throw new Error('claim transaction not found by id');
  return `claimed ${swapOutput.value} sat in ${claimTxId.slice(0, 16)}…, invoice settled`;
});

const createSubmarine = async (invoice) => {
  const keys = ECPair.makeRandom();
  const swap = (
    await axios.post(`${API}/v2/swap/submarine`, {
      invoice,
      from: 'BTC',
      to: 'BTC',
      refundPublicKey: keys.publicKey.toString('hex'),
    })
  ).data;
  return { swap, keys };
};

const lockUp = (swap, amount = swap.expectedAmount) =>
  lncli('lnd-user', 'sendcoins', '--addr', swap.address, '--amt', String(amount), '--sat_per_vbyte', '2').txid;

const cooperateOnClaim = async (swap, keys, invoice) => {
  const details = (await axios.get(`${API}/v2/swap/submarine/${swap.id}/claim`)).data;
  const paymentHash = bolt11.decode(invoice).tags.find((t) => t.tagName === 'payment_hash').data;
  if (!crypto.sha256(Buffer.from(details.preimage, 'hex')).equals(Buffer.from(paymentHash, 'hex'))) {
    throw new Error('the service gave a preimage that does not match the invoice');
  }
  const boltzPublicKey = Buffer.from(swap.claimPublicKey, 'hex');
  const musig = new Musig(zkp, keys, randomBytes(32), [boltzPublicKey, keys.publicKey]);
  TaprootUtils.tweakMusig(musig, SwapTreeSerializer.deserializeSwapTree(swap.swapTree).tree);
  musig.aggregateNonces([[boltzPublicKey, Buffer.from(details.pubNonce, 'hex')]]);
  musig.initializeSession(Buffer.from(details.transactionHash, 'hex'));
  await axios.post(`${API}/v2/swap/submarine/${swap.id}/claim`, {
    pubNonce: Buffer.from(musig.getPublicNonce()).toString('hex'),
    partialSignature: Buffer.from(musig.signPartial()).toString('hex'),
  });
};

const submarine = scenario('submarine', async () => {
  const amount = 60_000;
  const invoice = lncli('lnd-user', 'addinvoice', '--amt', String(amount)).payment_request;
  const { swap, keys } = await createSubmarine(invoice);
  lockUp(swap);
  mine(1);
  // One confirmation is not enough to be paid.
  await sleep(4000);
  // The public status can still read transaction.mempool here; what matters
  // is that nothing has been paid.
  const early = (await status(swap.id)).status;
  if (['invoice.pending', 'invoice.paid', 'transaction.claim.pending', 'transaction.claimed'].includes(early)) {
    throw new Error(`paid at one confirmation: ${early}`);
  }
  const earlyHash = bolt11.decode(invoice).tags.find((t) => t.tagName === 'payment_hash').data;
  if (lncli('lnd-user', 'lookupinvoice', earlyHash).state === 'SETTLED') throw new Error('invoice settled at one confirmation');
  mine(LOCKUP_CONFIRMATIONS - 1);

  await waitStatus(swap.id, ['transaction.claim.pending', 'invoice.paid', 'transaction.claimed']);
  const inv = lncli('lnd-user', 'lookupinvoice', bolt11.decode(invoice).tags.find((t) => t.tagName === 'payment_hash').data);
  if (inv.state !== 'SETTLED') throw new Error(`invoice state ${inv.state}`);

  // Claims are batched (deferredClaimSymbols); sweep now instead of waiting.
  if ((await status(swap.id)).status === 'invoice.paid') {
    sweep();
  }
  const s = await waitStatus(swap.id, ['transaction.claim.pending', 'transaction.claimed']);
  if (s === 'transaction.claim.pending') {
    await cooperateOnClaim(swap, keys, invoice);
  }
  mine(1);
  await waitStatus(swap.id, 'transaction.claimed', { mineEvery: 10_000 });
  return `paid ${amount} sat over Lightning for ${swap.expectedAmount} sat on chain`;
});

const refusesSha256Invoice = scenario('refuses-sha256-invoice', async () => {
  try {
    await createSubmarine(SHA256_CHAIN_INVOICE);
  } catch (err) {
    const msg = err.response?.data?.error ?? String(err);
    if (/BLAKE2b/i.test(msg)) return `refused: "${msg.slice(0, 90)}…"`;
    throw new Error(`refused, but not for the chain: ${msg}`);
  }
  throw new Error('a SHA256-chain invoice was accepted');
});

const refundSetup = async (amount) => {
  // A hold invoice we cancel before the service can pay it.
  const preimage = randomBytes(32);
  const hash = crypto.sha256(preimage).toString('hex');
  const invoice = lncli('lnd-user', 'addholdinvoice', hash, '--amt', String(amount)).payment_request;
  const { swap, keys } = await createSubmarine(invoice);
  lncli('lnd-user', 'cancelinvoice', hash);
  return { swap, keys };
};

const findLockup = async (swap, keys, lockupTxId) => {
  const hex = (await axios.get(`${API}/v2/chain/BTC/transaction/${lockupTxId}`)).data.hex;
  const lockupTx = Transaction.fromHex(hex);
  const boltzPublicKey = Buffer.from(swap.claimPublicKey, 'hex');
  const musig = new Musig(zkp, keys, randomBytes(32), [boltzPublicKey, keys.publicKey]);
  const tree = SwapTreeSerializer.deserializeSwapTree(swap.swapTree);
  const tweakedKey = TaprootUtils.tweakMusig(musig, tree.tree);
  const swapOutput = detectSwap(tweakedKey, lockupTx);
  if (!swapOutput) throw new Error('lockup output not found');
  return { lockupTx, swapOutput, musig, boltzPublicKey, tree, internalKey: musig.getAggregatedPublicKey() };
};

const cooperativeRefund = scenario('cooperative-refund', async () => {
  const { swap, keys } = await refundSetup(40_000);
  const lockupTxId = lockUp(swap);
  mine(LOCKUP_CONFIRMATIONS);
  await waitStatus(swap.id, 'invoice.failedToPay', { timeoutMs: 180_000 });

  const { lockupTx, swapOutput, musig, boltzPublicKey } = await findLockup(swap, keys, lockupTxId);
  const destination = lncli('lnd-user', 'newaddress', 'p2tr').address;
  const refundTx = targetFee(2, (fee) =>
    constructRefundTransaction(
      [{ ...swapOutput, keys, cooperative: true, type: OutputType.Taproot, txHash: lockupTx.getHash() }],
      address.toOutputScript(destination, network),
      0,
      fee,
      true,
    ),
  );
  const sig = (
    await axios.post(`${API}/v2/swap/submarine/${swap.id}/refund`, {
      index: 0,
      transaction: refundTx.toHex(),
      pubNonce: Buffer.from(musig.getPublicNonce()).toString('hex'),
    })
  ).data;
  musig.aggregateNonces([[boltzPublicKey, Buffer.from(sig.pubNonce, 'hex')]]);
  musig.initializeSession(
    refundTx.hashForWitnessV1(0, [swapOutput.script], [swapOutput.value], Transaction.SIGHASH_DEFAULT),
  );
  musig.addPartial(boltzPublicKey, Buffer.from(sig.partialSignature, 'hex'));
  musig.signPartial();
  refundTx.ins[0].witness = [musig.aggregatePartials()];
  const { id } = (await axios.post(`${API}/v2/chain/BTC/transaction`, { hex: refundTx.toHex() })).data;
  mine(1);
  return `refunded ${swapOutput.value} sat cooperatively in ${id.slice(0, 16)}…`;
});

const timeoutRefund = scenario('timeout-refund', async () => {
  const { swap, keys } = await refundSetup(30_000);
  // Underpay, so the lockup is refused outright.
  const lockupTxId = lockUp(swap, swap.expectedAmount - 1000);
  mine(1);
  await waitStatus(swap.id, ['transaction.lockupFailed', 'invoice.failedToPay']);

  const { lockupTx, swapOutput, tree, internalKey } = await findLockup(swap, keys, lockupTxId);
  // Past the timeout, a hundred blocks at a time with a pause between, so
  // the service follows block by block as it would on a live chain.
  const toMine = swap.timeoutBlockHeight - height() + 1;
  for (let left = toMine; left > 0; left -= 100) {
    mine(Math.min(100, left));
    await sleep(3000);
  }

  const destination = lncli('lnd-user', 'newaddress', 'p2tr').address;
  const refundTx = targetFee(2, (fee) =>
    constructRefundTransaction(
      [{ ...swapOutput, keys, cooperative: false, type: OutputType.Taproot, txHash: lockupTx.getHash(), swapTree: tree, internalKey }],
      address.toOutputScript(destination, network),
      swap.timeoutBlockHeight,
      fee,
      true,
    ),
  );
  const accept = JSON.parse(knots('testmempoolaccept', JSON.stringify([refundTx.toHex()])))[0];
  if (!accept.allowed) throw new Error(`the node refuses the timeout refund: ${accept['reject-reason']}`);
  const { id } = (await axios.post(`${API}/v2/chain/BTC/transaction`, { hex: refundTx.toHex() })).data;
  mine(1);
  return `refunded ${swapOutput.value} sat by the timeout path in ${id.slice(0, 16)}… (mined ${Math.max(toMine, 0)} blocks)`;
});

const rescan = scenario('rescan-after-downtime', async () => {
  const amount = 25_000;
  const invoice = lncli('lnd-user', 'addinvoice', '--amt', String(amount)).payment_request;
  const { swap } = await createSubmarine(invoice);
  compose('stop', 'boltz');
  try {
    lockUp(swap);
    mine(3); // confirmed in v2-header blocks the service has not seen
  } finally {
    compose('start', 'boltz');
  }
  await waitFor('the API after restart', async () => {
    try {
      await axios.get(`${API}/version`);
      return true;
    } catch {
      return false;
    }
  });
  await waitStatus(swap.id, ['invoice.paid', 'transaction.claim.pending', 'transaction.claimed'], { timeoutMs: 180_000 });
  return 'lockup confirmed while the service was down was found and paid';
});

// The chain's temporary data rules forbid OP_IF in tapscript. A leaf that
// needs no signature and uses OP_IF with a minimal argument must be refused;
// otherwise the swap scenarios did not run under the rules mainnet has. Knots
// reports the ban with the MINIMALIF error, which without the rules this
// argument (OP_1) would satisfy.
const rdtsActive = scenario('rdts-rules-in-force', async () => {
  const leaf = bscript.fromASM('OP_1 OP_IF OP_1 OP_ENDIF');
  const internalPubkey = Buffer.from(ECPair.makeRandom().publicKey.subarray(1));
  const scriptTree = { output: leaf };
  const p2tr = payments.p2tr({ internalPubkey, scriptTree, redeem: { output: leaf }, network });
  const fundTxid = knots('-rpcwallet=boltz', 'sendtoaddress', p2tr.address, '0.001');
  const fund = Transaction.fromHex(knots('-rpcwallet=boltz', 'gettransaction', fundTxid).match(/"hex": "([0-9a-f]+)"/)[1]);
  const vout = fund.outs.findIndex((o) => o.script.equals(p2tr.output));
  mine(1);

  const spend = new Transaction();
  spend.version = 2;
  spend.addInput(fund.getHash(), vout);
  spend.addOutput(p2tr.output, 90_000);
  spend.ins[0].witness = [leaf, p2tr.witness.at(-1)];
  const accept = JSON.parse(knots('testmempoolaccept', JSON.stringify([spend.toHex()])))[0];
  if (accept.allowed) throw new Error('a tapscript using OP_IF was accepted: the data rules are not in force');
  return `OP_IF in tapscript refused: ${accept['reject-reason']}`;
});

// A miner pays a swap's lockup address from their coinbase. The service must
// not pay the invoice: it could not claim the coin before the swap times out.
const coinbaseLockup = scenario('coinbase-lockup-refused', async () => {
  const amount = 12_000;
  const invoice = lncli('lnd-user', 'addinvoice', '--amt', String(amount)).payment_request;
  const { swap } = await createSubmarine(invoice);
  // The regtest subsidy is spent by now; a high-fee transaction in the same
  // block makes the coinbase large enough to cover the lockup.
  const feeTx = knots('-rpcwallet=boltz', '-named', 'sendtoaddress',
    `address=${knots('-rpcwallet=boltz', 'getnewaddress')}`, 'amount=0.001', 'fee_rate=500');
  // generatetoaddress mines the mempool with the fees in the coinbase (this
  // node's generateblock leaves them out).
  if (!JSON.parse(knots('getrawmempool')).includes(feeTx)) throw new Error('fee transaction not in the mempool');
  const minedBlock = JSON.parse(knots('generatetoaddress', '1', swap.address))[0];
  const coinbase = JSON.parse(knots('getblock', minedBlock, '2')).tx[0];
  const paid = coinbase.vout.find((o) => o.scriptPubKey.address === swap.address)?.value ?? 0;
  if (Math.round(paid * 1e8) < swap.expectedAmount) {
    throw new Error(`the coinbase paid ${paid} BTC, less than the ${swap.expectedAmount} sat expected: the test is not testing anything`);
  }
  const s = await waitStatus(swap.id, ['transaction.lockupFailed', 'invoice.pending', 'invoice.paid', 'transaction.claimed'], { timeoutMs: 60_000 });
  const hash = bolt11.decode(invoice).tags.find((t) => t.tagName === 'payment_hash').data;
  const state = lncli('lnd-user', 'lookupinvoice', hash).state;
  if (s !== 'transaction.lockupFailed' || state === 'SETTLED') {
    throw new Error(`coinbase lockup of ${Math.round(paid * 1e8)} sat: swap ${s}, invoice ${state}`);
  }
  const reason = (await status(swap.id)).failureReason;
  return `coinbase paying ${Math.round(paid * 1e8)} sat refused (${reason}); invoice ${state}`;
});

// Probe for the security review's C1: does lnd cancel a paid hold invoice
// while the lockup can still be claimed with the preimage? Not part of the
// default run: it passes when the service is safe.
const lateCancel = scenario('late-cancel-probe', async () => {
  const preimage = randomBytes(32);
  const keys = ECPair.makeRandom();
  const swap = (
    await axios.post(`${API}/v2/swap/reverse`, {
      invoiceAmount: 40_000, from: 'BTC', to: 'BTC',
      claimPublicKey: keys.publicKey.toString('hex'),
      preimageHash: crypto.sha256(preimage).toString('hex'),
    })
  ).data;
  const created = height();
  const payer = spawn('docker', ['compose', 'exec', '-T', 'lnd-user', 'lncli', '--network=regtest',
    'payinvoice', '--force', '--timeout', '600s', swap.invoice], { cwd: COMPOSE_DIR, stdio: 'ignore' });
  await waitStatus(swap.id, ['transaction.mempool', 'transaction.confirmed']);
  const lockup = Transaction.fromHex((await axios.get(`${API}/v2/swap/reverse/${swap.id}/transaction`)).data.hex);
  const hash = crypto.sha256(preimage).toString('hex');
  const htlcExpiry = Math.min(...lncli('lnd-swap', 'lookupinvoice', hash).htlcs.map((h) => Number(h.expiry_height)));
  let cancelledAt;
  for (let h = height(); h < swap.timeoutBlockHeight + 5; h = height()) {
    mine(1);
    await sleep(1200);
    const state = lncli('lnd-swap', 'lookupinvoice', hash).state;
    if (state === 'CANCELED') { cancelledAt = height(); break; }
  }
  payer.kill();
  const report = `created ${created}, lockup timeout ${swap.timeoutBlockHeight}, HTLC expiry ${htlcExpiry}, invoice cancelled at ${cancelledAt ?? 'never (by timeout+5)'}`;
  if (cancelledAt === undefined || cancelledAt >= swap.timeoutBlockHeight) return report + ': cancel not before the timeout';

  // Cancelled while the lockup is still claimable: try the script path.
  const boltzPublicKey = Buffer.from(swap.refundPublicKey, 'hex');
  const musig = new Musig(zkp, keys, randomBytes(32), [boltzPublicKey, keys.publicKey]);
  const tree = SwapTreeSerializer.deserializeSwapTree(swap.swapTree);
  const swapOutput = detectSwap(TaprootUtils.tweakMusig(musig, tree.tree), lockup);
  const destination = lncli('lnd-user', 'newaddress', 'p2tr').address;
  const claimTx = targetFee(2, (fee) => constructClaimTransaction(
    [{ ...swapOutput, keys, preimage, cooperative: false, type: OutputType.Taproot, txHash: lockup.getHash(), swapTree: tree, internalKey: musig.getAggregatedPublicKey() }],
    address.toOutputScript(destination, network), fee));
  const accept = JSON.parse(knots('testmempoolaccept', JSON.stringify([claimTx.toHex()])))[0];
  throw new Error(`${report}; claim by script path after the cancel: ${accept.allowed ? 'ACCEPTED — the attack works' : 'refused: ' + accept['reject-reason']}`);
});

// --- run -------------------------------------------------------------------

// The two nodes' channels are private, so after a restart neither knows
// where to find the other: reconnect them and wait for both channels.
const ensureChannels = async () => {
  const swapPub = lncli('lnd-swap', 'getinfo').identity_pubkey;
  if (lncli('lnd-user', 'listpeers').peers.length === 0) {
    try {
      lncli('lnd-user', 'connect', `${swapPub}@lnd-swap:9735`);
    } catch {
      // already connecting
    }
  }
  await waitFor('both channels active', async () =>
    lncli('lnd-user', 'listchannels').channels.filter((c) => c.active).length === 2);
};
await ensureChannels();

const all = [rdtsActive, coinbaseLockup, reverse, submarine, refusesSha256Invoice, cooperativeRefund, timeoutRefund, rescan];
const wanted = process.argv.slice(2);
const probes = [lateCancel];
const chosen = wanted.length ? [...all, ...probes].filter((s) => wanted.includes(s.name)) : all;

for (const s of chosen) {
  const started = Date.now();
  try {
    const detail = await s.fn();
    results.push([s.name, 'PASS', detail]);
  } catch (err) {
    const detail = err.response?.data ? JSON.stringify(err.response.data) : err.stack ?? String(err);
    results.push([s.name, 'FAIL', detail]);
  }
  const [name, verdict, detail] = results.at(-1);
  console.log(`${verdict} ${name} (${((Date.now() - started) / 1000).toFixed(0)} s): ${detail}`);
}

process.exit(results.every(([, v]) => v === 'PASS') ? 0 : 1);
