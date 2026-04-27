#!/usr/bin/env node
// meteor.mjs - fetches NASA JPL's Fireball Data API through middly, prints
// human-friendly tables of recorded atmospheric meteor events, and proves
// the cache works by replaying every query.
//
// Endpoint: https://ssd-api.jpl.nasa.gov/fireball.api  (free, key-less)
//   columns: date, energy (kt), impact-e (kt TNT eq.), lat, lat-dir,
//            lon, lon-dir, alt (km), vel (km/s)
//
// Usage:
//   # terminal 1 — start middly with a /jpl route
//   ./middly --routes='/jpl=https://ssd-api.jpl.nasa.gov'
//
//   # terminal 2
//   node examples/meteor/meteor.mjs
//   node examples/meteor/meteor.mjs --base=https://ssd-api.jpl.nasa.gov   # bypass middly
//
// Cold pass: real network. Warm pass sends `X-Middly-Mode: replay`, so a
// 200 unambiguously means middly served from its SQLite cache and a 502
// means the entry wasn't cached.
//
// Requires Node 18+ (uses native fetch).

import { argv, exit } from 'node:process';
import { performance } from 'node:perf_hooks';

const args = parseArgs(argv.slice(2));
if (args.help || args.h) usage(0);

const base = String(args.base ?? 'http://localhost:8080/jpl').replace(/\/+$/, '');
const settleMs = num(args.settle, 1500, '--settle');

// Each query is its own cache key in middly. Mix of filters so we touch
// different facets of the dataset and store several distinct rows.
const queries = [
  { label: 'top 10 most energetic ever',     params: 'limit=10&sort=-energy' },
  { label: '20 most recent fireballs',       params: 'limit=20' },
  { label: 'all events ≥100 kt energy',      params: 'energy-min=100' },
  { label: 'fireballs in 2013 (Chelyabinsk)', params: 'date-min=2013-01-01&date-max=2013-12-31&limit=15' },
  { label: 'fireballs in 2020',              params: 'date-min=2020-01-01&date-max=2020-12-31&limit=15' },
  { label: 'high-velocity (≥40 km/s), top 10', params: 'vel-min=40&limit=10&sort=-vel' },
];

const passLabels = [
  '(cold — real network, fills middly cache)',
  '(warm — X-Middly-Mode: replay; 200 = HIT, 502 = MISS)',
];

const passResults = [[], []];

for (let p = 0; p < 2; p++) {
  const replay = p === 1;
  console.log(`\n=== pass ${p + 1} ${passLabels[p]} ===\n`);

  if (replay && settleMs > 0) {
    // Cache writes are async (goroutine in middly); let them flush.
    await new Promise(r => setTimeout(r, settleMs));
  }

  for (const q of queries) {
    const url = `${base}/fireball.api?${q.params}`;
    const headers = {
      Accept: 'application/json',
      'User-Agent': 'middly-meteor-demo',
    };
    if (replay) headers['X-Middly-Mode'] = 'replay';

    const start = performance.now();
    let res, body;
    try {
      res = await fetch(url, { headers, signal: AbortSignal.timeout(30000) });
      body = await res.text();
    } catch (e) {
      console.log(`  ${q.label}: fetch failed (${e.message})\n`);
      passResults[p].push({ q, dur: performance.now() - start, status: 0, err: e.message });
      continue;
    }
    const dur = performance.now() - start;

    if (res.status !== 200) {
      const verdict = replay && res.status === 502 ? 'MISS (not in cache)' : `HTTP ${res.status}`;
      console.log(`  ${q.label}\n    ${fmtMs(dur).padStart(8)}  ${verdict}\n`);
      passResults[p].push({ q, dur, status: res.status });
      continue;
    }

    const json = JSON.parse(body);
    const rows = (json.data ?? []).map(r => Object.fromEntries(json.fields.map((f, i) => [f, r[i]])));
    const verdict = replay ? 'HIT' : 'cold';

    console.log(`  ${q.label}`);
    console.log(`    ${fmtMs(dur).padStart(8)}  ${String(rows.length).padStart(3)} rows  ${(body.length / 1024).toFixed(1)} KB  ${verdict}`);
    printRows(rows.slice(0, 5));
    if (rows.length > 5) console.log(`      … (${rows.length - 5} more)\n`);
    else console.log();

    passResults[p].push({ q, dur, status: 200, rows: rows.length, bytes: body.length });
  }
}

// Side-by-side summary
console.log('=== summary ===');
console.log(`${'query'.padEnd(44)}  ${'cold'.padStart(9)}  ${'warm'.padStart(9)}  ${'speedup'.padStart(8)}`);
console.log('-'.repeat(80));
for (let i = 0; i < queries.length; i++) {
  const c = passResults[0][i];
  const w = passResults[1][i];
  const speedup = (c?.status === 200 && w?.status === 200) ? `${(c.dur / w.dur).toFixed(0)}×` : '—';
  const coldStr = c?.status === 200 ? fmtMs(c.dur) : `(${c?.status ?? 'err'})`;
  const warmStr = w?.status === 200 ? fmtMs(w.dur) : `(${w?.status ?? 'err'})`;
  console.log(`${truncate(queries[i].label, 44).padEnd(44)}  ${coldStr.padStart(9)}  ${warmStr.padStart(9)}  ${speedup.padStart(8)}`);
}
console.log('-'.repeat(80));
const coldTotal = passResults[0].reduce((s, r) => s + (r.dur ?? 0), 0);
const warmTotal = passResults[1].reduce((s, r) => s + (r.dur ?? 0), 0);
console.log(`${'TOTAL'.padEnd(44)}  ${fmtMs(coldTotal).padStart(9)}  ${fmtMs(warmTotal).padStart(9)}  ${(coldTotal / warmTotal).toFixed(0).padStart(7)}×`);

const warmMisses = passResults[1].filter(r => r.status === 502).length;
if (warmMisses > 0) {
  console.log(`\n⚠ ${warmMisses} warm query(ies) returned 502 — those weren't cached.`);
  console.log(`   make sure middly's global mode is "record" (curl -X POST 'http://localhost:8080/dashboard/mode?mode=record')`);
}

// --- helpers ---

function printRows(rows) {
  if (rows.length === 0) {
    console.log('      (no rows)');
    return;
  }
  console.log(`      ${'date (UTC)'.padEnd(20)}  ${'rad-en'.padStart(9)}  ${'TNT-eq'.padStart(7)}  ${'where'.padEnd(20)}  ${'alt'.padStart(6)}  ${'vel'.padStart(8)}`);
  for (const r of rows) {
    const where = (r.lat && r.lon)
      ? `${(+r.lat).toFixed(1)}°${r['lat-dir']}, ${(+r.lon).toFixed(1)}°${r['lon-dir']}`
      : '—';
    console.log(
      `      ${String(r.date ?? '—').padEnd(20)}  ${formatEnergy(r.energy).padStart(9)}  ${formatTNT(r['impact-e']).padStart(7)}  ${where.padEnd(20)}  ${formatAlt(r.alt).padStart(6)}  ${formatVel(r.vel).padStart(8)}`
    );
  }
}

function formatEnergy(e) {
  // JPL's `energy` field is "approximate total radiated energy in the
  // atmosphere in 10^10 joules", so the raw value of 1 means 10 GJ.
  if (e == null) return '—';
  const joules = +e * 1e10;
  if (joules >= 1e15) return `${(joules / 1e15).toFixed(1)} PJ`;
  if (joules >= 1e12) return `${(joules / 1e12).toFixed(1)} TJ`;
  if (joules >= 1e9)  return `${(joules / 1e9).toFixed(1)} GJ`;
  return `${(joules / 1e6).toFixed(1)} MJ`;
}

function formatTNT(e) {
  if (e == null) return '—';
  const n = +e;
  if (n >= 1) return `${n.toFixed(1)} kt`;
  return `${(n * 1000).toFixed(0)} t`;
}

function formatAlt(a) { return a == null ? '—' : `${(+a).toFixed(0)} km`; }
function formatVel(v) { return v == null ? '—' : `${(+v).toFixed(1)} km/s`; }

function fmtMs(ms) {
  if (ms < 1) return `${(ms * 1000).toFixed(0)}µs`;
  if (ms < 1000) return `${ms.toFixed(1)}ms`;
  return `${(ms / 1000).toFixed(2)}s`;
}

function truncate(s, n) {
  s = String(s ?? '');
  return s.length <= n ? s : s.slice(0, n - 1) + '…';
}

function num(v, dflt, name) {
  if (v === undefined || v === true) return dflt;
  const n = Number(v);
  if (!Number.isFinite(n) || n < 0) {
    console.error(`${name} must be a non-negative number (got ${v})`);
    exit(2);
  }
  return n;
}

function parseArgs(arr) {
  const out = {};
  for (const a of arr) {
    const m = a.match(/^--([^=]+)(?:=(.*))?$/);
    if (!m) { console.error(`unknown arg: ${a}`); exit(2); }
    out[m[1]] = m[2] ?? true;
  }
  return out;
}

function usage(code) {
  console.log(`meteor.mjs - fetch NASA JPL Fireball API through middly

  --base=URL     base URL (default http://localhost:8080/jpl)
  --settle=MS    wait between cold/warm passes for async cache writes (default 1500)
  --help         show this help`);
  exit(code);
}
