// The overview: every configured database on one screen.
//
// Each row carries a finding rather than only a name. A list of names is
// navigation; a list that says which database has the worst problem answers
// the question that brought the user here.

import { h, text, clear, fmt } from '/dom.js';
import { getClusters, navigate } from '/app.js';
import { openClusterForm } from '/views/form.js';
import { removeButton } from '/views/remove.js';

export async function renderOverview(root) {
  clear(root);
  const clusters = getClusters();

  if (!clusters.length) {
    root.append(emptyState());
    return null;
  }

  const page = h('div', { class: 'page' },
    h('div', { class: 'page-head' },
      h('div', {},
        h('h1', {}, text('Databases')),
        h('div', { class: 'sub' }, text(summarize(clusters)))),
      h('div', { class: 'spacer' }),
      h('button', { class: 'btn btn-primary', onclick: () => openClusterForm() },
        text('Add database'))),
    table(clusters));

  root.append(page);
  return null;
}

function summarize(clusters) {
  const problems = clusters.filter((c) => c.last_run && c.last_run.recommendations > 0).length;
  const unchecked = clusters.filter((c) => !c.last_run).length;

  const parts = [`${clusters.length} configured`];
  if (problems) parts.push(`${problems} with missing indexes`);
  if (unchecked) parts.push(`${unchecked} not yet analysed`);
  return parts.join(' · ');
}

function table(clusters) {
  const body = h('tbody');

  for (const c of clusters) {
    const open = () => navigate(`/db/${encodeURIComponent(c.id)}`);

    const status = h('td', {},
      h('div', { class: 'row' },
        h('span', { class: 'dot ' + dotClass(c) }),
        h('span', { class: 'faint' }, text(c.status ? c.status.summary : 'checking'))));

    const name = h('td', {},
      h('div', { class: 'row' },
        h('strong', {}, text(c.name)),
        c.last_run && c.last_run.new_since_last > 0
          ? h('span', { class: 'badge-new' }, text(`${c.last_run.new_since_last} new`))
          : null),
      h('div', { class: 'faint' }, text(c.nodes.map((n) => n.label).join(', '))));

    const finding = h('td', {}, text(c.headline || '—'));

    const analysed = h('td', { class: 'nowrap faint' },
      text(c.last_run ? fmt.ago(c.last_run.ended_at) : 'never'));

    const schedule = h('td', { class: 'nowrap faint' },
      text(c.schedule && c.schedule.describe ? c.schedule.describe : 'only when you ask'));

    // The row actions stay quiet until the row is hovered or focused, so the
    // table reads as information rather than as a wall of buttons.
    const actions = h('td', { class: 'row-actions nowrap' },
      h('button', {
        class: 'btn btn-sm',
        onclick: (e) => { e.stopPropagation(); openClusterForm(c); },
      }, text('Edit')),
      removeButton(c));

    const row = h('tr', {
      onclick: open,
      style: 'cursor:pointer',
      tabindex: '0',
      onkeydown: (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); open(); } },
    }, status, name, finding, analysed, schedule, actions);

    body.append(row);
  }

  return h('div', { class: 'table-wrap' },
    h('table', {},
      h('thead', {}, h('tr', {},
        h('th', {}, text('Status')),
        h('th', {}, text('Database')),
        h('th', {}, text('Findings')),
        h('th', {}, text('Last analysed')),
        h('th', {}, text('Schedule')),
        h('th', {}, text('')))),
      body));
}

function dotClass(c) {
  if (!c.status) return 'dot-pending';
  if (c.status.state === 'error') return 'dot-error';
  if (c.status.state === 'warning') return 'dot-warn';
  return 'dot-ok';
}

// The first thing anyone sees. It says what the tool does, promises that it
// will not write to their database, and offers exactly one action.
function emptyState() {
  return h('div', { class: 'page' },
    h('div', { class: 'empty' },
      h('div', { class: 'empty-mark' }),
      h('h1', {}, text('Find the indexes your queries are missing')),
      h('p', {}, text(
        'idxa reads your query statistics, works out which indexes would help, '
        + 'and proves each one against the planner. It only ever reads: it will never '
        + 'create, drop or change anything in your database.')),
      h('button', {
        class: 'btn btn-primary',
        onclick: () => openClusterForm(),
      }, text('Add your first database')),
      h('p', { class: 'hint', style: 'margin-top:16px' },
        text('Press n at any time to add another.'))));
}
