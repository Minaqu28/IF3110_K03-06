export class ApiError extends Error {
  constructor(status, code, message) { super(message); this.status = status; this.code = code; }
}

let csrfToken = '';
export function setCSRF(token) { csrfToken = token || ''; }

export async function api(path, { method = 'GET', body, signal, csrf } = {}) {
  const headers = { Accept: 'application/json' };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (method !== 'GET' && method !== 'HEAD') headers['X-CSRF-Token'] = csrf || csrfToken;
  let response;
  try {
    response = await fetch(`/api/v1${path}`, {
      method, headers, credentials: 'same-origin',
      body: body === undefined ? undefined : JSON.stringify(body), signal,
    });
  } catch (error) {
    if (error.name === 'AbortError') throw error;
    throw new ApiError(0, 'NETWORK_ERROR', 'Koneksi gagal. Periksa jaringan dan coba lagi.');
  }
  if (response.status === 204) return null;
  const payload = await response.json().catch(() => null);
  if (!response.ok) throw new ApiError(response.status, payload?.error?.code || 'HTTP_ERROR', payload?.error?.message || 'Permintaan gagal.');
  return payload?.data;
}
