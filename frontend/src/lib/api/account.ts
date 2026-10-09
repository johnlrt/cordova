import { ApiFailure } from './client.ts';

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
}

export interface RegistrationResult extends SessionView {
  recovery_codes: string[];
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
function hasExactKeys(value: Record<string, unknown>, keys: string[]) {
  return Object.keys(value).length === keys.length && keys.every((key) => Object.hasOwn(value, key));
}

function isUuid(value: unknown): value is string {
  return typeof value === 'string' && /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(value);
}

function isTimestamp(value: unknown): value is string {
  return typeof value === 'string' && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(value) && Number.isFinite(Date.parse(value));
}

export function decodeSession(value: unknown): SessionView {
  if (!isRecord(value) || !hasExactKeys(value, ['account', 'csrf_token', 'expires_at']) || !isRecord(value.account) ||
      !hasExactKeys(value.account, ['id', 'username', 'created_at', 'totp_enabled', 'privacy_notice_version'])) throw new ApiFailure('invalid_response');
  const account = value.account;
  if (!isUuid(account.id) || typeof account.username !== 'string' || account.username.length < 3 || account.username.length > 64 ||
      !isTimestamp(account.created_at) || typeof account.totp_enabled !== 'boolean' ||
      typeof account.privacy_notice_version !== 'string' || !account.privacy_notice_version ||
      typeof value.csrf_token !== 'string' || value.csrf_token.length < 16 || value.csrf_token.length > 512 ||
      !isTimestamp(value.expires_at)) throw new ApiFailure('invalid_response');
  return {
    account: {
      id: account.id, username: account.username, created_at: account.created_at,
      totp_enabled: account.totp_enabled, privacy_notice_version: account.privacy_notice_version
    },
    csrf_token: value.csrf_token,
    expires_at: value.expires_at
  };
}

export function decodeRegistration(value: unknown): RegistrationResult {
  if (!isRecord(value) || !hasExactKeys(value, ['account', 'csrf_token', 'expires_at', 'recovery_codes']) ||
      !Array.isArray(value.recovery_codes) || value.recovery_codes.length < 1 || value.recovery_codes.length > 32 ||
      !value.recovery_codes.every((code) => typeof code === 'string' && code.length >= 8 && code.length <= 128)) {
    throw new ApiFailure('invalid_response');
  }
  const codes = value.recovery_codes as string[];
  if (new Set(codes).size !== codes.length) throw new ApiFailure('invalid_response');
  const session = decodeSession({ account: value.account, csrf_token: value.csrf_token, expires_at: value.expires_at });
  return { ...session, recovery_codes: [...codes] };
}

function decodeError(response: Response, value: unknown): ApiFailure {
  const envelope = isRecord(value) && isRecord(value.error) ? value.error : null;
  const validTypes = ['invalid_request_error', 'authentication_error', 'permission_error', 'rate_limit_error', 'server_error'];
  if (!isRecord(value) || Object.keys(value).length !== 2 || !Object.hasOwn(value, 'error') || !Object.hasOwn(value, 'request_id') ||
      !isUuid(value.request_id) ||
      !envelope || Object.keys(envelope).length !== 4 || !['message', 'type', 'param', 'code'].every((key) => Object.hasOwn(envelope, key)) ||
      typeof envelope.message !== 'string' || envelope.message.length > 512 || !validTypes.includes(String(envelope.type)) ||
      !(envelope.param === null || (typeof envelope.param === 'string' && envelope.param.length <= 128)) ||
      typeof envelope.code !== 'string' || !/^[a-z_]{1,64}$/.test(envelope.code)) {
    return new ApiFailure('invalid_response', response.status);
  }
  const code = envelope.code;
  const requestId = value.request_id;
  const retrySeconds = Number(response.headers.get('Retry-After'));
  return new ApiFailure(code, response.status, requestId,
    Number.isInteger(retrySeconds) && retrySeconds >= 0 && retrySeconds <= 3600 ? retrySeconds : null);
}

async function accountRequest(path: string, options: {
  method?: 'GET' | 'POST'; body?: Record<string, unknown>; csrfToken?: string; signal?: AbortSignal; expectedStatus?: number;
} = {}): Promise<unknown> {
  if (!path.startsWith('/api/') || path.startsWith('//') || path.includes('\\')) throw new ApiFailure('invalid_request');
  let url: URL;
  try { url = new URL(path, 'https://cordova.invalid'); } catch { throw new ApiFailure('invalid_request'); }
  if (url.origin !== 'https://cordova.invalid' || !url.pathname.startsWith('/api/') || url.hash) throw new ApiFailure('invalid_request');
  const requestPath = `${url.pathname}${url.search}`;
  const method = options.method ?? 'GET';
  if (method === 'POST' && !options.csrfToken && path === '/api/auth/logout') throw new ApiFailure('csrf_failed', 403);
  const headers: Record<string, string> = { Accept: 'application/json' };
  if (options.body) headers['Content-Type'] = 'application/json';
  if (options.csrfToken) headers['X-CSRF-Token'] = options.csrfToken;
  let response: Response;
  try {
    response = await fetch(requestPath, {
      method, credentials: 'same-origin', cache: 'no-store',
      headers, ...(options.body ? { body: JSON.stringify(options.body) } : {}),
      signal: options.signal ? AbortSignal.any([options.signal, AbortSignal.timeout(8000)]) : AbortSignal.timeout(8000)
    });
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') throw error;
    throw new ApiFailure('unavailable');
  }
  if (response.status === 204) {
    if (!response.ok || (options.expectedStatus !== undefined && options.expectedStatus !== 204)) throw new ApiFailure('invalid_response', response.status);
    return undefined;
  }
  let value: unknown;
  try { value = await response.json(); } catch { throw new ApiFailure('invalid_response', response.status); }
  if (!response.ok) throw decodeError(response, value);
  if (options.expectedStatus !== undefined && response.status !== options.expectedStatus) throw new ApiFailure('invalid_response', response.status);
  return value;
}

export async function getAccountJSON(path: string, signal?: AbortSignal): Promise<unknown> {
  return accountRequest(path, { signal });
}

export async function loadSession(signal?: AbortSignal): Promise<SessionView> {
  return decodeSession(await accountRequest('/api/session', { signal, expectedStatus: 200 }));
}

export async function registerAccount(username: string, password: string, privacyNoticeVersion: string, signal?: AbortSignal): Promise<RegistrationResult> {
  return decodeRegistration(await accountRequest('/api/auth/register', {
    method: 'POST', body: { username, password, privacy_notice_version: privacyNoticeVersion }, signal, expectedStatus: 201
  }));
}

export async function loginAccount(username: string, password: string, totpCode?: string, signal?: AbortSignal): Promise<SessionView> {
  return decodeSession(await accountRequest('/api/auth/login', {
    method: 'POST', body: { username, password, ...(totpCode ? { totp_code: totpCode } : {}) }, signal, expectedStatus: 200
  }));
}

export async function recoverAccount(username: string, recoveryCode: string, newPassword: string, signal?: AbortSignal): Promise<SessionView> {
  return decodeSession(await accountRequest('/api/auth/recover', {
    method: 'POST', body: { username, recovery_code: recoveryCode, new_password: newPassword }, signal, expectedStatus: 200
  }));
}

export async function logoutAccount(csrfToken: string, signal?: AbortSignal): Promise<void> {
  await accountRequest('/api/auth/logout', { method: 'POST', body: {}, csrfToken, signal, expectedStatus: 204 });
}

export async function fetchLiveTicket(csrfToken: string, signal?: AbortSignal): Promise<string> {
  const value = await accountRequest('/api/live-ticket', { method: 'POST', body: {}, csrfToken, signal, expectedStatus: 200 });
  if (!isRecord(value) || !hasExactKeys(value, ['ticket', 'expires_at']) || typeof value.ticket !== 'string' || value.ticket.length < 32 || value.ticket.length > 512 ||
      !isTimestamp(value.expires_at) || Date.parse(value.expires_at) <= Date.now() ||
      Date.parse(value.expires_at) > Date.now() + 35_000) throw new ApiFailure('invalid_response');
  return value.ticket;
}
