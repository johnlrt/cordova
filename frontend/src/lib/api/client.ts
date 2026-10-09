export interface Account {
  id: string;
  username: string;
  created_at: string;
  totp_enabled: boolean;
  privacy_notice_version: string;
}

export interface SessionView {
  account: Account;
  csrf_token: string;
  expires_at: string;
  recovery_codes?: string[];
}

export interface UsageRow {
  request_id: string;
  api_key_id: string;
  model_id: string;
  status: string;
  input_tokens: number;
  output_tokens: number;
  charged_piconero: string;
  latency_ms: number;
  created_at: string;
  finished_at: string | null;
}

export interface LiveTicket {
  ticket: string;
  expires_at: string;
}

let csrfToken = '';

export class ApiFailure extends Error {
  readonly code: string;
  readonly status: number;
  readonly requestId: string;
  constructor(code: string, status: number, requestId = '') {
    super(code);
    this.name = 'ApiFailure';
    this.code = code;
    this.status = status;
    this.requestId = requestId;
  }
}

function record(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function decodeSession(value: unknown): SessionView {
  if (!record(value) || !record(value.account) || typeof value.account.id !== 'string' ||
    typeof value.account.username !== 'string' || typeof value.csrf_token !== 'string' ||
    typeof value.expires_at !== 'string') throw new ApiFailure('invalid_response', 502);
  csrfToken = value.csrf_token;
  return value as unknown as SessionView;
}

export function decodeUsage(value: unknown): UsageRow[] {
  if (!record(value) || !Array.isArray(value.data) || value.data.length > 100) {
    throw new ApiFailure('invalid_response', 502);
  }
  return value.data.map((row: unknown) => {
    if (!record(row) || typeof row.request_id !== 'string' ||
      !/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(row.request_id) ||
      typeof row.api_key_id !== 'string' ||
      !/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(row.api_key_id) ||
      row.model_id !== 'cordova-chat-small-v1' || typeof row.status !== 'string' ||
      !Number.isSafeInteger(row.input_tokens) || (row.input_tokens as number) < 0 ||
      !Number.isSafeInteger(row.output_tokens) || (row.output_tokens as number) < 0 ||
      typeof row.charged_piconero !== 'string' || !/^(0|[1-8][0-9]{0,18}|9(?:0{0,18}))$/.test(row.charged_piconero) ||
      !Number.isSafeInteger(row.latency_ms) || (row.latency_ms as number) < 0 ||
      typeof row.created_at !== 'string' || !(row.finished_at === null || typeof row.finished_at === 'string')) {
      throw new ApiFailure('invalid_response', 502);
    }
    return row as unknown as UsageRow;
  });
}

export function safeReturnPath(value: string | null): string {
  if (!value || !value.startsWith('/') || value.startsWith('//') || value.includes('\\') ||
    value.includes('\r') || value.includes('\n')) return '/dashboard';
  try {
    const origin = typeof window === 'undefined' ? 'https://cordova.invalid' : window.location.origin;
    const parsed = new URL(value, origin);
    if (parsed.origin !== origin) return '/dashboard';
    return `${parsed.pathname}${parsed.search}${parsed.hash}`;
  } catch { return '/dashboard'; }
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  headers.set('Accept', 'application/json');
  if (init.body) headers.set('Content-Type', 'application/json');
  if (['POST', 'PUT', 'PATCH', 'DELETE'].includes((init.method || 'GET').toUpperCase()) && csrfToken) {
    headers.set('X-CSRF-Token', csrfToken);
  }
  let response: Response;
  try {
    response = await fetch(path, {
      ...init, headers, credentials: 'same-origin', cache: 'no-store',
      signal: AbortSignal.any([init.signal || new AbortController().signal, AbortSignal.timeout(10000)])
    });
  } catch { throw new ApiFailure('unavailable', 0); }
  if (response.status === 204) return undefined as T;
  let body: unknown;
  try { body = await response.json(); } catch { throw new ApiFailure('invalid_response', response.status); }
  if (!response.ok) {
    const code = record(body) && record(body.error) && typeof body.error.code === 'string' ? body.error.code : 'unavailable';
    const requestId = record(body) && typeof body.request_id === 'string' ? body.request_id : '';
    if (response.status === 401) csrfToken = '';
    throw new ApiFailure(code, response.status, requestId);
  }
  return body as T;
}

export const api = {
  register(input: { username: string; password: string; privacy_notice_version: string }) {
    return request<SessionView>('/api/auth/register', { method: 'POST', body: JSON.stringify(input) }).then(decodeSession);
  },
  login(input: { username: string; password: string }) {
    return request<SessionView>('/api/auth/login', { method: 'POST', body: JSON.stringify(input) }).then(decodeSession);
  },
  recover(input: { username: string; recovery_code: string; new_password: string }) {
    return request<SessionView>('/api/auth/recover', { method: 'POST', body: JSON.stringify(input) }).then(decodeSession);
  },
  session() { return request<SessionView>('/api/session').then(decodeSession); },
  logout() { return request<void>('/api/auth/logout', { method: 'POST', body: '{}' }).finally(() => { csrfToken = ''; }); },
  usage() { return request<unknown>('/api/usage').then(decodeUsage); },
  liveTicket() { return request<LiveTicket>('/api/live-ticket', { method: 'POST', body: '{}' }); },
  liveURL() {
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    return `${protocol}//${window.location.host}/api/live`;
  },
  createKey(input: { name: string; permissions: string[]; model_ids: string[]; daily_spend_cap_piconero: string }) {
    return request<unknown>('/api/keys', { method: 'POST', body: JSON.stringify(input) });
  }
};
