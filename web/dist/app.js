// idxa web interface.
//
// Plain ES modules, no framework and no build step. The whole interface ships
// inside the binary, so a dependency here would mean anyone wanting to change
// a colour first has to install a toolchain.
//
// Two rules shape the code:
//
//   * The page paints before it touches a database. The shell and sidebar
//     render from configuration alone; panels fill in as answers arrive. A
//     server that is down must never leave the interface blank.
//   * Live numbers update in place. Tables keep fixed column widths, a stable
//     sort order, and their rows are patched cell by cell rather than
//     rebuilt, so a value changing from 9 to 10 does not make the page jump.

import { h, text, clear, fmt, debounce } from '/dom.js';
import { api, ApiError } from '/api.js';
import { renderOverview } from '/views/overview.js';
import { renderDatabase } from '/views/database.js';
import { openClusterForm } from '/views/form.js';

const state = {
  clusters: [],
  version: '',
  route: { name: 'overview' },
  costs: {},
};

const els = {
  list: document.getElementById('cluster-list'),
  count: document.getElementById('nav-count'),
  view: document.getElementById('view'),
  costText: document.getElementById('cost-text'),
};

// ---------------------------------------------------------------- routing

function parseHash() {
  const hash = location.hash.replace(/^#/, '');
  if (!hash || hash === '/') return { name: 'overview' };
  const parts = hash.split('/').filter(Boolean);
  if (parts[0] === 'db' && parts[1]) {
    return { name: 'database', id: decodeURIComponent(parts[1]), tab: parts[2] || 'live' };
  }
  return { name: 'overview' };
}

export function navigate(path) {
  // Choosing a database closes the slide-over panel, so a tap does one thing
  // rather than leaving the panel covering the page it just opened.
  const sidebarEl = document.getElementById('sidebar');
  if (sidebarEl && sidebarEl.classList.contains('open')) closeSidebar();

  if (location.hash === '#' + path) {
    render();
    return;
  }
  location.hash = path;
}

window.addEventListener('hashchange', () => {
  state.route = parseHash();
  render();
});

// ---------------------------------------------------------------- data

export async function refreshClusters({ silent = false } = {}) {
  try {
    const data = await api.get('/api/clusters');
    state.clusters = data.clusters || [];
    state.version = data.version || '';
    renderSidebar();
    return state.clusters;
  } catch (err) {
    if (!silent) toast(err.message, 'error');
    return state.clusters;
  }
}

export function getClusters() { return state.clusters; }
export function getCluster(id) { return state.clusters.find((c) => c.id === id); }

// ---------------------------------------------------------------- sidebar

function renderSidebar() {
  const active = state.route.name === 'database' ? state.route.id : null;
  els.count.textContent = state.clusters.length || '';

  clear(els.list);
  for (const c of state.clusters) {
    const dot = h('span', {
      class: 'dot ' + dotClass(c),
      title: c.status ? c.status.summary : 'checking',
    });

    const nameRow = h('div', { class: 'cluster-top' }, dot, h('span', { class: 'cluster-name' }, text(c.name)));
    if (c.last_run && c.last_run.new_since_last > 0) {
      nameRow.append(h('span', { class: 'badge-new', title: 'new findings since the previous analysis' },
        text(`${c.last_run.new_since_last} new`)));
    }

    const item = h('li', {}, h('button', {
      class: 'cluster-item' + (c.id === active ? ' active' : ''),
      type: 'button',
      'aria-current': c.id === active ? 'page' : null,
      onclick: () => navigate(`/db/${encodeURIComponent(c.id)}`),
    }, nameRow, h('div', { class: 'cluster-sub' }, text(c.headline || ''))));

    els.list.append(item);
  }
}

function dotClass(c) {
  if (!c.status) return 'dot-pending';
  if (c.status.state === 'error') return 'dot-error';
  if (c.status.state === 'warning') return 'dot-warn';
  return 'dot-ok';
}

// ---------------------------------------------------------------- cost

// The footer shows what idxa itself is costing the databases it watches. A
// monitoring tool that cannot answer that question does not get installed
// twice.
async function refreshCost() {
  try {
    const costs = await api.get('/api/cost');
    state.costs = costs || {};
    const values = Object.values(state.costs);
    if (!values.length) {
      els.costText.textContent = 'read-only, never writes';
      return;
    }
    const worst = Math.max(...values.map((c) => c.duty_percent || 0));
    const total = values.reduce((sum, c) => sum + (c.total_ms || 0), 0);
    els.costText.textContent = `read-only · ${worst.toFixed(2)}% of server time · ${fmt.ms(total)} used`;
  } catch {
    /* the footer is decoration; failing to update it must not raise an error */
  }
}

// ---------------------------------------------------------------- render

let cleanup = null;

async function render() {
  if (cleanup) { cleanup(); cleanup = null; }
  renderSidebar();

  if (state.route.name === 'database') {
    const cluster = getCluster(state.route.id);
    if (!cluster) {
      // Configuration may not have loaded yet on a deep link.
      await refreshClusters({ silent: true });
      const retry = getCluster(state.route.id);
      if (!retry) {
        clear(els.view);
        els.view.append(notFound(state.route.id));
        return;
      }
    }
    cleanup = await renderDatabase(els.view, state.route.id, state.route.tab);
    renderSidebar();
    return;
  }

  cleanup = await renderOverview(els.view);
}

function notFound(id) {
  return h('div', { class: 'page' },
    h('div', { class: 'empty' },
      h('h1', {}, text('Not found')),
      h('p', {}, text(`No database named "${id}" is configured.`)),
      h('button', { class: 'btn btn-primary', onclick: () => navigate('/') }, text('Back to overview'))));
}

// ---------------------------------------------------------------- toasts

export function toast(message, kind = 'ok', ms = 5000) {
  const el = h('div', { class: `toast toast-${kind}`, role: 'status' }, text(message));
  document.getElementById('toasts').append(el);
  setTimeout(() => el.remove(), ms);
}

// ---------------------------------------------------------------- modal

const backdrop = document.getElementById('modal-backdrop');
const modalBody = document.getElementById('modal-body');
const modalTitle = document.getElementById('modal-title');
let lastFocused = null;

export function openModal(title, node) {
  lastFocused = document.activeElement;
  modalTitle.textContent = title;
  clear(modalBody);
  modalBody.append(node);
  backdrop.hidden = false;
  // Focus the first control so the form is usable from the keyboard at once.
  const first = modalBody.querySelector('input, select, textarea, button');
  if (first) first.focus();
}

export function closeModal() {
  backdrop.hidden = true;
  clear(modalBody);
  if (lastFocused && lastFocused.focus) lastFocused.focus();
}

document.getElementById('modal-close').addEventListener('click', closeModal);
backdrop.addEventListener('mousedown', (e) => { if (e.target === backdrop) closeModal(); });

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    if (!backdrop.hidden) { closeModal(); return; }
    if (sidebar.classList.contains('open')) { closeSidebar(); return; }
  }

  // Keyboard shortcuts, ignored while typing.
  const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement.tagName);
  if (typing || e.metaKey || e.ctrlKey || e.altKey) return;

  if (e.key === 'n' && backdrop.hidden) {
    e.preventDefault();
    openClusterForm();
  }
  if (e.key === 'g') {
    e.preventDefault();
    navigate('/');
  }
  // j and k move between databases, the way a developer expects.
  if (e.key === 'j' || e.key === 'k') {
    const ids = state.clusters.map((c) => c.id);
    if (!ids.length) return;
    const current = state.route.name === 'database' ? ids.indexOf(state.route.id) : -1;
    const next = e.key === 'j'
      ? Math.min(ids.length - 1, current + 1)
      : Math.max(0, current <= 0 ? 0 : current - 1);
    e.preventDefault();
    navigate(`/db/${encodeURIComponent(ids[next])}`);
  }
});

// ---------------------------------------------------------------- boot

// ---------------------------------------------------------------- narrow screens

// On a narrow screen the sidebar slides over the page. Without this it would
// be hidden with no way to reach another database.
const sidebar = document.getElementById('sidebar');
const scrim = document.getElementById('sidebar-scrim');
const menuButton = document.getElementById('menu-button');

export function closeSidebar() {
  sidebar.classList.remove('open');
  scrim.hidden = true;
  menuButton.setAttribute('aria-expanded', 'false');
}

function toggleSidebar() {
  const open = !sidebar.classList.contains('open');
  sidebar.classList.toggle('open', open);
  scrim.hidden = !open;
  menuButton.setAttribute('aria-expanded', String(open));
  if (open) {
    const first = els.list.querySelector('.cluster-item');
    if (first) first.focus();
  }
}

menuButton.addEventListener('click', toggleSidebar);
scrim.addEventListener('click', closeSidebar);

document.getElementById('add-database').addEventListener('click', () => openClusterForm());

// Paint the shell first, then load. The interface is usable before any
// database has answered.
state.route = parseHash();
render();
refreshClusters().then(render);

// Keep the sidebar and cost footer current without touching a database more
// often than the governor allows.
setInterval(() => refreshClusters({ silent: true }), 15000);
setInterval(refreshCost, 5000);
refreshCost();

export { ApiError, debounce };
