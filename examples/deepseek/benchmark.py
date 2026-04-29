#!/usr/bin/env python3
"""benchmark.py - hammer DeepSeek with heavy prompts via middly and print
per-request time / tokens / cost.

Each prompt is sent exactly once. No cold/warm split.

Usage:
    # terminal 1 — middly listening on TLS 8080, fronting DeepSeek
    ./middly --routes='/deepseek=https://api.deepseek.com'

    # terminal 2
    export DEEPSEEK_API_KEY=sk-...
    python3 examples/deepseek/benchmark.py
    python3 examples/deepseek/benchmark.py --model=deepseek-reasoner --max-tokens=2048

Stdlib only. Python 3.7+.
"""

import argparse
import json
import os
import ssl
import sys
import time
from urllib import request, error

DEFAULT_BASE = "http://localhost:8080/deepseek"
DEFAULT_MODEL = "deepseek-chat"

# Heavy prompts — each one is meant to produce a long, expensive completion.
PROMPTS = [
    (
        "long-essay",
        "Write a detailed 1500-word technical essay comparing the memory models of "
        "Rust, Go, and C++. Cover ownership, borrowing, garbage collection tradeoffs, "
        "escape analysis, RAII, smart pointers, and concurrency safety. Include "
        "concrete code examples in each language and discuss performance implications.",
    ),
    (
        "code-generation",
        "Implement a complete LRU cache in Python with thread-safety, TTL support, "
        "size-based eviction, and async access. Include type hints, full docstrings, "
        "and at least 15 unit tests covering edge cases (concurrent access, expiry, "
        "eviction order, capacity=0, large keys). Output only valid Python code.",
    ),
    (
        "system-design",
        "Design a globally distributed rate limiter that handles 10M req/s across 50 "
        "regions. Discuss token bucket vs leaky bucket, Redis vs in-memory, clock "
        "skew, regional failover, hot key mitigation, and cost/latency tradeoffs. "
        "Include sequence diagrams in ASCII and pseudocode for the critical path.",
    ),
    (
        "step-by-step-math",
        "Solve step-by-step: prove that the sum of the first n odd positive integers "
        "is n^2, by induction. Then derive a closed-form for sum_{k=1}^{n} k^3 from "
        "scratch (no Faulhaber's formula). Show every algebraic manipulation and "
        "explain the reasoning at each step as if teaching an undergraduate.",
    ),
    (
        "code-review",
        "Critically review the following hypothetical microservice architecture: an "
        "order service that fans out to inventory, payment, shipping, and "
        "notification services synchronously over HTTP. Identify at least 12 distinct "
        "failure modes, propose remediations for each (saga, outbox, idempotency "
        "keys, circuit breakers, retries with jitter), and rank them by impact.",
    ),
    (
        "translation-and-summary",
        "Translate the entire preamble of the Universal Declaration of Human Rights "
        "into French, German, Spanish, Japanese, and Arabic. Then write a 400-word "
        "comparative analysis of stylistic differences between the translations, "
        "noting register, formality, and any culturally-loaded word choices.",
    ),
    (
        "sql-deep-dive",
        "Given a 1B-row events table on Postgres with columns (user_id bigint, "
        "event_type text, occurred_at timestamptz, payload jsonb), write the most "
        "efficient query to compute, per user, the median time between consecutive "
        "events of type 'click' over the last 30 days. Then explain the chosen plan, "
        "indexes required, partitioning strategy, and how to make it run in <5s.",
    ),
    (
        "creative-writing",
        "Write a 1200-word short story in the style of Ted Chiang about a software "
        "engineer who discovers that their unit tests have started failing in ways "
        "that retroactively change the production code. Maintain a tone of quiet "
        "philosophical dread. Include at least three distinct scenes.",
    ),
]

# DeepSeek public pricing in USD per 1M tokens (approximate; override with --price-*).
# https://api-docs.deepseek.com/quick_start/pricing
PRICING = {
    "deepseek-chat":     {"input": 0.27, "output": 1.10},
    "deepseek-reasoner": {"input": 0.55, "output": 2.19},
}


def parse_args():
    p = argparse.ArgumentParser(
        description="Single-pass heavy-prompt benchmark for DeepSeek via middly.",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    p.add_argument("--base", default=DEFAULT_BASE,
                   help="base URL (defaults to middly on https://localhost:8080)")
    p.add_argument("--key", default=os.environ.get("DEEPSEEK_API_KEY", ""),
                   help="API key (default: $DEEPSEEK_API_KEY)")
    p.add_argument("--model", default=DEFAULT_MODEL)
    p.add_argument("--max-tokens", type=int, default=2048, dest="max_tokens",
                   help="upper bound on completion tokens per call")
    p.add_argument("--timeout", type=float, default=300.0,
                   help="per-request timeout in seconds")
    p.add_argument("--insecure", action="store_true", default=True,
                   help="skip TLS verification (default: on, since localhost is self-signed)")
    p.add_argument("--price-input", type=float, default=None,
                   help="USD per 1M input tokens (override pricing table)")
    p.add_argument("--price-output", type=float, default=None,
                   help="USD per 1M output tokens (override pricing table)")
    return p.parse_args()


def make_ssl_ctx(insecure):
    if not insecure:
        return None
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    return ctx


def call_chat(url, headers, body, timeout, ssl_ctx):
    """POST a chat completion. Return dict with dur, status, json, bytes (or err)."""
    data = json.dumps(body).encode("utf-8")
    req = request.Request(url, data=data, method="POST", headers={
        **headers, "Content-Type": "application/json"
    })
    start = time.perf_counter()
    raw = b""
    status = 0
    try:
        with request.urlopen(req, timeout=timeout, context=ssl_ctx) as resp:
            raw = resp.read()
            status = resp.status
    except error.HTTPError as e:
        raw = e.read()
        status = e.code
    except Exception as e:
        return {"err": str(e), "dur": time.perf_counter() - start, "status": 0}
    dur = time.perf_counter() - start
    parsed = None
    try:
        parsed = json.loads(raw)
    except (json.JSONDecodeError, ValueError):
        pass
    return {"dur": dur, "status": status, "json": parsed, "bytes": len(raw)}


def fmt_secs(s):
    if s is None:
        return "—"
    if s < 1:
        return f"{s*1000:.0f}ms"
    return f"{s:.2f}s"


def fmt_cost(usd):
    if usd is None:
        return "—"
    if usd < 0.01:
        return f"${usd*100:.3f}¢"
    return f"${usd:.4f}"


def truncate(s, n):
    s = "" if s is None else str(s)
    return s if len(s) <= n else s[: n - 1] + "…"


def resolve_pricing(args):
    base = PRICING.get(args.model, {"input": 0.0, "output": 0.0})
    return {
        "input":  args.price_input  if args.price_input  is not None else base["input"],
        "output": args.price_output if args.price_output is not None else base["output"],
    }


def compute_cost(usage, pricing):
    if not usage:
        return None
    pin = (usage.get("prompt_tokens") or 0) * pricing["input"] / 1_000_000
    pout = (usage.get("completion_tokens") or 0) * pricing["output"] / 1_000_000
    return pin + pout


def main():
    args = parse_args()
    base = args.base.rstrip("/")
    pricing = resolve_pricing(args)
    ssl_ctx = make_ssl_ctx(args.insecure) if base.startswith("https://") else None

    if not args.key:
        print("warning: DEEPSEEK_API_KEY not set — upstream will 401.\n",
              file=sys.stderr)

    headers = {
        "Accept": "application/json",
        "User-Agent": "middly-deepseek-bench",
    }
    if args.key:
        headers["Authorization"] = f"Bearer {args.key}"

    print(f"target:  {base}")
    print(f"model:   {args.model}  (input ${pricing['input']}/Mtok, "
          f"output ${pricing['output']}/Mtok)")
    print(f"prompts: {len(PROMPTS)}, max_tokens={args.max_tokens}\n")

    rows = []
    for name, prompt in PROMPTS:
        body = {
            "model": args.model,
            "messages": [{"role": "user", "content": prompt}],
            "max_tokens": args.max_tokens,
            "temperature": 0,
        }
        print(f"→ {name} …", flush=True)
        r = call_chat(f"{base}/v1/chat/completions", headers, body,
                      args.timeout, ssl_ctx)

        usage = ((r.get("json") or {}).get("usage") or {}) if not r.get("err") else {}
        cost = compute_cost(usage, pricing)
        rows.append({
            "name": name,
            "status": r.get("status", 0),
            "err": r.get("err"),
            "dur": r.get("dur"),
            "prompt_tokens": usage.get("prompt_tokens"),
            "completion_tokens": usage.get("completion_tokens"),
            "total_tokens": usage.get("total_tokens"),
            "cost": cost,
        })

        if r.get("err"):
            print(f"  network error: {r['err']}\n")
        elif r["status"] != 200:
            j = r.get("json") or {}
            err = j.get("error") if isinstance(j, dict) else None
            msg = err.get("message") if isinstance(err, dict) else j
            print(f"  HTTP {r['status']}: {truncate(msg, 200)}\n")
        else:
            print(f"  ok  {fmt_secs(r['dur'])}  "
                  f"in={usage.get('prompt_tokens')} "
                  f"out={usage.get('completion_tokens')} "
                  f"cost={fmt_cost(cost)}\n")

    # --- table ---
    print("=" * 92)
    print(f"{'request':<24}  {'status':>6}  {'time':>9}  "
          f"{'in':>7}  {'out':>7}  {'tokens':>7}  {'cost':>10}")
    print("-" * 92)
    tot_dur = tot_in = tot_out = tot_tot = 0.0
    tot_cost = 0.0
    any_cost = False
    for r in rows:
        status = r["err"] or str(r["status"])
        tot_dur += r["dur"] or 0
        tot_in += r["prompt_tokens"] or 0
        tot_out += r["completion_tokens"] or 0
        tot_tot += r["total_tokens"] or 0
        if r["cost"] is not None:
            tot_cost += r["cost"]
            any_cost = True
        print(f"{truncate(r['name'], 24):<24}  "
              f"{truncate(status, 6):>6}  "
              f"{fmt_secs(r['dur']):>9}  "
              f"{(r['prompt_tokens'] or 0):>7}  "
              f"{(r['completion_tokens'] or 0):>7}  "
              f"{(r['total_tokens'] or 0):>7}  "
              f"{fmt_cost(r['cost']):>10}")
    print("-" * 92)
    print(f"{'TOTAL':<24}  {'':>6}  "
          f"{fmt_secs(tot_dur):>9}  "
          f"{int(tot_in):>7}  "
          f"{int(tot_out):>7}  "
          f"{int(tot_tot):>7}  "
          f"{fmt_cost(tot_cost) if any_cost else '—':>10}")
    print("=" * 92)


if __name__ == "__main__":
    main()
