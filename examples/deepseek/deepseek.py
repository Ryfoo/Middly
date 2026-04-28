#!/usr/bin/env python3
"""deepseek.py - test middly with real DeepSeek API calls.

Sends a GET /v1/models and several distinct chat completions through middly,
then replays them. Cold pass costs real tokens; warm pass costs $0 because
middly serves every byte from its SQLite cache.

Usage:
    # terminal 1
    ./middly --routes='/deepseek=https://api.deepseek.com'

    # terminal 2
    export DEEPSEEK_API_KEY=sk-...
    python3 examples/deepseek/deepseek.py
    python3 examples/deepseek/deepseek.py --model=deepseek-reasoner --max-tokens=120
    python3 examples/deepseek/deepseek.py --base=https://api.deepseek.com   # bypass middly

Pass 2 sends `X-Middly-Mode: replay` per-request, so a 200 unambiguously
means middly served from its SQLite cache and a 502 means it wasn't cached.

No key set? The script still runs - upstream returns 401, middly caches the
401, and pass 2 replays it. That alone proves the proxy is wired correctly.

Stdlib only - works on Python 3.7+, no `pip install` needed.
"""

import argparse
import json
import os
import sys
import time
from urllib import request, error

DEFAULT_BASE = "http://localhost:8080/deepseek"
DEFAULT_MODEL = "deepseek-chat"

PROMPTS = [
    "In one sentence, what is a reverse proxy?",
    "In one sentence, why is HTTPS important?",
    "Name three programming languages with manual memory management.",
    "What does SQLite's WAL mode do? Two sentences max.",
    "Explain prompt caching in two sentences.",
]


def parse_args():
    p = argparse.ArgumentParser(
        description="Test middly with real DeepSeek API calls.",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    p.add_argument("--base", default=DEFAULT_BASE,
                   help="base URL (point at middly, or DeepSeek to bypass)")
    p.add_argument("--key", default=os.environ.get("DEEPSEEK_API_KEY", ""),
                   help="API key (default: $DEEPSEEK_API_KEY)")
    p.add_argument("--model", default=DEFAULT_MODEL)
    p.add_argument("--max-tokens", type=int, default=80, dest="max_tokens")
    p.add_argument("--settle", type=float, default=1.5,
                   help="seconds to wait between cold and warm passes")
    return p.parse_args()


def call_json(method, url, *, headers=None, body=None, timeout=60):
    """Make an HTTP call, return dict of {dur, status, json, bytes} or {err}."""
    headers = dict(headers or {})
    data = None
    if body is not None:
        headers.setdefault("Content-Type", "application/json")
        data = json.dumps(body).encode("utf-8")
    req = request.Request(url, data=data, method=method, headers=headers)
    start = time.perf_counter()
    raw = b""
    status = 0
    try:
        with request.urlopen(req, timeout=timeout) as resp:
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


def fmt_ms(seconds):
    if seconds is None:
        return "—"
    ms = seconds * 1000
    if ms < 1:
        return f"{ms*1000:.0f}µs"
    if ms < 1000:
        return f"{ms:.1f}ms"
    return f"{seconds:.2f}s"


def truncate(s, n):
    s = "" if s is None else str(s)
    return s if len(s) <= n else s[: n - 1] + "…"


def print_outcome(label, r, on_ok=None):
    if r.get("err"):
        print(f"  {label}\n    {fmt_ms(r['dur']):>8}  network error: {r['err']}\n")
        return
    if r["status"] == 200:
        tag = "ok"
    elif r["status"] == 502:
        tag = "MISS (not cached)"
    else:
        tag = f"HTTP {r['status']}"
    print(f"  {label}")
    print(f"    {fmt_ms(r['dur']):>8}  {r['bytes']/1024:.1f} KB  {tag}")
    j = r.get("json")
    if r["status"] == 200 and j and on_ok:
        on_ok(j)
    elif isinstance(j, dict) and "error" in j:
        err = j["error"]
        msg = err.get("message") if isinstance(err, dict) else str(err)
        print(f"    error: {msg}")
    print()


def main():
    args = parse_args()
    base = args.base.rstrip("/")
    key = args.key

    if not key:
        print("warning: DEEPSEEK_API_KEY not set — upstream will 401, but middly will still",
              file=sys.stderr)
        print("         cache and replay the 401 response (proving the cache works).\n",
              file=sys.stderr)

    results = [[], []]

    for p in range(2):
        replay = (p == 1)
        title = "(warm — X-Middly-Mode: replay; $0 cost)" if replay else "(cold — real DeepSeek calls)"
        print(f"=== pass {p + 1} {title} ===\n")

        if replay and args.settle > 0:
            time.sleep(args.settle)

        headers = {
            "Accept": "application/json",
            "User-Agent": "middly-deepseek-demo",
        }
        if key:
            headers["Authorization"] = f"Bearer {key}"
        if replay:
            headers["X-Middly-Mode"] = "replay"

        # GET /v1/models — small free-form lookup
        r = call_json("GET", f"{base}/v1/models", headers=headers)
        results[p].append({"label": "GET /v1/models", **r})

        def models_print(j):
            data = j.get("data", []) if isinstance(j, dict) else []
            ids = ", ".join((m.get("id") or "?") for m in data[:5])
            print(f"    {len(data)} models; first: {ids}")

        print_outcome("GET /v1/models", r, models_print)

        # POST /v1/chat/completions — one call per prompt = one cache key per prompt
        for prompt in PROMPTS:
            body = {
                "model": args.model,
                "messages": [{"role": "user", "content": prompt}],
                "max_tokens": args.max_tokens,
                "temperature": 0,
            }
            r = call_json("POST", f"{base}/v1/chat/completions",
                          headers=headers, body=body)
            row_label = f"chat: {truncate(prompt, 44)}"
            results[p].append({"label": row_label, **r})

            def on_ok(j):
                choices = j.get("choices") or []
                msg = (choices[0] if choices else {}).get("message", {}).get("content", "")
                print(f"    → {truncate(msg.strip() or '(no content)', 220)}")
                u = j.get("usage") or {}
                if u:
                    print(f"    tokens: prompt={u.get('prompt_tokens')} "
                          f"completion={u.get('completion_tokens')} "
                          f"total={u.get('total_tokens')}")

            print_outcome(row_label, r, on_ok)

    # --- summary ---
    print("=== summary ===")
    print(f"{'request':<52}  {'cold':>13}  {'warm':>13}  {'speedup':>8}")
    print("-" * 94)
    for c, w in zip(results[0], results[1]):
        matched = c.get("status", 0) > 0 and c["status"] == w.get("status")
        speedup = (f"{c['dur']/w['dur']:.0f}×"
                   if matched and c.get("dur") and w.get("dur")
                   else "—")
        cold_str = (fmt_ms(c.get("dur"))
                    + ("" if c.get("status") == 200 else f" ({c.get('status', 'err')})"))
        warm_str = (fmt_ms(w.get("dur"))
                    + ("" if w.get("status") == 200 else f" ({w.get('status', 'err')})"))
        print(f"{truncate(c['label'], 52):<52}  {cold_str:>13}  {warm_str:>13}  {speedup:>8}")
    print("-" * 94)

    cold_total = sum((r.get("dur") or 0) for r in results[0])
    warm_total = sum((r.get("dur") or 0) for r in results[1])
    speedup_total = f"{cold_total/warm_total:.0f}×" if warm_total > 0 else "—"
    print(f"{'TOTAL':<52}  {fmt_ms(cold_total):>13}  {fmt_ms(warm_total):>13}  {speedup_total:>8}")

    total_tokens = sum(
        ((r.get("json") or {}).get("usage") or {}).get("total_tokens") or 0
        for r in results[0]
    )
    if total_tokens:
        print(f"\ntokens billed by DeepSeek: cold={total_tokens}, warm=0  "
              f"(warm pass served from middly's cache)")

    warm_misses = sum(1 for r in results[1] if r.get("status") == 502)
    if warm_misses:
        print(f"\n⚠ {warm_misses} warm request(s) returned 502 — those weren't cached.",
              file=sys.stderr)
        print("  is middly's global mode \"record\"?  "
              "curl -X POST 'http://localhost:8080/dashboard/mode?mode=record'",
              file=sys.stderr)


if __name__ == "__main__":
    main()
