// Drives the idxa interface in a real headless Chromium over the DevTools
// protocol: clicks through every view, records console errors, and saves
// screenshots. This is the only way to know the interface actually works.
import { writeFileSync } from 'node:fs';

const BASE = process.env.IDXA_URL || 'http://127.0.0.1:8787';
const DEBUG_PORT = process.env.CDP_PORT || 9222;
const OUT = process.env.OUT_DIR || './.browser-test-out';

const targets = await (await fetch(`http://127.0.0.1:${DEBUG_PORT}/json/list`)).json();
const page = targets.find((t) => t.type === 'page');
if (!page) throw new Error('no page target; is the browser running with --remote-debugging-port?');

const ws = new WebSocket(page.webSocketDebuggerUrl);
await new Promise((resolve, reject) => {
  ws.onopen = resolve;
  ws.onerror = (e) => reject(new Error('cdp connect failed: ' + e.message));
});

let nextId = 1;
const pending = new Map();
const consoleErrors = [];
const pageErrors = [];

ws.onmessage = (event) => {
  const msg = JSON.parse(event.data);
  if (msg.id && pending.has(msg.id)) {
    const { resolve, reject } = pending.get(msg.id);
    pending.delete(msg.id);
    if (msg.error) reject(new Error(JSON.stringify(msg.error)));
    else resolve(msg.result);
    return;
  }
  if (msg.method === 'Runtime.consoleAPICalled' && msg.params.type === 'error') {
    consoleErrors.push(msg.params.args.map((a) => a.value || a.description || '').join(' '));
  }
  if (msg.method === 'Runtime.exceptionThrown') {
    const d = msg.params.exceptionDetails;
    pageErrors.push(`${d.text} ${d.exception?.description || ''}`.trim());
  }
  if (msg.method === 'Log.entryAdded' && msg.params.entry.level === 'error') {
    consoleErrors.push(msg.params.entry.text);
  }
};

function send(method, params = {}) {
  const id = nextId++;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    ws.send(JSON.stringify({ id, method, params }));
  });
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function evaluate(expression) {
  const res = await send('Runtime.evaluate', {
    expression,
    returnByValue: true,
    awaitPromise: true,
  });
  if (res.exceptionDetails) {
    throw new Error(res.exceptionDetails.text + ' ' +
      (res.exceptionDetails.exception?.description || ''));
  }
  return res.result.value;
}

async function screenshot(name) {
  const { data } = await send('Page.captureScreenshot', { format: 'png' });
  const path = `${OUT}/idxa-ui-${name}.png`;
  writeFileSync(path, Buffer.from(data, 'base64'));
  return path;
}

const results = [];
function check(name, ok, detail = '') {
  results.push({ name, ok, detail });
  console.log(`  [${ok ? 'PASS' : 'FAIL'}] ${name}${!ok && detail ? ' — ' + detail : ''}`);
}

await send('Runtime.enable');
await send('Log.enable');
await send('Page.enable');
await send('Emulation.setDeviceMetricsOverride', {
  width: 1440, height: 900, deviceScaleFactor: 2, mobile: false,
});

async function goto(url) {
  await send('Page.navigate', { url });
  await sleep(2500);
}

// ---------------------------------------------------------------- overview

console.log('\n== overview ==');
await goto(BASE + '/');

check('page title set', (await evaluate('document.title')).includes('idxa'));
check('sidebar lists databases',
  (await evaluate('document.querySelectorAll(".cluster-item").length')) > 0);
check('each row carries a finding',
  (await evaluate('document.querySelector(".cluster-sub")?.textContent.trim().length > 0')));
check('overview table rendered',
  (await evaluate('document.querySelectorAll("#view table tbody tr").length')) > 0);
check('modal is hidden at rest',
  (await evaluate('getComputedStyle(document.getElementById("modal-backdrop")).display')) === 'none');
console.log('  screenshot: ' + await screenshot('1-overview'));

// ------------------------------------------------------------------ modal

console.log('\n== add database dialog ==');
await evaluate('document.getElementById("add-database").click()');
await sleep(500);
check('dialog opens',
  (await evaluate('getComputedStyle(document.getElementById("modal-backdrop")).display')) !== 'none');
check('connection field present',
  await evaluate('!!document.querySelector("#modal-body input[placeholder*=postgres]")'));
check('test button present',
  await evaluate('[...document.querySelectorAll("#modal-body button")].some(b => b.textContent.includes("Test"))'));
console.log('  screenshot: ' + await screenshot('2-add-dialog'));

// The Test button must give a verdict before anything is saved.
await evaluate(`
  document.querySelector('#cluster-name').value = 'Probe';
  const dsn = document.querySelector('#modal-body input[placeholder*=postgres]');
  dsn.value = 'postgres://idxa:idxa@localhost:5499/idxa';
  [...document.querySelectorAll('#modal-body button')].find(b => b.textContent.includes('Test')).click();
`);
await sleep(3000);
const verdict = await evaluate('document.querySelector("#modal-body .notice")?.textContent || ""');
check('test connection reports a verdict', verdict.length > 0, verdict.slice(0, 80));
check('verdict names the server version', /PostgreSQL/.test(verdict), verdict.slice(0, 120));
console.log('  screenshot: ' + await screenshot('3-connection-tested'));

await evaluate('document.getElementById("modal-close").click()');
await sleep(400);
check('dialog closes',
  (await evaluate('getComputedStyle(document.getElementById("modal-backdrop")).display')) === 'none');

// ----------------------------------------------------------------- a database

console.log('\n== database view ==');
await evaluate('document.querySelector(".cluster-item").click()');
await sleep(3000);

check('navigated to a database', (await evaluate('location.hash')).startsWith('#/db/'));
check('five tabs shown', (await evaluate('document.querySelectorAll(".tab").length')) === 5);
check('read-only badge present',
  await evaluate('[...document.querySelectorAll(".pill")].some(p => p.textContent.includes("read only"))'));
check('run button present', await evaluate('!!document.getElementById("run-analysis")'));

// Wait for a couple of live ticks so the table has real numbers.
await sleep(7000);
const liveRows = await evaluate('document.querySelectorAll(".live-table tbody tr").length');
check('live table has rows', liveRows > 0, `rows=${liveRows}`);
const tiles = await evaluate('[...document.querySelectorAll(".tile-value")].map(t=>t.textContent).join(" | ")');
check('tiles show values', !/^(—\s*\|?\s*)+$/.test(tiles), tiles);
check('cost of idxa is disclosed',
  await evaluate('[...document.querySelectorAll("#view .hint")].some(h => h.textContent.includes("idxa has used"))'));
console.log('  live tiles: ' + tiles);
console.log('  screenshot: ' + await screenshot('4-live'));

// -------------------------------------------------------------------- tabs

console.log('\n== tabs ==');
for (const [label, file] of [['Recommendations', '5-recommendations'],
  ['Unused', '6-unused'], ['Redundant', '7-redundant'], ['Settings', '8-settings']]) {
  await evaluate(`[...document.querySelectorAll('.tab')].find(t => t.textContent.startsWith(${JSON.stringify(label)})).click()`);
  await sleep(2000);
  const content = await evaluate('document.getElementById("tab-panel")?.textContent.trim().length || 0');
  check(`${label} renders content`, content > 0, `chars=${content}`);
  console.log('  screenshot: ' + await screenshot(file));
}

// Expand a recommendation to confirm the detail panel works.
await evaluate(`[...document.querySelectorAll('.tab')].find(t => t.textContent.startsWith('Recommendations')).click()`);
await sleep(1500);
const hasExpander = await evaluate('!!document.querySelector(".expander")');
if (hasExpander) {
  await evaluate('document.querySelector(".expander").click()');
  await sleep(600);
  check('recommendation detail expands',
    await evaluate('!!document.querySelector(".rec-detail pre.sql")'));
  check('detail explains the reasoning',
    await evaluate('document.querySelector(".rec-detail")?.textContent.includes("Why")'));
  console.log('  screenshot: ' + await screenshot('9-recommendation-detail'));
} else {
  check('recommendation rows present', false, 'no expander found');
}

// ------------------------------------------------------------------ mobile

console.log('\n== narrow window ==');
await send('Emulation.setDeviceMetricsOverride', {
  width: 390, height: 844, deviceScaleFactor: 2, mobile: true,
});

await sleep(1200);
const sidebarVisible = await evaluate('getComputedStyle(document.querySelector(".sidebar")).display !== "none"');
const navReachable = await evaluate(`
  getComputedStyle(document.querySelector('.sidebar')).display !== 'none' ||
  !!document.querySelector('[data-mobile-nav]')
`);
check('navigation reachable on a narrow screen', navReachable,
  `sidebar visible=${sidebarVisible}`);
console.log('  screenshot: ' + await screenshot('10-narrow'));

await send('Emulation.setDeviceMetricsOverride', {
  width: 1440, height: 900, deviceScaleFactor: 2, mobile: false,
});

// ------------------------------------------------------------------ errors

console.log('\n== javascript errors ==');
if (pageErrors.length) {
  for (const e of pageErrors) console.log('  EXCEPTION: ' + e.split('\n')[0]);
} else {
  console.log('  no uncaught exceptions');
}
if (consoleErrors.length) {
  for (const e of consoleErrors.slice(0, 10)) console.log('  console error: ' + e.slice(0, 160));
} else {
  console.log('  no console errors');
}

const failed = results.filter((r) => !r.ok);
console.log(`\n${results.length - failed.length}/${results.length} checks passed`);
ws.close();
process.exit(failed.length || pageErrors.length ? 1 : 0);
