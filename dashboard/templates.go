package dashboard

const layoutHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>middly · cache dashboard</title>
<script src="https://unpkg.com/htmx.org@1.9.12"></script>
<style>
  :root {
    --bg: #0d1117; --panel: #161b22; --border: #30363d; --text: #e6edf3;
    --muted: #8b949e; --green: #3fb950; --orange: #f0883e; --blue: #58a6ff;
    --red: #f85149;
  }
  * { box-sizing: border-box; }
  body { font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
         margin: 0; padding: 2rem; background: var(--bg); color: var(--text); }
  header { display: flex; justify-content: space-between; align-items: center;
           margin-bottom: 1.5rem; }
  h1 { font-size: 1.1rem; margin: 0; }
  h2 { font-size: 0.9rem; color: var(--muted); text-transform: uppercase;
       letter-spacing: 0.08em; margin: 2rem 0 0.6rem; }
  .badge { background: var(--panel); border: 1px solid var(--border);
           border-radius: 999px; padding: 0.15rem 0.6rem; font-size: 0.75rem;
           color: var(--muted); }
  .mode-switch { display: inline-flex; gap: 0.25rem; margin-left: 0.4rem;
                 vertical-align: middle; align-items: center; }
  .mode-switch .label { color: var(--muted); font-size: 0.75rem; padding-right: 0.2rem; }
  .mode-btn { background: var(--panel); border: 1px solid var(--border);
              color: var(--muted); border-radius: 999px;
              padding: 0.15rem 0.6rem; font-size: 0.75rem;
              cursor: pointer; font-family: inherit; }
  .mode-btn:hover { color: var(--text); border-color: var(--text); }
  .mode-btn.active { cursor: default; }
  .mode-btn.active.mode-record { color: var(--orange); border-color: var(--orange); }
  .mode-btn.active.mode-replay { color: var(--blue);   border-color: var(--blue); }
  .mode-btn.active.mode-passthrough { color: var(--text); border-color: var(--text); }
  .cards { display: grid; grid-template-columns: repeat(auto-fit, minmax(11rem, 1fr));
           gap: 0.75rem; }
  .card { background: var(--panel); border: 1px solid var(--border);
          border-radius: 6px; padding: 0.9rem 1rem; }
  .card .label { font-size: 0.7rem; color: var(--muted);
                 text-transform: uppercase; letter-spacing: 0.06em; }
  .card .value { font-size: 1.6rem; margin-top: 0.2rem; }
  .card.green .value  { color: var(--green); }
  .card.orange .value { color: var(--orange); }
  .card.blue .value   { color: var(--blue); }
  table { width: 100%; border-collapse: collapse; font-size: 0.82rem; }
  th, td { padding: 0.45rem 0.6rem; text-align: left; border-bottom: 1px solid var(--border); }
  th { color: var(--muted); font-weight: normal; text-transform: uppercase;
       font-size: 0.7rem; letter-spacing: 0.06em; }
  tr:hover td { background: rgba(255,255,255,0.02); }
  .hash { color: var(--muted); }
  .hit  { color: var(--green); }
  .miss { color: var(--orange); }
  .bypass { color: var(--blue); }
  .err  { color: var(--red); }
  .routes { display: flex; flex-wrap: wrap; gap: 0.5rem; margin-bottom: 1rem; }
  .route { background: var(--panel); border: 1px solid var(--border);
           border-radius: 4px; padding: 0.25rem 0.6rem; font-size: 0.78rem; }
  .route .arrow { color: var(--muted); margin: 0 0.4rem; }
  button { background: var(--panel); border: 1px solid var(--border); color: var(--text);
           border-radius: 4px; padding: 0.35rem 0.7rem; cursor: pointer;
           font-family: inherit; font-size: 0.78rem; }
  button:hover { border-color: var(--red); color: var(--red); }
  .stale { opacity: 0.4; }
</style>
</head>
<body>
  <header>
    <h1>middly <span class="badge">cache proxy</span>
        {{template "mode" .}}
        <span class="badge">{{.CacheRows}} rows</span></h1>
    <button hx-post="/dashboard/clear" hx-confirm="Clear all cache entries?"
            hx-swap="none">clear cache</button>
  </header>

  <div class="routes">
    {{range .Routes}}
    <div class="route"><strong>{{.Prefix}}</strong><span class="arrow">→</span>{{.Target}}</div>
    {{end}}
  </div>

  <h2>live stats</h2>
  <div hx-get="/dashboard/stats" hx-trigger="load, every 1s, cache-cleared from:body"
       hx-swap="innerHTML"></div>

  <h2>recent requests</h2>
  <div hx-get="/dashboard/recent" hx-trigger="load, every 1s, cache-cleared from:body"
       hx-swap="innerHTML"></div>
</body>
</html>
`

const statsHTML = `<div class="cards">
  <div class="card"><div class="label">total</div><div class="value">{{.Total}}</div></div>
  <div class="card green"><div class="label">hits</div><div class="value">{{.Hits}}</div></div>
  <div class="card orange"><div class="label">misses</div><div class="value">{{.Misses}}</div></div>
  <div class="card blue"><div class="label">bypass</div><div class="value">{{.Bypass}}</div></div>
  <div class="card"><div class="label">hit rate</div><div class="value">{{pct .HitRatePct}}</div></div>
  <div class="card"><div class="label">stored</div><div class="value">{{.Stored}}</div></div>
</div>`

const modeHTML = `<span class="mode-switch" id="mode-switch">
  <span class="label">mode:</span>
  <button class="mode-btn mode-record{{if eq .Mode "record"}} active{{end}}"
          hx-post="/dashboard/mode?mode=record"
          hx-target="#mode-switch" hx-swap="outerHTML"
          {{if eq .Mode "record"}}disabled{{end}}>record</button>
  <button class="mode-btn mode-replay{{if eq .Mode "replay"}} active{{end}}"
          hx-post="/dashboard/mode?mode=replay"
          hx-target="#mode-switch" hx-swap="outerHTML"
          {{if eq .Mode "replay"}}disabled{{end}}>replay</button>
  <button class="mode-btn mode-passthrough{{if eq .Mode "passthrough"}} active{{end}}"
          hx-post="/dashboard/mode?mode=passthrough"
          hx-target="#mode-switch" hx-swap="outerHTML"
          {{if eq .Mode "passthrough"}}disabled{{end}}>passthrough</button>
</span>`

const recentHTML = `<table>
  <thead>
    <tr>
      <th>time</th><th>method</th><th>route</th><th>path</th>
      <th>status</th><th>result</th><th>latency</th><th>hash</th>
    </tr>
  </thead>
  <tbody>
    {{range .Recent}}
    <tr>
      <td>{{timefmt .Time}}</td>
      <td>{{.Method}}</td>
      <td>{{.Namespace}}</td>
      <td>{{.Path}}</td>
      <td>{{if ge .Status 400}}<span class="err">{{.Status}}</span>{{else}}{{.Status}}{{end}}</td>
      <td>{{if .Hit}}<span class="hit">HIT</span>{{else if eq .Mode "passthrough"}}<span class="bypass">BYPASS</span>{{else}}<span class="miss">MISS</span>{{end}}</td>
      <td>{{micros .DurMicros}}</td>
      <td class="hash">{{shortHash .Hash}}</td>
    </tr>
    {{else}}
    <tr><td colspan="8" class="hash">no requests yet — send one through the proxy</td></tr>
    {{end}}
  </tbody>
</table>`
