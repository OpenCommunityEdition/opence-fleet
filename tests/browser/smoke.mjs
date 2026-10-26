// The pages in a real browser (Playwright's Chromium), against a whole
// fleet on loopback: a control plane, a node's agent with the process
// runtime, and tools/fakehalo as the release (OPENCE_FLEET_PLAN.md 9.4). It
// signs in through the setup link, makes a profile and three servers, sees
// them come up on the pages without a reload, kicks a scripted player,
// changes a rotation and sees the server's refusal, runs a bulk say, types
// sv_status on the console, switches the theme, and looks at it all at
// 390 px. Each page's screenshot is kept (SHOTS, for a human look; not
// compared pixel by pixel). A page's console error fails it.
//
//     FLEET_BIN=<opence-fleet> FAKEHALO_BIN=<fakehalo> node tests/browser/smoke.mjs
//
// (with Playwright where Node finds it: npm ci in tests/browser, or NODE_PATH)

import { spawn, execFileSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, readdirSync, rmSync } from 'node:fs';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';

const require = createRequire(import.meta.url);
const { chromium } = require('playwright');

const fleetBin = process.env.FLEET_BIN || 'opence-fleet';
const fakeBin = process.env.FAKEHALO_BIN || 'fakehalo';
const work = mkdtempSync(path.join(tmpdir(), 'opence-fleet-browser-'));
const shots = process.env.SHOTS || path.join(work, 'shots');
mkdirSync(shots, { recursive: true });
const port = 18200 + Math.floor(Math.random() * 500);
const base = `http://127.0.0.1:${port}`;
const children = [];
let failures = 0;

// (nothing leaves loopback: the registry, the update check and the masters
// point nowhere)
const env = {
  ...process.env,
  FLEET_DATA: path.join(work, 'control'),
  FLEET_LISTEN: `127.0.0.1:${port}`,
  FLEET_PUBLIC_URL: base,
  FLEET_IMAGE: '127.0.0.1:9/opence-server',
  FLEET_UPDATES: 'off',
  FLEET_UPDATE_URL: 'http://127.0.0.1:9/',
  FLEET_TIMEZONE: 'UTC',
  FLEET_LOG_LEVEL: 'warn',
};

function run(bin, args, extra = {}) {
  return execFileSync(bin, args, { env: { ...env, ...extra }, encoding: 'utf8' });
}

function start(bin, args, extra, onLine) {
  const child = spawn(bin, args, { env: { ...env, ...extra }, stdio: ['ignore', 'pipe', 'pipe'] });
  children.push(child);
  for (const stream of [child.stdout, child.stderr]) {
    let buffer = '';
    stream.on('data', (chunk) => {
      buffer += chunk;
      let i;
      while ((i = buffer.indexOf('\n')) >= 0) {
        const line = buffer.slice(0, i);
        buffer = buffer.slice(i + 1);
        if (process.env.VERBOSE) console.log(`[${path.basename(bin)}] ${line}`);
        onLine?.(line);
      }
    });
  }
  return child;
}

function check(what, ok, detail = '') {
  console.log(`${ok ? 'ok  ' : 'FAIL'} ${what}${detail ? `: ${detail}` : ''}`);
  if (!ok) failures++;
}

async function until(what, fn, timeout = 30000) {
  const deadline = Date.now() + timeout;
  for (;;) {
    try {
      if (await fn()) return true;
    } catch {}
    if (Date.now() > deadline) {
      check(what, false, `not within ${timeout / 1000} s`);
      return false;
    }
    await sleep(250);
  }
}

let browser;
try {
  // ---------- the fleet
  run(fakeBin, ['--make-release', path.join(work, 'release.zip')]);
  run(fakeBin, ['--make-maps', path.join(work, 'gamedata')]);
  run(fleetBin, ['release-zip', '--latest', 'build-900', path.join(work, 'release.zip')]);
  // (ports and loopback addresses of their own, beside whatever else runs here)
  const joinToken = run(fleetBin, ['join-token', '--quiet', '--runtime', 'process', '--tunnel-ports', '42700-42719',
    '--web-ports', '42800-42819', '--loopback-base', '120', 'home']).trim();
  let setupLink = '';
  start(fleetBin, ['serve'], {}, (line) => {
    const m = line.match(/https?:\/\/\S+\/setup\?code=\w+/);
    if (m) setupLink = m[0];
  });
  await until('the control plane', async () => (await fetch(`${base}/healthz`)).ok, 15000);
  const agentEnv = {
    FLEET_AGENT_DIR: path.join(work, 'node'),
    FLEET_AGENT_RUNTIME: 'process',
    FLEET_AGENT_DATA_ROOT: path.join(work, 'gamedata'),
    FLEET_AGENT_LOG_LEVEL: 'warn',
  };
  const envFile = path.join(work, 'agent.env');
  run(fleetBin, ['agent', '--join', base, '--token', joinToken, '--env-file', envFile], agentEnv);
  const nodeToken = (await import('node:fs')).readFileSync(envFile, 'utf8').match(/^FLEET_AGENT_TOKEN=(.*)$/m)[1];
  start(fleetBin, ['agent'], { ...agentEnv, FLEET_AGENT_URL: base, FLEET_AGENT_TOKEN: nodeToken });
  await until('the setup link', () => setupLink !== '', 10000);

  // ---------- the pages
  browser = await chromium.launch();
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await context.newPage();
  const errors = [];
  page.on('console', (m) => m.type() === 'error' && errors.push(`${page.url()}: ${m.text()}`));
  page.on('pageerror', (e) => errors.push(`${page.url()}: ${e.message}`));
  const shot = (name) => page.screenshot({ path: path.join(shots, `${name}.png`), fullPage: true });

  await page.goto(setupLink);
  await page.fill('#fleet', 'Browser fleet');
  await page.fill('#name', 'owner');
  await page.fill('#password', 'a long enough password');
  await page.click('button[type=submit]');
  await page.waitForURL(`${base}/`);
  check('the setup made the owner and signed in', (await page.textContent('body')).includes('owner'));
  await shot('dashboard-empty');

  await page.goto(`${base}/profiles/new`);
  await page.fill('#name', 'BTB');
  await page.fill('#region', 'eu');
  await page.fill('textarea[name=rotation]', 'bloodgulch:ctf, sidewinder:slayer');
  await page.locator('form[action^="/profiles"] button[type=submit]').first().click();
  await page.waitForLoadState();
  check('a profile is made', (await page.textContent('body')).includes('BTB'), page.url());
  await shot('profile');

  await page.goto(`${base}/servers/new`);
  await page.selectOption('#profile', { label: 'BTB' });
  await page.fill('#count', '3');
  await page.fill('#pattern', 'BTB #{n}');
  await page.click('form[action="/servers"] button[type=submit]');
  await page.waitForLoadState();
  await shot('servers-made');

  // (up on the dashboard without a reload: the live stream's fragments)
  await page.goto(`${base}/`);
  await until('three servers up on the dashboard, live', async () =>
    (await page.evaluate(() => (document.body.innerText.match(/\bup\b|playing|lobby/gi) || []).length)) >= 3, 45000);
  await shot('dashboard');

  await page.goto(`${base}/servers`);
  await shot('servers');
  check('the servers list names them', (await page.textContent('body')).includes('btb-3'));

  // kick a scripted player
  await page.goto(`${base}/servers/btb-1/players`);
  const kick = page.locator('form[action*="/players/"][action$="/kick"]');
  if (await until('a scripted player on btb-1', async () => (await kick.count()) > 0, 45000)) {
    await shot('players');
    await kick.first().evaluate((form) => form.requestSubmit());
    await page.waitForLoadState();
    check('a kick is done', /kick/i.test(await page.textContent('body')), page.url());
  }

  // a rotation the server refuses
  await page.goto(`${base}/servers/btb-1/rotation`);
  await page.fill('textarea[name=rotation]', 'bloodgulch:ctf, nowhere:slayer');
  await page.locator('form[action$="/rotation"] button[type=submit]').first().click();
  await page.waitForLoadState();
  check("the refused rotation's entry is named", (await page.textContent('body')).includes('nowhere'));
  await shot('rotation-refused');

  // a bulk say
  await page.goto(`${base}/servers`);
  await page.locator('input[type=checkbox][name=slug]').first().waitFor();
  for (const box of await page.locator('input[type=checkbox][name=slug]').all()) await box.check();
  await page.selectOption('select[name=action]', 'say');
  await page.fill('#bulk input[name=text]', 'hello from the browser');
  await page.click('#bulk button[type=submit]');
  await page.waitForLoadState();
  await until('the bulk say is done', async () => /done|succeeded|3 of 3|3\/3/i.test(await page.textContent('body')), 20000);
  await shot('bulk-say');

  // the console
  await page.goto(`${base}/servers/btb-1/console`);
  await page.fill('[data-console-form] input[name=line]', 'sv_status');
  await page.press('[data-console-form] input[name=line]', 'Enter');
  await until('sv_status answered on the console', async () =>
    /players/i.test(await page.textContent('[data-console-log]')), 15000);
  await shot('console');

  // the theme
  const before = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
  await page.click('[data-theme-toggle]');
  await page.waitForLoadState();
  const after = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
  check('the theme switches', before !== after, `${before} → ${after}`);
  await shot('console-other-theme');

  // the library: several maps at once, then a whole folder (each file by
  // its folder; those the library has already counted, not sent again)
  const mapsDir = path.join(work, 'gamedata', 'maps');
  const maps = readdirSync(mapsDir).filter((f) => f.endsWith('.map')).sort();
  await page.goto(`${base}/library`);
  await page.selectOption('form[data-upload] select[name=kind]', 'map');
  await page.setInputFiles('form[data-upload] input[name=file]', maps.slice(0, 3).map((f) => path.join(mapsDir, f)));
  await page.click('form[data-upload] button[type=submit]');
  await until('three maps uploaded at once', async () =>
    /^3 added\./.test(await page.textContent('[data-upload-words]')), 30000);
  await page.goto(`${base}/library`);
  await page.setInputFiles('form[data-upload] input[name=folder]', path.join(work, 'gamedata'));
  await page.click('form[data-upload] button[type=submit]');
  await until('a folder of maps uploaded', async () =>
    new RegExp(`^${maps.length - 3} added, 3 the library had already\\.`).test(await page.textContent('[data-upload-words]')),
    60000);
  await page.goto(`${base}/library`);
  const listed = await page.textContent('body');
  check('every map is in the library', maps.every((f) => listed.includes(f.replace(/\.map$/, ''))), maps.join(' '));
  await shot('library');

  // the other pages, each once
  for (const p of ['/nodes', '/profiles', '/players', '/bans', '/library', '/releases', '/jobs', '/schedules', '/alerts',
    '/audit', '/team', '/settings', '/account', '/servers/btb-2', '/servers/btb-2/settings', '/servers/btb-2/history']) {
    const response = await page.goto(base + p);
    check(`${p} answers`, response.status() < 400, String(response.status()));
    await shot(`page${p.replaceAll('/', '_')}`);
  }

  // at 390 px: nothing scrolls sideways
  const phone = await browser.newContext({ viewport: { width: 390, height: 844 }, storageState: await context.storageState() });
  const small = await phone.newPage();
  small.on('pageerror', (e) => errors.push(`${small.url()}: ${e.message}`));
  for (const p of ['/', '/servers', '/servers/btb-1', '/servers/btb-1/players', '/servers/btb-1/console', '/nodes', '/profiles']) {
    await small.goto(base + p);
    await small.waitForTimeout(500);
    const width = await small.evaluate(() => document.documentElement.scrollWidth);
    check(`${p} fits 390 px`, width <= 390, `${width} px wide`);
    await small.screenshot({ path: path.join(shots, `390${p.replaceAll('/', '_')}.png`), fullPage: true });
  }

  check('no console errors', errors.length === 0, errors.join('\n'));
} catch (e) {
  check('the smoke test ran', false, e.stack || String(e));
} finally {
  await browser?.close();
  for (const child of children.reverse()) child.kill('SIGTERM');
  await sleep(1500);
  for (const child of children) if (child.exitCode === null) child.kill('SIGKILL');
  console.log(`screenshots: ${shots}`);
  if (!process.env.SHOTS) {
    // (the screenshots are kept when asked for)
  }
  rmSync(path.join(work, 'node'), { recursive: true, force: true });
}
console.log(failures ? `${failures} failed` : 'all passed');
process.exit(failures ? 1 : 0);
