// The add and edit form.
//
// Adding a database has to be one paste and one click. The Test button gives
// a verdict before anything is written to disk, and reports which extensions
// are present, because a server without pg_stat_statements produces a much
// thinner result and the user should learn that here rather than after their
// first empty analysis.

import { h, text, clear, fill } from '/dom.js';
import { api } from '/api.js';
import { openModal, closeModal, toast, refreshClusters, navigate } from '/app.js';

const PLACEHOLDER = 'postgres://user:password@host:5432/database';

export function openClusterForm(existing = null) {
  const editing = Boolean(existing);
  const form = buildForm(existing);
  openModal(editing ? `Edit ${existing.name}` : 'Add a database', form);
}

function buildForm(existing) {
  const nameInput = h('input', {
    type: 'text',
    id: 'cluster-name',
    placeholder: 'Production',
    value: existing ? existing.name : '',
    autocomplete: 'off',
  });

  const dsnList = h('div', {});
  const startingDSNs = existing && existing.nodes.length
    ? existing.nodes.map((n) => n.dsn)
    : [''];

  // A redacted connection string cannot be saved back, so editing starts the
  // field empty rather than writing "****" into the password.
  const redacted = startingDSNs.some((d) => d.includes('****'));

  function addRow(value = '', focus = false) {
    const input = h('input', {
      type: 'text',
      placeholder: PLACEHOLDER,
      value: value.includes('****') ? '' : value,
      spellcheck: 'false',
      autocapitalize: 'off',
      autocomplete: 'off',
      'aria-label': 'Connection string',
    });
    const row = h('div', { class: 'dsn-row' },
      input,
      h('button', {
        class: 'btn btn-sm',
        type: 'button',
        title: 'Remove this connection string',
        'aria-label': 'Remove connection string',
        onclick: () => {
          row.remove();
          if (!dsnList.children.length) addRow('', true);
        },
      }, text('−')));
    dsnList.append(row);
    if (focus) input.focus();
    return input;
  }

  for (const dsn of startingDSNs) addRow(dsn);

  const result = h('div', {});

  const testButton = h('button', {
    class: 'btn',
    type: 'button',
    onclick: async () => {
      const dsns = collect(dsnList);
      if (!dsns.length) {
        fill(result, notice('warn', 'Enter a connection string first.'));
        return;
      }
      testButton.disabled = true;
      testButton.textContent = 'Testing…';
      try {
        const status = await api.post('/api/clusters/test', {
          name: nameInput.value || 'test',
          nodes: dsns.map((dsn) => ({ dsn })),
        });
        fill(result, renderStatus(status));
      } catch (err) {
        fill(result, notice('error', err.message));
      } finally {
        testButton.disabled = false;
        testButton.textContent = 'Test connection';
      }
    },
  }, text('Test connection'));

  const saveButton = h('button', { class: 'btn btn-primary', type: 'submit' },
    text(existing ? 'Save changes' : 'Add database'));

  const form = h('form', {
    onsubmit: async (e) => {
      e.preventDefault();
      const dsns = collect(dsnList);
      if (!nameInput.value.trim()) {
        fill(result, notice('warn', 'Give this database a name.'));
        nameInput.focus();
        return;
      }
      if (!dsns.length) {
        fill(result, notice('warn', 'Enter at least one connection string.'));
        return;
      }

      saveButton.disabled = true;
      saveButton.textContent = 'Saving…';
      const payload = {
        name: nameInput.value.trim(),
        description: '',
        nodes: dsns.map((dsn) => ({ name: '', dsn })),
        // Carry the existing schedule through, or editing a connection
        // string would quietly switch automatic analysis back off.
        schedule: existing ? scheduleOf(existing) : { mode: 'manual' },
      };

      try {
        const saved = existing
          ? await api.put(`/api/clusters/${encodeURIComponent(existing.id)}`, payload)
          : await api.post('/api/clusters', payload);
        closeModal();
        await refreshClusters();
        toast(existing ? 'Saved' : `Added ${saved.name}`, 'ok');
        navigate(`/db/${encodeURIComponent(saved.id)}`);
      } catch (err) {
        fill(result, notice('error', err.message));
      } finally {
        saveButton.disabled = false;
        saveButton.textContent = existing ? 'Save changes' : 'Add database';
      }
    },
  },
    h('div', { class: 'field' },
      h('label', { for: 'cluster-name' }, text('Name')),
      nameInput,
      h('div', { class: 'hint' }, text('Whatever you call this database in conversation.'))),

    h('div', { class: 'field' },
      h('label', {}, text('Connection strings')),
      dsnList,
      h('button', {
        class: 'btn btn-sm',
        type: 'button',
        onclick: () => addRow('', true),
      }, text('Add a replica')),
      h('div', { class: 'hint' }, text(
        'Add the primary and, if you have them, its read replicas. '
        + 'Statistics are counted separately on every instance, so an index used only '
         + 'on one replica looks unused if idxa can only see the primary.')),
      h('div', { class: 'hint' }, text(
        'To keep the password out of the configuration file, write ${PGPASSWORD} '
         + 'and set that variable in the environment where idxa runs.')),
      redacted ? notice('info', 'Passwords are hidden, so re-enter the connection string to change it.') : null),

    result,

    h('div', { class: 'modal-actions' },
      testButton,
      h('button', { class: 'btn', type: 'button', onclick: closeModal }, text('Cancel')),
      saveButton));

  return form;
}

// scheduleOf extracts the saved schedule from a rendered view, dropping the
// fields the server computes for display.
function scheduleOf(cluster) {
  const s = cluster.schedule || {};
  const out = { mode: s.mode || 'manual' };
  if (s.interval) out.interval = s.interval;
  if (s.cron) out.cron = s.cron;
  if (s.timezone) out.timezone = s.timezone;
  return out;
}

function collect(list) {
  return Array.from(list.querySelectorAll('input'))
    .map((i) => i.value.trim())
    .filter(Boolean);
}

function notice(kind, message) {
  return h('div', { class: `notice notice-${kind}` }, text(message));
}

// Renders the verdict from a connection test: what answered, what version,
// and which extensions are available, with the exact command for anything
// missing.
function renderStatus(status) {
  const wrap = h('div', {});

  for (const node of status.nodes || []) {
    if (!node.reachable) {
      wrap.append(notice('error', `${node.dsn}: ${node.error}`));
      continue;
    }
    const bits = [
      node.role === 'primary' ? 'primary' : node.role === 'replica' ? 'replica' : 'connected',
      shortVersion(node.version),
    ];
    wrap.append(h('div', { class: 'notice notice-ok' },
      h('div', {}, text(`${node.dsn} — ${bits.join(', ')}`)),
      h('div', { class: 'row wrap', style: 'margin-top:6px;gap:6px' },
        pill(node.pg_stat_statements && node.pg_stat_statements_preloaded, 'pg_stat_statements'),
        pill(node.hypopg, 'hypopg'))));
  }

  for (const advice of status.advice || []) {
    wrap.append(notice('warn', advice));
  }
  if (!(status.advice || []).length && (status.nodes || []).every((n) => n.reachable)) {
     wrap.append(notice('ok', 'Everything idxa needs is available.'));
  }
  return wrap;
}

function pill(present, label) {
  return h('span', { class: 'pill ' + (present ? 'pill-ok' : 'pill-warn') },
    text(present ? label : `${label} missing`));
}

function shortVersion(version) {
  if (!version) return 'PostgreSQL';
  const match = /PostgreSQL (\d+(\.\d+)?)/.exec(version);
  return match ? `PostgreSQL ${match[1]}` : 'PostgreSQL';
}
