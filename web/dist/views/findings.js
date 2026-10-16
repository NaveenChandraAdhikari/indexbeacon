// The three findings tabs.
//
// Every recommendation explains itself. A row that says only "add this index"
// asks the reader to trust a number they cannot check; showing which queries
// it helps, how selective it is, and what the planner said about it lets them
// judge for themselves.

import { h, text, clear, fmt, copy } from '/dom.js';
import { api } from '/api.js';
import { toast } from '/app.js';

// ------------------------------------------------------------ empty states

function emptyCard(title, body, extra) {
  return h('div', { class: 'card' },
    h('h3', { style: 'margin-top:0' }, text(title)),
    h('p', { class: 'muted', style: 'margin:0' }, text(body)),
    extra || null);
}

// The state a database without pg_stat_statements lands in. It has to offer a
// way forward rather than naming an extension the reader may not be able to
// install: watching live queries needs no change to the database at all.
function noWorkloadCard(onRecord) {
  const card = h('div', { class: 'card' },
    h('h3', { style: 'margin-top:0' }, text('idxa cannot see which queries run here')),
    h('p', { class: 'muted' }, text(
      'Without that, it can report unused and redundant indexes but not missing ones. '
      + 'There are two ways to fix it, and the first needs no change to your database.')),

    h('h3', {}, text('Watch the queries for a few minutes')),
    h('p', { class: 'muted', style: 'margin-top:0' }, text(
      'idxa samples what is running and builds a picture of the workload. A query that '
      + 'runs constantly is seen in most samples; one that runs rarely is seen in few. '
      + 'The counts are estimates, so anything found this way is marked lower confidence. '
      + 'Record while the application is under its normal load.')));

  if (onRecord) {
    const row = h('div', { class: 'row wrap', style: 'margin-bottom:8px' });
    for (const [label, seconds] of [['2 minutes', 120], ['5 minutes', 300], ['10 minutes', 600]]) {
      row.append(h('button', {
        class: 'btn btn-primary',
        onclick: () => onRecord(seconds),
      }, text(`Record for ${label}`)));
    }
    card.append(row);
    card.append(h('div', { class: 'hint' }, text(
      'You can stop a recording at any time and keep what it has seen so far.')));
  }

  card.append(
    h('h3', {}, text('Or install pg_stat_statements')),
    h('p', { class: 'muted', style: 'margin-top:0' }, text(
      'It gives exact call counts rather than estimates. It is per database, so it has to '
      + 'be created in each one, and the library must be loaded at server start.')),
    h('pre', { class: 'sql' }, text(
      "-- once per server, in postgresql.conf, then restart\n"
      + "shared_preload_libraries = 'pg_stat_statements'\n\n"
      + '-- then once per database\n'
      + 'CREATE EXTENSION pg_stat_statements;')));

  return card;
}

function noRunYet() {
  return emptyCard(
    'Not analysed yet',
    'Press Run analysis to look at this database. It reads statistics only, and takes a few seconds.');
}

// --------------------------------------------------------- recommendations

export function renderRecommendations(run, onRecord) {
  if (!run || !run.report) return noRunYet();

  const report = run.report;
  const recs = report.recommendations || [];
  const wrap = h('div', {});

  wrap.append(runBanner(run));

  for (const warning of report.warnings || []) {
    wrap.append(h('div', { class: 'notice notice-warn' }, text(warning)));
  }

  if (!recs.length) {
    if (report.mode === 'table-only') {
      wrap.append(noWorkloadCard(onRecord));
    } else {
      wrap.append(emptyCard(
        'No missing indexes found',
        'Every query idxa could see is already served by an index. '
        + 'The unused and redundant tabs may still have something worth removing.'));
    }
    if ((report.hot_tables || []).length) wrap.append(hotTables(report.hot_tables));
    return wrap;
  }

  // A single action for everything, since a person acting on this list wants
  // the whole script, not one statement at a time.
  wrap.append(h('div', { class: 'row wrap', style: 'margin-bottom:12px' },
    h('button', {
      class: 'btn',
      onclick: async () => {
        const sql = recs.map((r) => r.create_sql).join('\n\n');
        toast(await copy(sql) ? `Copied ${recs.length} statements` : 'Could not copy', 'ok');
      },
    }, text('Copy all statements')),
    h('a', {
      class: 'btn',
      href: api.url(`/api/runs/${encodeURIComponent(run.id)}/export?format=sql`),
      download: '',
    }, text('Download .sql')),
    h('a', {
      class: 'btn',
      href: api.url(`/api/runs/${encodeURIComponent(run.id)}/export?format=markdown`),
      download: '',
    }, text('Download report')),
    h('div', { class: 'spacer' }),
    h('span', { class: 'hint' },
      text('idxa does not run these. Review them, then apply them yourself.'))));

  const body = h('tbody');
  for (const rec of recs) {
    body.append(...recommendationRows(rec, run));
  }

  wrap.append(h('div', { class: 'table-wrap' },
    h('table', {},
      h('thead', {}, h('tr', {},
        h('th', { style: 'width:28px' }, text('')),
        h('th', {}, text('Index')),
        h('th', { class: 'num' }, text('Queries')),
        h('th', { class: 'num' }, text('Est. size')),
        h('th', {}, text('Confidence')),
        h('th', {}, text('Planner')))),
      body)));

  return wrap;
}

function recommendationRows(rec, run) {
  const isNew = run.changes && (run.changes.new_recommendations || [])
    .some((k) => k.includes(`${rec.schema}.${rec.table}`) && k.includes(rec.columns.join(', ')));

  const detail = h('tr', { hidden: true }, h('td', { colspan: '6', class: 'rec-detail' }));
  const toggle = h('button', {
    class: 'btn-icon expander',
    'aria-expanded': 'false',
    'aria-label': 'Show details',
    onclick: (e) => {
      const open = detail.hidden;
      detail.hidden = !open;
      e.currentTarget.setAttribute('aria-expanded', String(open));
      e.currentTarget.textContent = open ? '▾' : '▸';
      if (open && !detail.firstChild.hasChildNodes()) {
        detail.firstChild.append(recommendationDetail(rec));
      }
    },
  }, text('▸'));

  const planner = rec.validated
    ? (rec.plan_uses_index
      ? h('span', { class: 'pill pill-ok', title: 'the planner chose this index when it was offered' },
        text('confirmed'))
      : h('span', { class: 'pill pill-warn', title: 'the planner declined to use it' },
        text('not used')))
    : h('span', { class: 'pill', title: 'hypopg was not available, so this is an estimate' },
      text('estimated'));

  const confidence = h('span', {
    class: 'pill ' + (rec.confidence === 'high' ? 'pill-ok'
      : rec.confidence === 'medium' ? 'pill-info' : 'pill-warn'),
  }, text(rec.confidence || 'unknown'));

  const main = h('tr', {},
    h('td', {}, toggle),
    h('td', {},
      h('div', { class: 'row' },
        h('span', { class: 'mono' }, text(`${rec.schema}.${rec.table}`)),
        isNew ? h('span', { class: 'badge-new' }, text('new')) : null),
      h('div', { class: 'mono faint' }, text(`(${rec.columns.join(', ')})`)),
      rec.predicate ? h('div', { class: 'mono faint' }, text(`WHERE ${rec.predicate}`)) : null),
    h('td', { class: 'num' }, text(String((rec.queries || []).length))),
    h('td', { class: 'num' }, text(fmt.bytes(rec.estimated_bytes))),
    h('td', {}, confidence),
    h('td', {}, planner));

  return [main, detail];
}

function recommendationDetail(rec) {
  const wrap = h('div', {});

  const sql = h('pre', { class: 'sql' }, text(rec.create_sql));
  wrap.append(
    h('div', { class: 'row', style: 'margin-bottom:4px' },
      h('strong', {}, text('Statement')),
      h('div', { class: 'spacer' }),
      h('button', {
        class: 'btn btn-sm',
        onclick: async () => toast(await copy(rec.create_sql) ? 'Copied' : 'Could not copy', 'ok'),
      }, text('Copy'))),
    sql,
    h('div', { class: 'hint' }, text(
      'CONCURRENTLY lets the index build without blocking writes. It takes longer '
      + 'and cannot run inside a transaction block.')));

  if ((rec.notes || []).length) {
    wrap.append(h('strong', {}, text('Why')),
      h('ul', {}, ...rec.notes.map((n) => h('li', {}, text(n)))));
  }

  if ((rec.queries || []).length) {
    wrap.append(h('strong', {}, text('Queries this would help')));
    const list = h('div', { class: 'stack' });
    for (const q of rec.queries.slice(0, 5)) {
      list.append(h('div', {},
        h('div', { class: 'faint' },
          text(`${fmt.count(q.calls)} calls, ${fmt.ms(q.total_time_ms)} total`)),
        h('pre', { class: 'sql' }, text(q.query))));
    }
    wrap.append(list);
  }

  if ((rec.alternatives || []).length) {
    wrap.append(h('div', { class: 'hint' },
      text(`A narrower index would also work: ${rec.alternatives.join(', ')}`)));
  }
  return wrap;
}

function runBanner(run) {
  const report = run.report || {};
  const bits = [
    `analysed ${fmt.ago(run.ended_at)}`,
    run.trigger === 'scheduled' ? 'on a schedule' : 'when you asked',
  ];
  if (report.mode === 'validated') bits.push('confirmed against the planner');
  else if (report.mode === 'heuristic') bits.push('estimated from statistics');

  const changes = run.changes || {};
  const banner = h('div', { class: 'row wrap', style: 'margin-bottom:12px' },
    h('span', { class: 'hint' }, text(bits.join(' · '))));

  if ((changes.new_recommendations || []).length) {
    banner.append(h('span', { class: 'pill pill-info' },
      text(`${changes.new_recommendations.length} new since the last run`)));
  }
  if ((changes.resolved_recommendations || []).length) {
    banner.append(h('span', { class: 'pill pill-ok' },
      text(`${changes.resolved_recommendations.length} resolved`)));
  }
  return banner;
}

function hotTables(tables) {
  const body = h('tbody');
  for (const t of tables.slice(0, 10)) {
    body.append(h('tr', {},
      h('td', { class: 'mono' }, text(`${t.schema}.${t.table}`)),
      h('td', { class: 'num' }, text(fmt.count(t.rows))),
      h('td', { class: 'num' }, text(fmt.count(t.seq_scans))),
      h('td', { class: 'num' }, text(fmt.count(t.seq_tup_read))),
      h('td', { class: 'num faint' }, text(fmt.bytes(t.table_bytes)))));
  }
  return h('div', { style: 'margin-top:16px' },
    h('h3', {}, text('Tables being scanned the most')),
    h('div', { class: 'table-wrap' },
      h('table', {},
        h('thead', {}, h('tr', {},
          h('th', {}, text('Table')),
          h('th', { class: 'num' }, text('Rows')),
          h('th', { class: 'num' }, text('Seq scans')),
          h('th', { class: 'num' }, text('Rows read')),
          h('th', { class: 'num' }, text('Size')))),
        body)));
}

// ----------------------------------------------------------------- unused

export function renderUnused(run) {
  if (!run || !run.report) return noRunYet();
  const items = run.report.unused || [];

  if (!items.length) {
    return emptyCard('No unused indexes',
      'Every index on this database has been used at least once since statistics were last reset.');
  }

  const total = items.reduce((sum, u) => sum + (u.size_bytes || 0), 0);
  const body = h('tbody');

  // A schema with thousands of tables produces thousands of findings, and a
  // table that long is slow to render and useless to read. Show the biggest
  // and say how many were left out.
  const LIMIT = 200;
  const shown = items.slice(0, LIMIT);

  for (const u of shown) {
    const scans = (u.scans_per_node || [])
      .map((s) => `${s.node}: ${s.count}`).join(', ');
    body.append(h('tr', {},
      h('td', { class: 'mono' }, text(u.index)),
      h('td', { class: 'mono faint' }, text(`${u.schema}.${u.table}`)),
      h('td', { class: 'num' }, text(fmt.bytes(u.size_bytes))),
      h('td', { class: 'faint', title: scans }, text(scans || '0')),
      h('td', {}, h('span', {
        class: 'pill ' + (u.confidence === 'high' ? 'pill-ok' : 'pill-warn'),
      }, text(u.confidence))),
      h('td', {}, h('button', {
        class: 'btn btn-sm',
        onclick: async () => toast(await copy(u.drop_sql) ? 'Copied' : 'Could not copy', 'ok'),
      }, text('Copy drop')))));
  }

  return h('div', {},
    items.length > LIMIT
      ? h('div', { class: 'notice notice-warn' },
        text(`Showing the ${LIMIT} largest of ${items.length} unused indexes. `
          + 'Download the report for the full list.'))
      : null,
    h('div', { class: 'notice notice-info' },
      text(`Dropping these would reclaim ${fmt.bytes(total)}. `
        + 'Counters are summed across every node you configured, so an index used only on a '
        + 'replica is not listed here. Check that statistics have been collecting long enough '
        + 'to cover your monthly and quarterly jobs before dropping anything.')),
    h('div', { class: 'table-wrap' },
      h('table', {},
        h('thead', {}, h('tr', {},
          h('th', {}, text('Index')),
          h('th', {}, text('Table')),
          h('th', { class: 'num' }, text('Size')),
          h('th', {}, text('Scans per node')),
          h('th', {}, text('Confidence')),
          h('th', {}, text('')))),
        body)));
}

// ------------------------------------------------------------- duplicates

export function renderDuplicates(run) {
  if (!run || !run.report) return noRunYet();
  const items = run.report.duplicates || [];

  if (!items.length) {
    return emptyCard('No redundant indexes',
      'No index on this database is an exact copy of, or a leading subset of, another.');
  }

  const total = items.reduce((sum, d) => sum + (d.redundant_size_bytes || 0), 0);
  const body = h('tbody');

  for (const d of items) {
    body.append(h('tr', {},
      h('td', { class: 'mono' }, text(d.redundant_index)),
      h('td', { class: 'mono faint' }, text(d.covered_by)),
      h('td', {}, h('span', { class: 'pill' }, text(d.kind))),
      h('td', { class: 'num' }, text(fmt.bytes(d.redundant_size_bytes))),
      h('td', { class: 'faint' }, text(d.reason)),
      h('td', {}, d.drop_sql ? h('button', {
        class: 'btn btn-sm',
        onclick: async () => toast(await copy(d.drop_sql) ? 'Copied' : 'Could not copy', 'ok'),
      }, text('Copy drop')) : null)));
  }

  return h('div', {},
    h('div', { class: 'notice notice-info' },
      text(`Dropping the redundant indexes would reclaim ${fmt.bytes(total)} `
        + 'and remove that much work from every insert and update. '
        + 'Primary keys and unique constraints are never listed here.')),
    h('div', { class: 'table-wrap' },
      h('table', {},
        h('thead', {}, h('tr', {},
          h('th', {}, text('Redundant index')),
          h('th', {}, text('Already covered by')),
          h('th', {}, text('Kind')),
          h('th', { class: 'num' }, text('Size')),
          h('th', {}, text('Why')),
          h('th', {}, text('')))),
        body)));
}
