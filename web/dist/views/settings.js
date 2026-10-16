// Per-database settings: the schedule, the connection details, and removal.
//
// The schedule is offered as presets rather than a cron box, and the resolved
// next run is always shown in plain words. Not knowing what you configured is
// the single most common way a schedule goes wrong.

import { h, text, clear, fill, add, fmt } from '/dom.js';
import { api } from '/api.js';
import { toast, refreshClusters, navigate, getCluster } from '/app.js';
import { openClusterForm } from '/views/form.js';
import { removeAndGoHome } from '/views/remove.js';

const PRESETS = [
  { id: 'manual', label: 'Manual', schedule: { mode: 'manual' } },
  { id: '6h', label: 'Every 6 hours', schedule: { mode: 'interval', interval: '6h' } },
  { id: 'daily', label: 'Daily', schedule: { mode: 'cron', cron: '0 3 * * *' } },
  { id: 'weekly', label: 'Weekly', schedule: { mode: 'cron', cron: '0 3 * * 0' } },
  { id: 'custom', label: 'Custom', schedule: { mode: 'cron', cron: '0 3 * * *' } },
];

export async function renderSettings(id) {
  const wrap = h('div', { class: 'stack' });
  const cluster = getCluster(id);

  let current;
  try {
    current = await api.get(`/api/clusters/${encodeURIComponent(id)}/schedule`);
  } catch (err) {
    current = { mode: 'manual', describe: 'only when you ask', error: err.message };
  }

  wrap.append(scheduleCard(id, current));
  if (cluster) wrap.append(connectionCard(cluster));
  wrap.append(dangerCard(id, cluster));
  return wrap;
}

// ------------------------------------------------------------- schedule

function scheduleCard(id, current) {
  const card = h('div', { class: 'card' });
  const preview = h('div', { class: 'hint' });
  const customWrap = h('div', { hidden: true });

  const cronInput = h('input', {
    type: 'text',
    value: current.cron || '0 3 * * *',
    placeholder: '0 3 * * *',
    'aria-label': 'Cron expression',
  });

  const tzSelect = h('select', { 'aria-label': 'Timezone' });
  for (const zone of zones(current.timezone)) {
    tzSelect.append(h('option', { value: zone, selected: zone === (current.timezone || '') || null },
      text(zone || 'server local time')));
  }

  customWrap.append(
    h('div', { class: 'field' },
      h('label', {}, text('Cron expression')),
      cronInput,
      h('div', { class: 'hint' }, text('Five fields: minute, hour, day of month, month, day of week.'))),
    h('div', { class: 'field' },
      h('label', {}, text('Timezone')),
      tzSelect));

  let selected = presetFor(current);

  const segmented = h('div', { class: 'segmented', role: 'group', 'aria-label': 'How often to analyse' });
  for (const preset of PRESETS) {
    segmented.append(h('button', {
      type: 'button',
      'aria-pressed': String(preset.id === selected),
      onclick: () => {
        selected = preset.id;
        for (const btn of segmented.children) {
          btn.setAttribute('aria-pressed', String(btn.textContent === preset.label));
        }
        customWrap.hidden = preset.id !== 'custom';
        refreshPreview();
      },
    }, text(preset.label)));
  }
  customWrap.hidden = selected !== 'custom';

  function build() {
    const preset = PRESETS.find((p) => p.id === selected) || PRESETS[0];
    if (preset.id !== 'custom') return { ...preset.schedule };
    return { mode: 'cron', cron: cronInput.value.trim(), timezone: tzSelect.value };
  }

  // The preview is the whole point of the presets: it says, in words, what
  // was actually configured, recomputed as the user edits.
  const refreshPreview = debounceLocal(async () => {
    const schedule = build();
    if (schedule.mode === 'manual') {
      fill(preview, text('idxa will only analyse this database when you press Run.'));
      return;
    }
    try {
      const view = await previewSchedule(id, schedule);
      if (view.error) {
        fill(preview, h('span', { class: 'notice notice-error', style: 'display:block' }, text(view.error)));
        return;
      }
      const next = (view.next_runs || []).slice(0, 3).map(fmt.when);
      fill(preview, text(next.length
        ? `Next run: ${next[0]}${next.length > 1 ? `, then ${next.slice(1).join(', ')}` : ''}`
        : view.describe));
    } catch (err) {
      fill(preview, text(err.message));
    }
  }, 200);

  cronInput.addEventListener('input', refreshPreview);
  tzSelect.addEventListener('change', refreshPreview);

  const save = h('button', {
    class: 'btn btn-primary',
    onclick: async () => {
      save.disabled = true;
      try {
        const view = await api.put(`/api/clusters/${encodeURIComponent(id)}/schedule`, build());
        toast(view.mode === 'manual' ? 'Automatic analysis turned off' : 'Schedule saved', 'ok');
        await refreshClusters({ silent: true });
        refreshPreview();
      } catch (err) {
        toast(err.message, 'error');
      } finally {
        save.disabled = false;
      }
    },
  }, text('Save schedule'));

  add(card,
    h('h3', { style: 'margin-top:0' }, text('Automatic analysis')),
    h('p', { class: 'muted', style: 'margin-top:0' }, text(
      'Off by default. Recommendations get more reliable as statistics build up, '
      + 'so once a day at a quiet hour usually works well.')),
    segmented,
    customWrap,
    h('div', { style: 'margin-top:12px' }, preview),
    current.last_skip
      ? h('div', { class: 'notice notice-warn', style: 'margin-top:12px' },
        text(`Last due run was skipped: ${current.last_skip}`))
      : null,
    h('div', { style: 'margin-top:16px' }, save));

  refreshPreview();
  return card;
}

async function previewSchedule(id, schedule) {
  // The server computes firing times, including the timezone and the jitter,
  // so the preview cannot disagree with what will actually happen.
  try {
    return await api.put(`/api/clusters/${encodeURIComponent(id)}/schedule/preview`, schedule);
  } catch {
    // No preview endpoint: describe it locally instead of showing nothing.
    return { describe: describeLocally(schedule), next_runs: [] };
  }
}

function describeLocally(schedule) {
  if (schedule.mode === 'interval') return `every ${schedule.interval}`;
  if (schedule.mode === 'cron') return `on the schedule ${schedule.cron}`;
  return 'only when you ask';
}

function presetFor(current) {
  if (!current || current.mode === 'manual' || !current.mode) return 'manual';
  if (current.mode === 'interval') return current.interval === '6h' ? '6h' : 'custom';
  if (current.cron === '0 3 * * *') return 'daily';
  if (current.cron === '0 3 * * 0') return 'weekly';
  return 'custom';
}

function zones(currentZone) {
  const common = ['', 'UTC', 'Asia/Kolkata', 'Europe/London', 'Europe/Berlin',
    'America/New_York', 'America/Los_Angeles', 'Asia/Singapore', 'Australia/Sydney'];
  if (currentZone && !common.includes(currentZone)) common.splice(1, 0, currentZone);
  return common;
}

// ------------------------------------------------------------ connection

function connectionCard(cluster) {
  const rows = (cluster.nodes || []).map((n) => h('tr', {},
    h('td', { class: 'mono' }, text(n.label)),
    h('td', { class: 'mono faint' }, text(n.dsn)),
    h('td', {}, n.inline_password
      ? h('span', { class: 'pill pill-warn', title: 'the password is stored in the configuration file' },
        text('password on disk'))
      : h('span', { class: 'pill pill-ok' }, text('no stored password')))));

  return h('div', { class: 'card' },
    h('h3', { style: 'margin-top:0' }, text('Connections')),
    h('div', { class: 'table-wrap' },
      h('table', {},
        h('thead', {}, h('tr', {},
          h('th', {}, text('Server')),
          h('th', {}, text('Connection string')),
          h('th', {}, text('Password')))),
        h('tbody', {}, ...rows))),
    h('div', { class: 'hint', style: 'margin-top:8px' },
      text('Passwords are never shown. To keep one out of the file entirely, '
        + 'write ${PGPASSWORD} in the connection string and set that variable where idxa runs.')),
    h('div', { style: 'margin-top:12px' },
      h('button', { class: 'btn', onclick: () => openClusterForm(cluster) },
        text('Edit connections'))));
}

// ---------------------------------------------------------------- removal

function dangerCard(id, cluster) {
  const name = cluster ? cluster.name : id;
  return h('div', { class: 'card' },
    h('h3', { style: 'margin-top:0' }, text('Remove')),
    h('p', { class: 'muted' }, text(
      `Removing ${name} deletes it from idxa only. Your database, its data and `
      + 'its indexes are untouched: idxa has never written to it.')),
    removeAndGoHome(cluster || { id, name }, {
      class: 'btn btn-danger',
      label: 'Remove this database',
    }));
}

function debounceLocal(fn, ms) {
  let timer;
  return (...args) => {
    clearTimeout(timer);
    timer = setTimeout(() => fn(...args), ms);
  };
}
