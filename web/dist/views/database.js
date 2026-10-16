// One database: live metrics, recommendations, unused and redundant indexes,
// and settings.
//
// The page opens on data rather than a menu of buttons. Making someone press
// Run before they see anything wastes the visit, and the live view is useful
// on its own.

import { h, text, clear, fill, setText, fmt, sparkline } from '/dom.js';
import { api } from '/api.js';
import { getCluster, navigate, toast, refreshClusters } from '/app.js';
import { renderRecommendations, renderUnused, renderDuplicates } from '/views/findings.js';
import { renderSettings } from '/views/settings.js';
import { openClusterForm } from '/views/form.js';
import { removeAndGoHome } from '/views/remove.js';

const TABS = [
  { id: 'live', label: 'Live' },
  { id: 'recommendations', label: 'Recommendations' },
  { id: 'unused', label: 'Unused' },
  { id: 'duplicates', label: 'Redundant' },
  { id: 'settings', label: 'Settings' },
];

export async function renderDatabase(root, id, tab) {
  clear(root);
  const cluster = getCluster(id);
  if (!cluster) return null;

  const page = h('div', { class: 'page' });
  root.append(page);

  const state = {
    id,
    overview: null,
    run: null,
    disposers: [],
  };

  const head = h('div', { class: 'page-head' });
  const tabBar = h('div', { class: 'tabs', role: 'tablist' });
  const panel = h('div', { id: 'tab-panel', role: 'tabpanel' });
  page.append(head, tabBar, panel);

  renderHead(head, cluster, state);
  renderTabs(tabBar, id, tab, null);

  // Show the tab immediately from what is already known, then fill in.
  await mountTab(panel, state, tab);

  // Load the saved analysis and status in the background.
  api.get(`/api/clusters/${encodeURIComponent(id)}/overview`)
    .then((data) => {
      state.overview = data;
      state.run = data.run;
      renderHead(head, data.cluster || cluster, state);
      renderTabs(tabBar, id, tab, data.run);
      if (tab !== 'live') mountTab(panel, state, tab);
    })
    .catch((err) => toast(err.message, 'error'));

  return () => {
    for (const dispose of state.disposers) dispose();
    state.disposers = [];
  };
}

// ------------------------------------------------------------------ header

function renderHead(head, cluster, state) {
  clear(head);

  const status = cluster.status;
  const dot = h('span', {
    class: 'dot ' + (!status ? 'dot-pending'
      : status.state === 'error' ? 'dot-error'
        : status.state === 'warning' ? 'dot-warn' : 'dot-ok'),
  });

  const nodes = (cluster.nodes || []).map((n) => n.label).join(', ');
  const lastRun = state.run
    ? `analysed ${fmt.ago(state.run.ended_at)}`
    : 'not analysed yet';

  head.append(
    h('div', {},
      h('h1', {}, text(cluster.name)),
      h('div', { class: 'sub row wrap' },
        dot,
        h('span', {}, text(status ? status.summary : 'checking')),
        h('span', { class: 'faint' }, text('·')),
        h('span', { class: 'faint mono' }, text(nodes)),
        h('span', { class: 'faint' }, text('·')),
        h('span', { class: 'faint' }, text(lastRun)))),
    h('div', { class: 'spacer' }),
    h('div', { class: 'row' },
      h('span', { class: 'pill pill-ok', title: 'idxa never writes to your database' },
        text('read only')),
      h('button', {
        class: 'btn',
        onclick: () => openClusterForm(cluster),
      }, text('Edit')),
      removeAndGoHome(cluster, { class: 'btn btn-danger' }),
      h('button', {
        class: 'btn btn-primary',
        id: 'run-analysis',
        onclick: () => startRun(state),
      }, text('Run analysis'))));
}

// ------------------------------------------------------------------- tabs

function renderTabs(bar, id, active, run) {
  clear(bar);
  const counts = {
    recommendations: run && run.report ? (run.report.recommendations || []).length : null,
    unused: run && run.report ? (run.report.unused || []).length : null,
    duplicates: run && run.report ? (run.report.duplicates || []).length : null,
  };

  for (const t of TABS) {
    const count = counts[t.id];
    bar.append(h('button', {
      class: 'tab',
      role: 'tab',
      'aria-selected': String(t.id === active),
      onclick: () => navigate(`/db/${encodeURIComponent(id)}/${t.id}`),
    },
      text(t.label),
      count !== null && count !== undefined
        ? h('span', { class: 'tab-count' }, text(String(count)))
        : null));
  }
}

async function mountTab(panel, state, tab) {
  // Tearing down the previous tab's stream matters: an open live stream keeps
  // the server querying the database.
  for (const dispose of state.disposers) dispose();
  state.disposers = [];

  clear(panel);
  switch (tab) {
    case 'recommendations':
      panel.append(renderRecommendations(state.run, (seconds) => startRun(state, seconds)));
      break;
    case 'unused':
      panel.append(renderUnused(state.run));
      break;
    case 'duplicates':
      panel.append(renderDuplicates(state.run));
      break;
    case 'settings':
      panel.append(await renderSettings(state.id));
      break;
    default:
      state.disposers.push(mountLive(panel, state));
  }
}

// ------------------------------------------------------------------- live

function mountLive(panel, state) {
  const tiles = h('div', { class: 'tiles' });
  const tableWrap = h('div', { class: 'table-wrap' });
  const idleNote = h('div', { class: 'hint', style: 'margin-bottom:8px' });
  const warning = h('div', {});
  const costLine = h('div', { class: 'hint', style: 'margin-top:12px' });

  panel.append(
    warning,
    tiles,
    h('div', { class: 'row', style: 'margin-bottom:8px' },
      h('h2', { style: 'margin:0' }, text('Tables under sequential-scan pressure')),
      h('div', { class: 'spacer' }),
      h('span', { class: 'hint', id: 'live-updated' }, text('connecting…'))),
    idleNote,
    tableWrap,
    costLine);

  const tileDefs = [
    { key: 'seq_tuples_per_sec', label: 'Rows scanned', format: fmt.rate, note: 'sequentially, per second' },
    { key: 'seq_scans_per_sec', label: 'Sequential scans', format: fmt.rate, note: 'started per second' },
    { key: 'cache_hit_ratio', label: 'Cache hit', format: (v) => fmt.percent(v, 1), note: 'of block reads' },
    { key: 'transactions_per_sec', label: 'Transactions', format: fmt.rate, note: 'committed and rolled back' },
    { key: 'active_queries', label: 'Active', format: (v) => fmt.count(v), note: 'queries running now' },
    { key: 'connections', label: 'Connections', format: (v) => fmt.count(v), note: 'to this database' },
  ];

  const tileValues = {};
  for (const def of tileDefs) {
    const value = h('div', { class: 'tile-value' }, text('—'));
    tileValues[def.key] = value;
    tiles.append(h('div', { class: 'tile' },
      h('div', { class: 'tile-label' }, text(def.label)),
      value,
      h('div', { class: 'tile-note' }, text(def.note))));
  }

  const body = h('tbody');
  tableWrap.append(h('table', { class: 'live-table' },
    h('colgroup', {},
      h('col', {}), h('col', { class: 'c-rate' }), h('col', { class: 'c-rate' }),
      h('col', { class: 'c-rate' }), h('col', { class: 'c-rate' }),
      h('col', { class: 'c-size' }), h('col', { style: 'width:96px' })),
    h('thead', {}, h('tr', {},
      h('th', {}, text('Table')),
      h('th', { class: 'num' }, text('Rows scanned/s')),
      h('th', { class: 'num' }, text('Seq scans/s')),
      h('th', { class: 'num' }, text('Index scans/s')),
      h('th', { class: 'num' }, text('Writes/s')),
      h('th', { class: 'num' }, text('Size')),
      h('th', {}, text('Trend')))),
    body));

  // Rows are kept and patched rather than rebuilt, so numbers update without
  // the table flickering or losing the row under the pointer.
  const rows = new Map();
  const updated = panel.querySelector('#live-updated');

  function applySnapshot(snap) {
    if (snap.warning) {
      fill(warning, h('div', { class: 'notice notice-warn' }, text(snap.warning)));
    } else {
      clear(warning);
    }

    for (const def of tileDefs) {
      setText(tileValues[def.key], def.format(snap[def.key]));
    }
    setText(updated, `updated ${new Date(snap.at).toLocaleTimeString()}`);

    if (snap.cost) {
      setText(costLine,
        `idxa has used ${fmt.ms(snap.cost.total_ms)} of this server's time across `
        + `${snap.cost.statements} statements, ${snap.cost.duty_percent.toFixed(2)}% of the clock. `
        + `Next reading in ${snap.cost.cooldown_seconds.toFixed(1)}s.`);
    }

    const seen = new Set();
    let index = 0;
    for (const t of snap.tables || []) {
      const key = `${t.schema}.${t.table}`;
      seen.add(key);
      let row = rows.get(key);
      if (!row) {
        row = buildRow(key);
        rows.set(key, row);
      }
      patchRow(row, t);
      // Keep the DOM order matching the sort without rebuilding the table.
      if (body.children[index] !== row.tr) body.insertBefore(row.tr, body.children[index] || null);
      index++;
    }
    for (const [key, row] of rows) {
      if (!seen.has(key)) { row.tr.remove(); rows.delete(key); }
    }

    if (!rows.size) {
      fill(body, h('tr', {}, h('td', { colspan: '7', class: 'faint' },
        text('No tables to show yet.'))));
      return;
    }

    // When nothing is moving, say so rather than presenting a wall of zeros
    // that the reader has to scan to reach the same conclusion.
    const anyActivity = (snap.tables || []).some(
      (t) => t.seq_tuples_per_sec > 0 || t.seq_scans_per_sec > 0 || t.idx_scans_per_sec > 0
        || t.writes_per_sec > 0);
    setText(idleNote, anyActivity ? '' : 'No queries are running against this database right now.');
  }

  const stop = api.stream(`/api/clusters/${encodeURIComponent(state.id)}/live`, {
    onEvent: (_type, snap) => applySnapshot(snap),
    onError: (err) => setText(updated, err.message),
  });

  // Close the stream while the tab is hidden. An unattended browser tab must
  // not keep a database busy.
  let paused = null;
  const onVisibility = () => {
    if (document.hidden) {
      if (!paused) { paused = true; stopStream(); setText(updated, 'paused while this tab is hidden'); }
    } else if (paused) {
      paused = false;
      restart();
    }
  };
  let current = stop;
  function stopStream() { if (current) { current(); current = null; } }
  function restart() {
    current = api.stream(`/api/clusters/${encodeURIComponent(state.id)}/live`, {
      onEvent: (_type, snap) => applySnapshot(snap),
      onError: (err) => setText(updated, err.message),
    });
  }
  document.addEventListener('visibilitychange', onVisibility);

  return () => {
    document.removeEventListener('visibilitychange', onVisibility);
    stopStream();
  };
}

function buildRow(name) {
  const cells = {
    name: h('td', {}, h('span', { class: 'mono' }, text(name))),
    seqTuples: h('td', { class: 'num' }, text('—')),
    seqScans: h('td', { class: 'num' }, text('—')),
    idxScans: h('td', { class: 'num' }, text('—')),
    writes: h('td', { class: 'num' }, text('—')),
    size: h('td', { class: 'num faint' }, text('—')),
    spark: h('td', {}),
  };
  const tr = h('tr', {}, cells.name, cells.seqTuples, cells.seqScans,
    cells.idxScans, cells.writes, cells.size, cells.spark);
  return { tr, cells };
}

function patchRow(row, t) {
  // A zero is rendered faintly so the eye lands on the values that are not
  // zero, which is the entire point of the view.
  rate(row.cells.seqTuples, t.seq_tuples_per_sec);
  rate(row.cells.seqScans, t.seq_scans_per_sec);
  rate(row.cells.idxScans, t.idx_scans_per_sec);
  rate(row.cells.writes, t.writes_per_sec);
  setText(row.cells.size, fmt.bytes(t.bytes));
  fill(row.cells.spark, sparkline(t.sparkline));
}

function rate(cell, value) {
  setText(cell, value ? fmt.count(value) : '0');
  const cls = value ? 'num' : 'num faint';
  if (cell.className !== cls) cell.className = cls;
}

// -------------------------------------------------------------- run an analysis

async function startRun(state, sampleSeconds = 0) {
  const button = document.getElementById('run-analysis');
  if (button) { button.disabled = true; button.textContent = 'Starting…'; }

  try {
    const run = await api.post(`/api/clusters/${encodeURIComponent(state.id)}/analyze`,
      sampleSeconds ? { sample_seconds: sampleSeconds } : {});
    showProgress(state, run);
  } catch (err) {
    toast(err.message, 'error');
    if (button) { button.disabled = false; button.textContent = 'Run analysis'; }
  }
}

// Progress is shown as a list of named phases rather than a bar, because the
// phases say what is happening and a bar only says how long is left.
function showProgress(state, run) {
  const panel = document.getElementById('tab-panel');
  if (!panel) return;

  const list = h('ul', { class: 'phases' });
  const heading = h('h2', { style: 'margin-top:0' }, text('Analysing'));
  const progress = h('div', { class: 'hint', style: 'margin:8px 0' });
  const note = h('div', { class: 'hint' },
    text('One pass over the statistics produces all three reports, so your server is only read once.'));
  const cancel = h('button', {
    class: 'btn btn-sm',
    style: 'margin-top:12px',
    onclick: async () => {
      try { await api.delete(`/api/runs/${encodeURIComponent(run.id)}`); } catch { /* already finished */ }
    },
  }, text('Stop'));
  const card = h('div', { class: 'card' }, heading, list, progress, note, cancel);

  clear(panel);
  panel.append(card);

  const knownPhases = [
    'connecting', 'reading statistics', 'watching queries', 'reading the workload',
    'parsing queries', 'proposing indexes', 'scoring', 'confirming with the planner',
  ];
  const items = new Map();
  for (const name of knownPhases) {
    const item = h('li', { class: 'phase' },
      h('span', { class: 'phase-mark' }, text('·')),
      h('span', {}, text(name)));
    items.set(name, item);
    list.append(item);
  }

  const stop = api.stream(`/api/runs/${encodeURIComponent(run.id)}/events`, {
    onEvent: async (_type, update) => {
      const currentIndex = knownPhases.indexOf(update.phase);
      knownPhases.forEach((name, i) => {
        const item = items.get(name);
        const done = currentIndex === -1 ? update.status === 'complete' : i < currentIndex;
        const active = i === currentIndex;
        item.className = 'phase' + (active ? ' active' : done ? ' done' : '');
        item.firstChild.textContent = done ? '✓' : active ? '›' : '·';
      });

      // A recording takes minutes, so say what it has seen so far rather
      // than leaving a phase list sitting still.
      setText(progress, update.sample_progress || '');

      if (update.status === 'canceled') {
        stop();
        clear(panel);
        panel.append(h('div', { class: 'notice notice-warn' }, text('Analysis stopped.')));
        const button = document.getElementById('run-analysis');
        if (button) { button.disabled = false; button.textContent = 'Run analysis'; }
        return;
      }

      if (update.status === 'complete') {
        stop();
        state.run = update;
        toast(summarizeRun(update), 'ok');
        await refreshClusters({ silent: true });
        navigate(`/db/${encodeURIComponent(state.id)}/recommendations`);
      } else if (update.status === 'failed') {
        stop();
        clear(panel);
        panel.append(h('div', { class: 'notice notice-error' },
          text(update.error || 'the analysis failed')));
        const button = document.getElementById('run-analysis');
        if (button) { button.disabled = false; button.textContent = 'Run analysis'; }
      }
    },
    onError: () => {},
  });

  state.disposers.push(stop);
}

function summarizeRun(run) {
  if (!run.report) return 'Analysis finished';
  const recs = (run.report.recommendations || []).length;
  const unused = (run.report.unused || []).length;
  const dups = (run.report.duplicates || []).length;
  if (!recs && !unused && !dups) return 'Analysis finished: nothing to change';
  const parts = [];
  if (recs) parts.push(`${recs} index${recs === 1 ? '' : 'es'} to add`);
  if (unused) parts.push(`${unused} unused`);
  if (dups) parts.push(`${dups} redundant`);
  return `Analysis finished: ${parts.join(', ')}`;
}
