#!/usr/bin/env node
// loadtest.mjs - hammer middly with hundreds of unique requests.
//
// Uses PokeAPI (https://pokeapi.co), which is free, key-less, and explicitly
// asks consumers to cache responses — exactly what middly does. Each request
// targets a distinct resource so pass 1 fills the cache; pass 2 is all HIT
// and orders of magnitude faster.
//
// Pass 2+ sets the per-request header `X-Middly-Mode: replay`, so a 200
// unambiguously means middly served from its own cache and a 502 means it
// wasn't cached. We don't read the upstream `X-Cache` header — many origins
// (PokeAPI included, via Cloudflare) set it themselves and would mislead us.
//
// Usage:
//   # terminal 1 — start middly with a /poke route
//   ./middly --routes='/poke=https://pokeapi.co'
//
//   # terminal 2 — run the load test (defaults: 400 reqs, concurrency 20, 2 passes)
//   node examples/loadtest/loadtest.mjs
//   node examples/loadtest/loadtest.mjs --count=800 --concurrency=32 --passes=3
//
//   # bypass middly to compare raw upstream
//   node examples/loadtest/loadtest.mjs --base=https://pokeapi.co
//
// Requires Node 18+ (uses native fetch).

import { argv, exit, stdout } from 'node:process';
import { performance } from 'node:perf_hooks';

const args = parseArgs(argv.slice(2));
if (args.help || args.h) usage(0);

const base = String(args.base ?? 'http://localhost:8080/poke').replace(/\/+$/, '');
const count = num(args.count, 400, '--count');
const concurrency = num(args.concurrency, 20, '--concurrency');
const passes = num(args.passes, 2, '--passes');
const timeoutMs = num(args.timeout, 30000, '--timeout');
const settleMs = num(args.settle, 1500, '--settle');

// Endpoint families with their max known id. Round-robin across them so
// we exercise diverse paths (different cache keys, different payload shapes).
const families = [
  { path: 'pokemon',  max: 1010 },
  { path: 'ability',  max: 298  },
  { path: 'move',     max: 919  },
  { path: 'item',     max: 2180 },
  { path: 'location', max: 1054 },
];

const targets = buildTargets(count, families);

console.log(`base       : ${base}`);
console.log(`count      : ${targets.length} unique requests across ${families.length} endpoint families`);
console.log(`concurrency: ${concurrency}`);
console.log(`passes     : ${passes}`);
console.log();

for (let p = 1; p <= passes; p++) {
  const replay = p > 1;
  const label = replay
    ? '(warm — X-Middly-Mode: replay; 200 = HIT, 502 = MISS)'
    : '(cold — fills the cache)';
  console.log(`pass ${p} ${label}`);

  if (p > 1 && settleMs > 0) {
    // middly's cache write is fire-and-forget on a goroutine; give it a
    // moment to flush before we start asking for replays.
    await new Promise(r => setTimeout(r, settleMs));
  }

  const stats = await runPass(targets, base, concurrency, timeoutMs, replay);
  printStats(stats, replay);
  console.log();
}

async function runPass(targets, base, concurrency, timeoutMs, replay) {
  const stats = {
    ok: 0, miss: 0, otherStatus: 0, err: 0,
    status: new Map(), errMsg: new Map(), latencies: [],
  };
  const headers = {
    Accept: 'application/json',
    'User-Agent': 'middly-loadtest',
  };
  if (replay) headers['X-Middly-Mode'] = 'replay';

  const t0 = performance.now();
  let cursor = 0;

  await Promise.all(Array.from({ length: concurrency }, async () => {
    while (true) {
      const i = cursor++;
      if (i >= targets.length) return;
      const url = `${base}${targets[i]}`;
      const start = performance.now();
      try {
        const res = await fetch(url, { headers, signal: AbortSignal.timeout(timeoutMs) });
        await res.arrayBuffer();
        stats.latencies.push(performance.now() - start);
        stats.status.set(res.status, (stats.status.get(res.status) ?? 0) + 1);
        if (res.status === 200) stats.ok++;
        else if (replay && res.status === 502) stats.miss++;
        else stats.otherStatus++;
      } catch (e) {
        stats.latencies.push(performance.now() - start);
        stats.err++;
        const k = e?.name || 'Error';
        stats.errMsg.set(k, (stats.errMsg.get(k) ?? 0) + 1);
      }
      if (stdout.isTTY && ((i + 1) % 50 === 0 || i + 1 === targets.length)) {
        stdout.write(`  progress: ${i + 1}/${targets.length}\r`);
      }
    }
  }));

  stats.elapsedSec = (performance.now() - t0) / 1000;
  return stats;
}

function printStats(s, replay) {
  s.latencies.sort((a, b) => a - b);
  const total = s.ok + s.miss + s.otherStatus + s.err;
  const q = (p) => s.latencies[Math.min(s.latencies.length - 1, Math.floor(s.latencies.length * p))] ?? 0;

  if (stdout.isTTY) stdout.write(' '.repeat(40) + '\r');

  console.log(`  total      : ${total}  (${(total / s.elapsedSec).toFixed(1)} req/s, elapsed ${s.elapsedSec.toFixed(2)}s)`);
  if (replay) {
    console.log(`  hit (200)  : ${s.ok}  (${pct(s.ok, total)})`);
    console.log(`  miss (502) : ${s.miss}  (${pct(s.miss, total)})`);
  } else {
    console.log(`  ok (200)   : ${s.ok}  (${pct(s.ok, total)})`);
  }
  if (s.otherStatus) console.log(`  other 4xx/5xx: ${s.otherStatus}`);
  if (s.err) console.log(`  errors     : ${s.err}  (${[...s.errMsg].map(([k, n]) => `${k}:${n}`).join(' ')})`);
  console.log(`  latency    : p50=${fmtMs(q(0.5))}  p95=${fmtMs(q(0.95))}  p99=${fmtMs(q(0.99))}  max=${fmtMs(s.latencies.at(-1) ?? 0)}`);
  const statusList = [...s.status.entries()].sort((a, b) => a[0] - b[0]).map(([st, n]) => `${st}:${n}`).join(' ');
  console.log(`  statuses   : ${statusList}`);
}

function buildTargets(count, families) {
  // Round-robin across families, advancing each family's id independently
  // so every emitted path is unique.
  const cursors = families.map(() => 1);
  const out = [];
  let f = 0;
  while (out.length < count) {
    const fam = families[f % families.length];
    const id = cursors[f % families.length]++;
    if (id <= fam.max) out.push(`/api/v2/${fam.path}/${id}`);
    f++;
    // If every family is exhausted, stop early.
    if (cursors.every((c, i) => c > families[i].max)) break;
  }
  return out;
}

function fmtMs(ms) {
  if (ms < 1) return `${(ms * 1000).toFixed(0)}µs`;
  if (ms < 1000) return `${ms.toFixed(1)}ms`;
  return `${(ms / 1000).toFixed(2)}s`;
}

function pct(a, b) {
  return b === 0 ? '0.0%' : `${((a / b) * 100).toFixed(1)}%`;
}

function num(v, dflt, name) {
  if (v === undefined || v === true) return dflt;
  const n = Number(v);
  if (!Number.isFinite(n) || n <= 0) {
    console.error(`${name} must be a positive number (got ${v})`);
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
  console.log(`loadtest.mjs - hammer middly with hundreds of unique requests

  --base=URL         base URL (default http://localhost:8080/poke)
  --count=N          total requests per pass (default 400)
  --concurrency=N    in-flight requests (default 20)
  --passes=N         how many cold/warm passes to run (default 2)
  --timeout=MS       per-request timeout in ms (default 30000)
  --settle=MS        wait between passes for async cache writes (default 1500)
  --help             show this help`);
  exit(code);
}
