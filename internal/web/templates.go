package web

const clusterTemplate = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <title>Cluster — weft</title>
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <style>
      :root {
        color-scheme: light;
        --bg: #f4f2ed;
        --panel: #ffffff;
        --text: #1c1b17;
        --muted: #6f6b65;
        --accent: #235a3a;
        --accent-weak: #d7e6dd;
        --border: #e2ddd4;
        --warning: #c96b2c;
        --danger: #b0382e;
      }
      * { box-sizing: border-box; }
      body {
        margin: 0;
        font-family: "IBM Plex Sans", "Helvetica Neue", Arial, sans-serif;
        color: var(--text);
        background: radial-gradient(circle at top, #fdfbf7, var(--bg));
      }
      header {
        padding: 20px 24px 12px;
        background: var(--panel);
        border-bottom: 1px solid var(--border);
        position: sticky;
        top: 0;
        z-index: 10;
      }
      .title-row {
        display: flex;
        align-items: baseline;
        justify-content: space-between;
        gap: 12px;
      }
      h1 { margin: 0; font-size: 20px; letter-spacing: 0.08em; text-transform: uppercase; }
      h2 { font-size: 16px; margin: 0 0 12px; text-transform: uppercase; letter-spacing: 0.05em; color: var(--muted); }
      .muted { color: var(--muted); font-size: 13px; }
      .nav { display: flex; gap: 8px; margin-top: 12px; }
      .nav a {
        padding: 6px 10px;
        border-radius: 999px;
        border: 1px solid var(--border);
        text-decoration: none;
        color: var(--text);
        font-size: 13px;
      }
      .nav a.active {
        background: var(--accent-weak);
        border-color: var(--accent);
        color: var(--accent);
        font-weight: 600;
      }
      main { padding: 20px 24px 32px; }
      section { margin-bottom: 28px; }
      .host-grid {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
        gap: 12px;
      }
      .host-card {
        border: 1px solid var(--border);
        border-radius: 12px;
        padding: 14px 16px;
        background: var(--panel);
      }
      .host-header {
        display: flex;
        justify-content: space-between;
        align-items: center;
        margin-bottom: 10px;
      }
      .host-name { font-weight: 600; font-size: 15px; }
      .status-badge {
        font-size: 12px;
        padding: 2px 8px;
        border-radius: 999px;
        font-weight: 500;
      }
      .status-online { background: var(--accent-weak); color: var(--accent); }
      .status-offline { background: #fde8e8; color: var(--danger); }
      .status-unknown { background: #f0ede6; color: var(--muted); }
      .gpu-list { margin: 8px 0 0; }
      .gpu-item {
        display: flex;
        align-items: center;
        gap: 8px;
        margin-bottom: 6px;
        font-size: 13px;
      }
      .gpu-name { min-width: 100px; color: var(--muted); }
      .gpu-bar-track {
        flex: 1;
        height: 8px;
        background: #f0ede6;
        border-radius: 4px;
        overflow: hidden;
      }
      .gpu-bar-fill {
        height: 100%;
        background: var(--accent);
        border-radius: 4px;
        transition: width 0.3s;
      }
      .gpu-mem { font-size: 12px; color: var(--muted); min-width: 50px; text-align: right; }
      .host-metrics {
        display: flex;
        gap: 16px;
        font-size: 13px;
        color: var(--muted);
        margin-top: 8px;
        padding-top: 8px;
        border-top: 1px solid var(--border);
      }
      .coord-panel {
        border: 1px solid var(--border);
        border-radius: 12px;
        padding: 14px 16px;
        background: var(--panel);
        display: flex;
        gap: 24px;
        align-items: center;
      }
      .coord-status {
        display: flex;
        align-items: center;
        gap: 8px;
        font-weight: 600;
      }
      .coord-dot {
        width: 10px;
        height: 10px;
        border-radius: 50%;
        display: inline-block;
      }
      .coord-dot.running { background: var(--accent); }
      .coord-dot.stopped { background: var(--danger); }
      .coord-detail { font-size: 13px; color: var(--muted); }
      table {
        width: 100%;
        border-collapse: collapse;
        background: var(--panel);
        border: 1px solid var(--border);
        border-radius: 12px;
        overflow: hidden;
      }
      thead {
        background: #f0ede6;
        text-transform: uppercase;
        letter-spacing: 0.05em;
        font-size: 11px;
        color: var(--muted);
      }
      th, td { padding: 4px 8px; border-bottom: 1px solid var(--border); text-align: left; font-size: 13px; }
      th { padding: 6px 8px; }
      tr:last-child td { border-bottom: none; }
      tbody tr:hover { background: #f9f8f6; }
      .op-err { color: var(--danger); }
      .nowrap { white-space: nowrap; }
      .mono { font-family: "SF Mono", "Menlo", monospace; font-size: 12px; }
    </style>
  </head>
  <body>
    <header>
      <div class="title-row">
        <h1>weft cluster</h1>
        <div class="muted" id="last-update"></div>
      </div>
      <div class="nav">
        <a href="/">Jobs</a>
        <a href="/cluster" class="active">Cluster</a>
      </div>
    </header>
    <main>
      <section>
        <h2>Coordinator</h2>
        <div class="coord-panel" id="coord-panel">
          <div class="coord-status">
            <span class="coord-dot stopped" id="coord-dot"></span>
            <span id="coord-label">loading…</span>
          </div>
        </div>
      </section>
      <section>
        <h2>Hosts</h2>
        <div class="host-grid" id="host-grid"></div>
      </section>
      <section>
        <h2>Recent Decisions</h2>
        <table>
          <thead>
            <tr>
              <th class="nowrap">Time</th>
              <th class="nowrap">Operation</th>
              <th class="nowrap">Host</th>
              <th class="nowrap">Job</th>
              <th>Detail</th>
            </tr>
          </thead>
          <tbody id="oplog-body">
            <tr><td colspan="5" class="muted">Loading…</td></tr>
          </tbody>
        </table>
      </section>
    </main>
    <script>
    (function() {
      function fetchJSON(url) {
        return fetch(url).then(function(r) { return r.json(); });
      }

      function renderHosts(hosts) {
        var grid = document.getElementById('host-grid');
        grid.innerHTML = '';
        hosts.forEach(function(h) {
          var statusClass = 'status-unknown';
          if (h.status === 'online') statusClass = 'status-online';
          else if (h.status === 'offline') statusClass = 'status-offline';

          var gpuHTML = '';
          if (h.gpus && h.gpus.length) {
            gpuHTML = '<div class="gpu-list">';
            h.gpus.forEach(function(g) {
              gpuHTML += '<div class="gpu-item">' +
                '<span class="gpu-name">' + g.name + '</span>' +
                '<div class="gpu-bar-track"><div class="gpu-bar-fill" style="width:0%"></div></div>' +
                '<span class="gpu-mem">' + g.memory + '</span>' +
                '</div>';
            });
            gpuHTML += '</div>';
          }

          var metricsHTML = '<div class="host-metrics">' +
            '<div>CPU ' + (h.cpu_load || '--') + '</div>' +
            '<div>RAM ' + (h.mem_usage || '--') + '</div>' +
            '<div>Net ' + (h.network_bw || '--') + '</div>' +
            '</div>';

          var card = document.createElement('div');
          card.className = 'host-card';
          card.innerHTML = '<div class="host-header">' +
            '<span class="host-name">' + h.name + '</span>' +
            '<span class="status-badge ' + statusClass + '">' + (h.status || 'unknown') + '</span>' +
            '</div>' +
            '<div style="font-size:13px;color:var(--muted)">' + h.os + '/' + h.arch + ' · ' + h.cpu_cores + ' cores · ' + h.memory + '</div>' +
            gpuHTML +
            metricsHTML;
          grid.appendChild(card);
        });
      }

      function renderCoordinator(state) {
        var dot = document.getElementById('coord-dot');
        var label = document.getElementById('coord-label');
        if (state.running) {
          dot.className = 'coord-dot running';
          label.textContent = 'Running (PID ' + state.pid + ')';
        } else {
          dot.className = 'coord-dot stopped';
          label.textContent = 'Not running';
        }
      }

      function renderOplog(entries) {
        var body = document.getElementById('oplog-body');
        if (!entries || entries.length === 0) {
          body.innerHTML = '<tr><td colspan="5" class="muted">No recent operations.</td></tr>';
          return;
        }
        body.innerHTML = '';
        entries.forEach(function(e) {
          var tr = document.createElement('tr');
          var t = new Date(e.t);
          var timeStr = t.toLocaleTimeString();
          var errClass = e.err ? ' op-err' : '';
          var detail = e.detail || '';
          if (e.err) detail += (detail ? ' — ' : '') + e.err;
          tr.innerHTML = '<td class="nowrap mono">' + timeStr + '</td>' +
            '<td class="nowrap' + errClass + '">' + e.op + '</td>' +
            '<td class="nowrap">' + (e.host || '—') + '</td>' +
            '<td class="nowrap">' + (e.job || '—') + '</td>' +
            '<td>' + detail + '</td>';
          body.appendChild(tr);
        });
      }

      function refresh() {
        Promise.all([
          fetchJSON('/api/hosts'),
          fetchJSON('/api/coordinator'),
          fetchJSON('/api/oplog')
        ]).then(function(results) {
          renderHosts(results[0]);
          renderCoordinator(results[1]);
          renderOplog(results[2]);
          document.getElementById('last-update').textContent = 'Updated ' + new Date().toLocaleTimeString();
        }).catch(function() {});
      }

      refresh();
      setInterval(refresh, 10000);
    })();
    </script>
  </body>
</html>
`

const indexTemplate = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <title>{{.Title}}</title>
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <style>
      :root {
        color-scheme: light;
        --bg: #f4f2ed;
        --panel: #ffffff;
        --text: #1c1b17;
        --muted: #6f6b65;
        --accent: #235a3a;
        --accent-weak: #d7e6dd;
        --border: #e2ddd4;
        --warning: #c96b2c;
        --danger: #b0382e;
      }
      * { box-sizing: border-box; }
      body {
        margin: 0;
        font-family: "IBM Plex Sans", "Helvetica Neue", Arial, sans-serif;
        color: var(--text);
        background: radial-gradient(circle at top, #fdfbf7, var(--bg));
      }
      header {
        padding: 20px 24px 12px;
        background: var(--panel);
        border-bottom: 1px solid var(--border);
        position: sticky;
        top: 0;
        z-index: 10;
      }
      .title-row {
        display: flex;
        align-items: baseline;
        justify-content: space-between;
        gap: 12px;
      }
      h1 {
        margin: 0;
        font-size: 20px;
        letter-spacing: 0.08em;
        text-transform: uppercase;
      }
      .muted { color: var(--muted); font-size: 13px; }
      .tabs, .filters {
        display: flex;
        gap: 8px;
        margin-top: 12px;
        flex-wrap: wrap;
      }
      .tab, .filter {
        padding: 6px 10px;
        border-radius: 999px;
        border: 1px solid var(--border);
        text-decoration: none;
        color: var(--text);
        font-size: 13px;
      }
      .tab.active, .filter.active {
        background: var(--accent-weak);
        border-color: var(--accent);
        color: var(--accent);
        font-weight: 600;
      }
      main { padding: 20px 24px 32px; }
      .host-summary {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
        gap: 10px;
        margin-bottom: 18px;
      }
      .host-card {
        border: 1px solid var(--border);
        border-radius: 12px;
        padding: 10px 12px;
        background: var(--panel);
      }
      .host-name {
        font-weight: 600;
        display: flex;
        justify-content: space-between;
        align-items: center;
      }
      .host-metrics {
        display: flex;
        gap: 10px;
        margin-top: 6px;
        font-size: 13px;
        color: var(--muted);
      }
      .status-online { color: var(--accent); }
      .status-checking { color: var(--warning); }
      .status-offline { color: var(--danger); }
      .status-unknown { color: var(--muted); }
      .host-dimmed { opacity: 0.5; }
      /* Job status colors - high contrast for white background */
      .job-running { color: #16a34a; }  /* darker green */
      .job-completed { color: var(--text); }
      .job-failed { color: #dc2626; }   /* darker red */
      .job-pending { color: #ca8a04; }  /* darker yellow/amber */
      .job-paused { color: #a21caf; }   /* darker magenta */
      .job-dead { color: #57534e; }     /* darker gray */
      .job-draft { color: #0891b2; }    /* darker cyan */
      /* Tags styling */
      .tags { margin-left: 8px; }
      .tag {
        display: inline-block;
        background: var(--accent-weak);
        color: var(--accent);
        font-size: 11px;
        padding: 2px 6px;
        border-radius: 4px;
        margin-left: 4px;
      }
      /* Tooltip styling - appears below by default to avoid clipping at top */
      .has-tooltip { position: relative; }
      .tooltip {
        display: none;
        position: absolute;
        left: 0;
        top: 100%;
        margin-top: 4px;
        background: var(--panel);
        border: 1px solid var(--border);
        border-radius: 8px;
        padding: 10px 14px;
        box-shadow: 0 4px 12px rgba(0,0,0,0.15);
        z-index: 100;
        min-width: 360px;
        max-width: 500px;
        font-size: 12px;
        white-space: nowrap;
      }
      .has-tooltip:hover .tooltip { display: block; }
      .tooltip-row {
        display: flex;
        gap: 12px;
        margin-bottom: 4px;
      }
      .tooltip-row:last-child { margin-bottom: 0; }
      .tooltip-label {
        color: var(--muted);
        min-width: 70px;
        flex-shrink: 0;
      }
      .tooltip-value {
        font-family: "SF Mono", "Menlo", "Monaco", monospace;
        color: var(--text);
        white-space: pre-wrap;
        word-break: break-all;
      }
      .tooltip-value.wrap { white-space: pre-wrap; max-width: 380px; }
      table {
        width: 100%;
        border-collapse: collapse;
        background: var(--panel);
        border: 1px solid var(--border);
        border-radius: 12px;
        overflow: hidden;
      }
      thead {
        background: #f0ede6;
        text-transform: uppercase;
        letter-spacing: 0.05em;
        font-size: 11px;
        color: var(--muted);
      }
      th, td { padding: 4px 8px; border-bottom: 1px solid var(--border); text-align: left; font-size: 13px; }
      th { padding: 6px 8px; }
      tr:last-child td { border-bottom: none; }
      td.desc { font-weight: 500; }
      .status { font-variant-numeric: tabular-nums; }
      .nowrap { white-space: nowrap; }
      tbody tr:hover { background: #f9f8f6; }
      @media (max-width: 720px) {
        th:nth-child(2), td:nth-child(2),
        th.project-col, td.project-col { display: none; }
      }
    </style>
  </head>
  <body data-refresh="{{.RefreshSeconds}}">
    <header>
      <div class="title-row">
        <h1>{{.Title}}</h1>
        <div class="muted">Last update {{.LastUpdated}}</div>
      </div>
      <div class="tabs">
        {{range .Views}}
          <a class="tab {{if eq $.SelectedView .ID}}active{{end}}" href="{{queryWith $.QueryParams "view" .ID}}">{{.Label}}</a>
        {{end}}
        <a class="tab" href="/cluster" style="margin-left:auto">Cluster</a>
      </div>
      <div class="filters">
        {{range .HostFilters}}
          <a class="filter {{if eq $.SelectedHost .ID}}active{{end}}" href="{{queryWith $.QueryParams "host" .ID}}">{{.Label}}</a>
        {{end}}
      </div>
    </header>
    <main>
      <section class="host-summary">
        {{range .HostSummaries}}
          <div class="host-card{{if .Dimmed}} host-dimmed{{end}}">
            <div class="host-name">
              <span>{{.Name}}</span>
              <span class="{{.Style}}">{{.Status}}</span>
            </div>
            <div class="host-metrics">
              <div>CPU {{.CPU}}</div>
              <div>RAM {{.RAM}}</div>
            </div>
          </div>
        {{end}}
      </section>
      <table>
        <thead>
          <tr>
            <th class="nowrap">ID</th>
            <th class="nowrap">Host</th>
            {{if .ShowProject}}<th class="nowrap project-col">Project</th>{{end}}
            <th class="nowrap">Status</th>
            <th class="nowrap">Time</th>
            {{if .ShowGPU}}<th class="nowrap">GPU</th>{{end}}
            <th>Description</th>
          </tr>
        </thead>
        <tbody>
          {{range .Jobs}}
            <tr class="{{.StatusClass}}">
              <td class="nowrap">{{.ID}}</td>
              <td class="nowrap">{{.Host}}</td>
              {{if $.ShowProject}}<td class="nowrap project-col">{{.Project}}</td>{{end}}
              <td class="status nowrap">{{.Status}}</td>
              <td class="nowrap">{{.Time}}</td>
              {{if $.ShowGPU}}<td class="nowrap">{{.GPU}}</td>{{end}}
              <td class="desc has-tooltip">{{.Description}}{{if .Tags}}<span class="tags">{{range .Tags}}<span class="tag">{{.}}</span>{{end}}</span>{{end}}{{.TooltipHTML}}</td>
            </tr>
          {{end}}
          {{if eq (len .Jobs) 0}}
            <tr><td colspan="99" class="muted">No jobs match this view.</td></tr>
          {{end}}
        </tbody>
      </table>
    </main>
    <script>
      (function() {
        var interval = parseInt(document.body.getAttribute('data-refresh'), 10);
        if (!interval) return;
        var timer = setInterval(refresh, interval * 1000);
        function refresh() {
          fetch(window.location.href)
            .then(function(r) { return r.text(); })
            .then(function(html) {
              var doc = new DOMParser().parseFromString(html, 'text/html');
              var newMain = doc.querySelector('main');
              var oldMain = document.querySelector('main');
              if (newMain && oldMain) oldMain.innerHTML = newMain.innerHTML;
              var newMuted = doc.querySelector('.title-row .muted');
              var oldMuted = document.querySelector('.title-row .muted');
              if (newMuted && oldMuted) oldMuted.textContent = newMuted.textContent;
              var newInterval = parseInt(doc.body.getAttribute('data-refresh'), 10);
              if (newInterval && newInterval !== interval) {
                clearInterval(timer);
                interval = newInterval;
                document.body.setAttribute('data-refresh', newInterval);
                timer = setInterval(refresh, interval * 1000);
              }
            })
            .catch(function() {});
        }
      })();
    </script>
  </body>
</html>
`
