package web

const indexTemplate = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <title>{{.Title}}</title>
    <meta name="viewport" content="width=device-width, initial-scale=1">
    {{if gt .RefreshSeconds 0}}<meta http-equiv="refresh" content="{{.RefreshSeconds}}">{{end}}
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
      th, td { padding: 10px 12px; border-bottom: 1px solid var(--border); text-align: left; }
      tr:last-child td { border-bottom: none; }
      td.desc { font-weight: 500; }
      .status { font-variant-numeric: tabular-nums; }
      .nowrap { white-space: nowrap; }
      @media (max-width: 720px) {
        th:nth-child(2), td:nth-child(2) { display: none; }
      }
    </style>
  </head>
  <body>
    <header>
      <div class="title-row">
        <h1>{{.Title}}</h1>
        <div class="muted">Last update {{.LastUpdated}}</div>
      </div>
      <div class="tabs">
        {{range .Views}}
          <a class="tab {{if eq $.SelectedView .ID}}active{{end}}" href="{{queryWith $.QueryParams "view" .ID}}">{{.Label}}</a>
        {{end}}
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
          <div class="host-card">
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
            <th class="nowrap">Status</th>
            <th class="nowrap">Time</th>
            {{if .ShowGPU}}<th class="nowrap">GPU</th>{{end}}
            <th>Description</th>
          </tr>
        </thead>
        <tbody>
          {{range .Jobs}}
            <tr>
              <td class="nowrap">{{.ID}}</td>
              <td class="nowrap">{{.Host}}</td>
              <td class="status nowrap">{{.Status}}</td>
              <td class="nowrap">{{.Time}}</td>
              {{if $.ShowGPU}}<td class="nowrap">{{.GPU}}</td>{{end}}
              <td class="desc">{{.Description}}</td>
            </tr>
          {{end}}
          {{if eq (len .Jobs) 0}}
            <tr><td colspan="{{if .ShowGPU}}6{{else}}5{{end}}" class="muted">No jobs match this view.</td></tr>
          {{end}}
        </tbody>
      </table>
    </main>
  </body>
</html>
`
