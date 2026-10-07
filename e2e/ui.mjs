// Swaps through the real web app in a browser, served by the production
// nginx config (deploy/regtest/ui-nginx.sh) in front of the regtest service
// (deploy/regtest). Fails on any Content-Security-Policy violation or page
// error, so it also checks the policy against the app.
//
//   cd webapp && bun run regtest && \
//     VITE_API_URL=https://localhost:18443 npx vite build --outDir /tmp/lfs-ui-dist
//   deploy/regtest/ui-nginx.sh up /tmp/lfs-ui-dist
//   cd e2e && node ui.mjs            all scenarios
//   node ui.mjs reverse               one by name
//
// Chrome (channel "chrome") must be installed; screenshots go to
// $UI_SHOTS (default /tmp/lfswap-ui).

import { execFileSync } from 'node:child_process';
import { mkdirSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright-core';

const SITE = process.env.SITE ?? 'https://localhost:18443';
const SHOTS = process.env.UI_SHOTS ?? '/tmp/lfswap-ui';
const COMPOSE_DIR = join(dirname(fileURLToPath(import.meta.url)), '..', 'deploy', 'regtest');
mkdirSync(SHOTS, { recursive: true });

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
const mine = (n = 1) => knots('generatetoaddress', String(n), knots('-rpcwallet=boltz', 'getnewaddress'));
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const api = async (path) => {
  const res = await fetch(`${SITE}${path}`);
  return res.json();
};
const swapStatus = async (id) => (await api(`/v2/swap/${id}`)).status;

const waitFor = async (what, fn, { timeoutMs = 120_000, mineEvery = 0 } = {}) => {
  const start = Date.now();
  let lastMine = start;
  let last;
  while (Date.now() - start < timeoutMs) {
    last = await fn();
    if (last) return last;
    if (mineEvery && Date.now() - lastMine > mineEvery) {
      mine(1);
      lastMine = Date.now();
    }
    await sleep(1000);
  }
  throw new Error(`timed out waiting for ${what} (last: ${JSON.stringify(last)})`);
};

// Coins received by an address of a node, confirmed or not
const receivedBy = (address) => {
  const utxos = JSON.parse(knots('scantxoutset', 'start', JSON.stringify([`addr(${address})`])));
  return Math.round(utxos.total_amount * 1e8);
};

// --- the browser -----------------------------------------------------------

process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0'; // the harness's own certificate

const browser = await chromium.launch({
  channel: 'chrome',
  headless: true,
  args: ['--ignore-certificate-errors'],
});

const newPage = async () => {
  const context = await browser.newContext({
    ignoreHTTPSErrors: true,
    acceptDownloads: true,
    viewport: { width: 1280, height: 1000 },
  });
  const problems = [];
  await context.addInitScript(() => {
    document.addEventListener('securitypolicyviolation', (e) =>
      console.error(`CSP violation: ${e.violatedDirective} ${e.blockedURI}`),
    );
  });
  const page = await context.newPage();
  page.on('console', (m) => {
    // A service worker cannot register over the harness's own certificate
    if (m.type() === 'error' && !/service.?worker|ERR_FAILED/i.test(m.text())) {
      problems.push(m.text());
    }
  });
  page.on('pageerror', (e) => problems.push(`page error: ${e.message}`));
  return { context, page, problems };
};

let shot = 0;
const screenshot = async (page, name) => {
  shot += 1;
  await page.screenshot({ path: join(SHOTS, `${String(shot).padStart(2, '0')}-${name}.png`), fullPage: true });
};

// With UI_DEBUG set, what is on screen, for writing a scenario
const dump = async (page, name) => {
  if (!process.env.UI_DEBUG) return;
  await screenshot(page, `debug-${name}`);
  const lines = await page.$$eval('button, input, textarea, a[href], [data-testid], h2, h3, p, span', (els) =>
    els
      .filter((e) => e.offsetParent !== null)
      .map((e) => `${e.tagName} [${e.getAttribute('data-testid') ?? ''}] #${e.id ?? ''} ${(e.textContent ?? '').trim().replace(/\s+/g, ' ').slice(0, 70)}`),
  );
  console.log(`== ${name} ${page.url()}\n${lines.join('\n')}`);
};

const assertNoProblems = (problems, scenario) => {
  const csp = problems.filter((p) => p.includes('CSP violation'));
  if (csp.length > 0) {
    throw new Error(`${scenario}: ${csp.join('; ')}`);
  }
  const errors = problems.filter((p) => p.startsWith('page error'));
  if (errors.length > 0) {
    throw new Error(`${scenario}: ${errors.join('; ')}`);
  }
};

const swapIdOf = (page) => {
  const match = new URL(page.url()).pathname.match(/^\/swap\/([A-Za-z0-9]+)/);
  if (!match) throw new Error(`not on a swap page: ${page.url()}`);
  return match[1];
};

// --- scenarios ---------------------------------------------------------------

const scenarios = {};

// Lightning to chain: the app shows the hold invoice, the user's node pays
// it, the service locks up, and the app claims to the user's address
scenarios.reverse = async () => {
  const { page, problems } = await newPage();
  const address = lncli('lnd-user', 'newaddress', 'p2tr').address;

  await page.goto(`${SITE}/`, { waitUntil: 'networkidle' });
  await page.fill('#sendAmount', '50000');
  await page.fill('#onchainAddress', address);
  const created = page.waitForResponse(
    (r) => r.url().endsWith('/v2/swap/reverse') && r.request().method() === 'POST',
  );
  await page.click('#create-swap-button');
  const swap = await (await created).json();
  await page.waitForURL(/\/swap\//);
  const id = swapIdOf(page);
  if (id !== swap.id) throw new Error(`page shows ${id}, the API created ${swap.id}`);
  await page.getByText('Pay this invoice').waitFor();
  await screenshot(page, 'reverse-created');

  const payment = JSON.parse(
    compose('exec', '-T', 'lnd-user', 'lncli', '--network=regtest', 'payinvoice', '--force', '--json', swap.invoice),
  );
  if (payment.status !== 'SUCCEEDED') throw new Error(`payment ${payment.status}`);

  // The app claims as soon as the lockup is seen
  const claimed = await waitFor('the claim to reach the address', () => {
    mine(1);
    return receivedBy(address);
  }, { timeoutMs: 60_000 });
  await page.getByText('Congratulations!').waitFor();
  await screenshot(page, 'reverse-done');
  assertNoProblems(problems, 'reverse');
  return `claimed ${claimed} sat to ${address.slice(0, 16)}… after paying ${swap.invoice.slice(0, 16)}…`;
};

// Creates a submarine swap in the app for an invoice of the user's node,
// going through the rescue key backup; returns the swap the API created
const createSubmarine = async (page, sendAmount, invoiceSat, keyFile) => {
  const invoice = lncli('lnd-user', 'addinvoice', '--amt', String(invoiceSat)).payment_request;
  await page.goto(`${SITE}/`, { waitUntil: 'networkidle' });
  await page.click('#flip-assets');
  await page.fill('#sendAmount', String(sendAmount));
  await page.fill('#invoice', invoice);
  const created = page.waitForResponse(
    (r) => r.url().endsWith('/v2/swap/submarine') && r.request().method() === 'POST',
  );
  await page.click('#create-swap-button');
  const swap = await (await created).json();

  // A browser that has made a swap before has its rescue key already
  const button = page.getByText('Download rescue key');
  const payScreen = page.locator('a[href^="bitcoin:"]').first();
  const first = await Promise.race([
    button.waitFor().then(() => 'key'),
    payScreen.waitFor().then(() => 'pay'),
  ]);
  if (first === 'key') {
    const download = page.waitForEvent('download');
    await button.click();
    await (await download).saveAs(keyFile);
    await page.setInputFiles('#rescueFileUpload', keyFile);
  }
  await payScreen.waitFor();
  return { swap, invoice };
};

const bip21Of = async (page) =>
  page.locator('a[href^="bitcoin:"]').first().getAttribute('href');

// Chain to Lightning: the app shows where to send, the user's wallet pays
// on chain, the service pays the invoice once the lockup is deep enough
scenarios.submarine = async () => {
  const { page, problems } = await newPage();
  const { swap } = await createSubmarine(page, 60_500, 60_000, join(SHOTS, 'rescue-key.json'));
  await screenshot(page, 'submarine-created');

  // The link and QR code are built by the app from what it checked
  const bip21 = await bip21Of(page);
  const amount = (swap.expectedAmount / 1e8).toFixed(8).replace(/0+$/, '');
  if (bip21 !== `bitcoin:${swap.address}?amount=${amount}`) {
    throw new Error(`unexpected BIP21 on the page: ${bip21}`);
  }

  knots('-rpcwallet=boltz', 'sendtoaddress', swap.address, (swap.expectedAmount / 1e8).toFixed(8));
  await waitFor('the payment', async () => {
    const status = await swapStatus(swap.id);
    return ['transaction.claimed', 'invoice.paid', 'transaction.claim.pending'].includes(status) && status;
  }, { mineEvery: 3000 });
  await page.getByText(/successfully|Congratulations/i).first().waitFor({ timeout: 30_000 });
  await screenshot(page, 'submarine-done');
  assertNoProblems(problems, 'submarine');
  return `paid ${swap.expectedAmount} sat on chain through the app (${bip21.slice(0, 24)}…)`;
};

// A lockup the service refuses (too little): the app offers a refund, which
// the service co-signs, to an address of the user's choosing
scenarios.refund = async () => {
  const { page, problems } = await newPage();
  const { swap } = await createSubmarine(page, 40_500, 40_000, join(SHOTS, 'rescue-key.json'));
  knots('-rpcwallet=boltz', 'sendtoaddress', swap.address, ((swap.expectedAmount - 5000) / 1e8).toFixed(8));
  await waitFor('the lockup to be refused', async () => {
    const status = await swapStatus(swap.id);
    return status === 'transaction.lockupFailed' && status;
  }, { mineEvery: 3000 });
  await page.getByText('Lockup Failed!').waitFor();
  await screenshot(page, 'refund-offered');

  const address = lncli('lnd-user', 'newaddress', 'p2tr').address;
  await page.fill('#refundAddress', address);
  await page.click('[data-testid=refundButton]');
  const refunded = await waitFor('the refund to reach the address', () => {
    mine(1);
    return receivedBy(address);
  }, { timeoutMs: 60_000 });
  await page.waitForTimeout(1500);
  await dump(page, 'refund-done');
  await screenshot(page, 'refund-done');
  assertNoProblems(problems, 'refund');

  const locked = swap.expectedAmount - 5000;
  if (refunded >= locked || refunded < locked - 3000) {
    throw new Error(`refunded ${refunded} sat of ${locked}`);
  }
  return `refunded ${refunded} of ${locked} sat through the app's refund form`;
};

// A browser with nothing but the rescue key file finds a failed swap made in
// another browser and refunds it
scenarios.rescue = async () => {
  const keyFile = join(SHOTS, 'rescue-key-rescue.json');
  let failedId;
  let failedLocked;
  {
    const { page, context } = await newPage();
    const { swap } = await createSubmarine(page, 30_500, 30_000, keyFile);
    failedId = swap.id;
    failedLocked = swap.expectedAmount - 4000;
    knots('-rpcwallet=boltz', 'sendtoaddress', swap.address, (failedLocked / 1e8).toFixed(8));
    await waitFor('the lockup to be refused', async () => {
      const status = await swapStatus(swap.id);
      return status === 'transaction.lockupFailed' && status;
    }, { mineEvery: 3000 });
    await context.close();
  }

  const { page, problems } = await newPage();
  await page.goto(`${SITE}/rescue`, { waitUntil: 'networkidle' });
  await page.setInputFiles('#refundUpload', keyFile);
  await page.getByRole('button', { name: 'Rescue', exact: true }).click();
  const item = page.locator(`[data-testid=swaplist-item-${failedId}]`);
  await item.waitFor({ timeout: 30_000 });
  await screenshot(page, 'rescue-listed');
  await item.getByText('Refund').click();
  await page.locator('#refundAddress').waitFor();
  await dump(page, 'rescue-refund');

  const address = lncli('lnd-user', 'newaddress', 'p2tr').address;
  await page.fill('#refundAddress', address);
  await page.click('[data-testid=refundButton]');
  const refunded = await waitFor('the refund to reach the address', () => {
    mine(1);
    return receivedBy(address);
  }, { timeoutMs: 60_000 });
  await page.waitForTimeout(1500);
  await screenshot(page, 'rescue-done');
  assertNoProblems(problems, 'rescue');
  // At most the app's fee limit for a small swap
  if (refunded >= failedLocked || refunded < failedLocked - 3000) {
    throw new Error(`refunded ${refunded} sat of ${failedLocked}`);
  }
  return `refunded ${refunded} of ${failedLocked} sat of swap ${failedId} from the rescue page, with only the key file`;
};

// --- run -------------------------------------------------------------------

const only = process.argv.slice(2);
let failed = 0;
for (const [name, run] of Object.entries(scenarios)) {
  if (only.length > 0 && !only.includes(name)) continue;
  const start = Date.now();
  try {
    const detail = await run();
    console.log(`PASS ${name} (${Math.round((Date.now() - start) / 1000)} s): ${detail}`);
  } catch (error) {
    failed += 1;
    console.log(`FAIL ${name}: ${error.stack ?? error}`);
  }
}
await browser.close();
process.exit(failed === 0 ? 0 : 1);
