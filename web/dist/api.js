// API client.
//
// Every response the interface shows a person passes through here, including
// errors, so a failure arrives as a sentence rather than a status code.

export class ApiError extends Error {
  constructor(message, status) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

// The access token, when the server was started on a network address. It is
// carried in the query string so a copied link keeps working.
const token = new URLSearchParams(location.search).get('token');

function withToken(path) {
  if (!token) return path;
  return path + (path.includes('?') ? '&' : '?') + 'token=' + encodeURIComponent(token);
}

async function request(method, path, body) {
  let response;
  try {
    response = await fetch(withToken(path), {
      method,
      headers: body ? { 'Content-Type': 'application/json' } : undefined,
      body: body ? JSON.stringify(body) : undefined,
    });
  } catch {
    // A failed fetch means the process is gone, which is worth saying
    // plainly rather than showing "NetworkError".
    throw new ApiError('idxa is not responding. Is the command still running?', 0);
  }

  if (response.status === 204) return null;

  const isJSON = (response.headers.get('Content-Type') || '').includes('json');
  const payload = isJSON ? await response.json().catch(() => null) : await response.text();

  if (!response.ok) {
    const message = payload && payload.error
      ? payload.error
      : typeof payload === 'string' && payload.trim()
        ? payload.trim()
        : `request failed (${response.status})`;
    throw new ApiError(message, response.status);
  }
  return payload;
}

export const api = {
  get: (path) => request('GET', path),
  post: (path, body) => request('POST', path, body),
  put: (path, body) => request('PUT', path, body),
  delete: (path) => request('DELETE', path),

  /** The URL for a download, with the token attached when one is needed. */
  url: (path) => withToken(path),

  /**
   * Subscribe to a server-sent event stream.
   *
   * The returned function closes the connection. Callers must call it when
   * the view goes away: an open stream keeps the server collecting, and
   * collection means queries against somebody's database.
   */
  stream(path, { onEvent, onError } = {}) {
    let source;
    try {
      source = new EventSource(withToken(path));
    } catch {
      if (onError) onError(new ApiError('this browser cannot open a live connection', 0));
      return () => {};
    }

    const handler = (event) => {
      if (!onEvent) return;
      try {
        onEvent(event.type, JSON.parse(event.data));
      } catch {
        /* a malformed frame is not worth interrupting the stream for */
      }
    };

    source.addEventListener('snapshot', handler);
    source.addEventListener('run', handler);
    source.onerror = () => {
      // EventSource reconnects on its own; only report a stream that closed
      // for good, so a brief blip does not raise an alarm.
      if (source.readyState === EventSource.CLOSED && onError) {
        onError(new ApiError('the live connection closed', 0));
      }
    };

    return () => {
      source.removeEventListener('snapshot', handler);
      source.removeEventListener('run', handler);
      source.onerror = null;
      source.close();
    };
  },
};
