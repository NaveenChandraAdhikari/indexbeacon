// Small DOM helpers. Everything the interface builds goes through these, so
// that text always lands via textContent and never as HTML: values here come
// from a database, and a table named after a script tag must render as
// characters, not markup.

/** Create an element. Attributes starting with "on" become listeners. */
export function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs || {})) {
    if (value === null || value === undefined || value === false) continue;
    if (key.startsWith('on') && typeof value === 'function') {
      el.addEventListener(key.slice(2), value);
    } else if (key === 'class') {
      el.className = value;
    } else if (key === 'dataset') {
      Object.assign(el.dataset, value);
    } else {
      el.setAttribute(key, value === true ? '' : String(value));
    }
  }
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) continue;
    el.append(child);
  }
  return el;
}

/** A text node. Never interpolate values into markup. */
export function text(value) {
  return document.createTextNode(value === null || value === undefined ? '' : String(value));
}

export function clear(el) {
  while (el.firstChild) el.removeChild(el.firstChild);
}

/**
 * Append children, skipping the empty ones.
 *
 * Element.append() converts null to the string "null" and renders it, which
 * is how a conditional child such as `cond ? node : null` ends up printing
 * the word "null" on the page. Everything in the interface appends through
 * here or through h(), which filter the same way.
 */
export function add(el, ...children) {
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) continue;
    el.append(child);
  }
  return el;
}

/** Replace an element's children in one operation. */
export function fill(el, ...children) {
  clear(el);
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) continue;
    el.append(child);
  }
  return el;
}

/**
 * Set textContent only when it differs.
 *
 * This is what keeps the live view calm. Assigning the same string still
 * dirties the node, and a table of forty rows rewritten every few seconds
 * makes selection flicker and screen readers re-announce rows that did not
 * change.
 */
export function setText(el, value) {
  const next = value === null || value === undefined ? '' : String(value);
  if (el.textContent !== next) el.textContent = next;
}

export function setClass(el, cls) {
  if (el.className !== cls) el.className = cls;
}

export function debounce(fn, ms = 250) {
  let timer;
  return (...args) => {
    clearTimeout(timer);
    timer = setTimeout(() => fn(...args), ms);
  };
}

// -------------------------------------------------------------- formatting

const NBSP = ' ';

export const fmt = {
  /** Bytes in the binary units PostgreSQL itself reports. */
  bytes(n) {
    if (n === null || n === undefined) return '—';
    if (n < 1024) return `${n}${NBSP}B`;
    const units = ['KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
    let value = n / 1024;
    let i = 0;
    while (value >= 1024 && i < units.length - 1) { value /= 1024; i++; }
    return `${value.toFixed(value >= 100 ? 0 : 1)}${NBSP}${units[i]}`;
  },

  /** Counts, abbreviated: an exact figure implies a precision estimates lack. */
  count(n) {
    if (n === null || n === undefined) return '—';
    const abs = Math.abs(n);
    if (abs >= 1e12) return `${trim(n / 1e12)}T`;
    if (abs >= 1e9) return `${trim(n / 1e9)}G`;
    if (abs >= 1e6) return `${trim(n / 1e6)}M`;
    if (abs >= 1e3) return `${trim(n / 1e3)}k`;
    if (abs >= 10 || Number.isInteger(n)) return String(Math.round(n));
    return trim(n);
  },

  /** A per-second rate, with the unit attached. */
  rate(n) {
    if (!n) return `0${NBSP}/s`;
    return `${fmt.count(n)}${NBSP}/s`;
  },

  percent(fraction, digits = 1) {
    if (fraction === null || fraction === undefined) return '—';
    return `${(fraction * 100).toFixed(digits)}%`;
  },

  ms(value) {
    if (value === null || value === undefined) return '—';
    if (value < 1) return `${value.toFixed(2)}${NBSP}ms`;
    if (value < 1000) return `${Math.round(value)}${NBSP}ms`;
    return `${(value / 1000).toFixed(1)}${NBSP}s`;
  },

  /** "4 minutes ago", which is easier to judge than a timestamp. */
  ago(iso) {
    if (!iso) return 'never';
    const then = new Date(iso).getTime();
    if (Number.isNaN(then)) return 'never';
    const secs = Math.max(0, (Date.now() - then) / 1000);
    if (secs < 45) return 'just now';
    if (secs < 90) return 'a minute ago';
    if (secs < 3600) return `${Math.round(secs / 60)} minutes ago`;
    if (secs < 7200) return 'an hour ago';
    if (secs < 86400) return `${Math.round(secs / 3600)} hours ago`;
    if (secs < 172800) return 'yesterday';
    return `${Math.round(secs / 86400)} days ago`;
  },

  /** A future time in words, for the next scheduled run. */
  when(iso) {
    if (!iso) return '—';
    const at = new Date(iso);
    if (Number.isNaN(at.getTime())) return '—';
    const now = new Date();
    const time = at.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    const sameDay = at.toDateString() === now.toDateString();
    const tomorrow = new Date(now.getTime() + 86400000).toDateString() === at.toDateString();
    if (sameDay) return `today at ${time}`;
    if (tomorrow) return `tomorrow at ${time}`;
    return `${at.toLocaleDateString([], { weekday: 'long', month: 'short', day: 'numeric' })} at ${time}`;
  },

  duration(seconds) {
    if (!seconds && seconds !== 0) return '—';
    if (seconds < 60) return `${Math.round(seconds)} seconds`;
    if (seconds < 3600) return `${Math.round(seconds / 60)} minutes`;
    if (seconds < 172800) return `${Math.round(seconds / 3600)} hours`;
    return `${Math.round(seconds / 86400)} days`;
  },
};

function trim(value) {
  const s = value.toFixed(1);
  return s.endsWith('.0') ? s.slice(0, -2) : s;
}

// -------------------------------------------------------------- sparkline

/**
 * An inline sparkline. Drawn by hand rather than with a chart library,
 * because one line of SVG path data is cheaper than a dependency and this
 * renders identically in both themes.
 */
export function sparkline(values, { width = 84, height = 20 } = {}) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('class', 'spark');
  svg.setAttribute('width', width);
  svg.setAttribute('height', height);
  svg.setAttribute('viewBox', `0 0 ${width} ${height}`);
  svg.setAttribute('aria-hidden', 'true');

  const points = (values || []).filter((v) => Number.isFinite(v));
  if (points.length < 2) return svg;

  const max = Math.max(...points, 1);
  const step = width / (points.length - 1);
  const y = (v) => height - 1.5 - (v / max) * (height - 3);

  let line = '';
  points.forEach((v, i) => {
    line += `${i === 0 ? 'M' : 'L'}${(i * step).toFixed(1)},${y(v).toFixed(1)}`;
  });

  const area = document.createElementNS('http://www.w3.org/2000/svg', 'path');
  area.setAttribute('class', 'area');
  area.setAttribute('d', `${line}L${width},${height}L0,${height}Z`);

  const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
  path.setAttribute('d', line);

  svg.append(area, path);
  return svg;
}

/** Copy text, reporting whether it worked so the caller can confirm. */
export async function copy(value) {
  try {
    await navigator.clipboard.writeText(value);
    return true;
  } catch {
    // Clipboard access is refused outside a secure context, so fall back to
    // a selection the user can copy with the keyboard.
    const area = document.createElement('textarea');
    area.value = value;
    area.style.position = 'fixed';
    area.style.opacity = '0';
    document.body.append(area);
    area.select();
    let ok = false;
    try { ok = document.execCommand('copy'); } catch { ok = false; }
    area.remove();
    return ok;
  }
}
