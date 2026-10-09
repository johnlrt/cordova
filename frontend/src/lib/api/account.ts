import { ApiFailure } from './client.ts';
import { matchesOpenApi } from './generated/contracts.ts';
import type { AccountErrorResponse, LiveTicket, RegistrationResult, SessionView } from './generated/contracts.ts';
export type { Account, RegistrationResult, SessionView } from './generated/contracts.ts';
export function decodeSession(value: unknown): SessionView {
  if (!matchesOpenApi(value, 'SessionView')) throw new ApiFailure('invalid_response');
  return value as SessionView;
}

export function decodeRegistration(value: unknown): RegistrationResult {
  if (!matchesOpenApi(value, 'RegistrationResult')) throw new ApiFailure('invalid_response');
  return value as RegistrationResult;
}

function decodeError(response: Response, value: unknown): ApiFailure {
  if (!matchesOpenApi(value, 'AccountErrorResponse')) return new ApiFailure('invalid_response', response.status);
  const envelope = value as AccountErrorResponse;
  const error = envelope.error;
  const code = error.code;
  const requestId = envelope.request_id;
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
  if (!matchesOpenApi(value, 'LiveTicket')) throw new ApiFailure('invalid_response');
  const ticket = value as LiveTicket;
  if (Date.parse(ticket.expires_at) <= Date.now() || Date.parse(ticket.expires_at) > Date.now() + 35_000) throw new ApiFailure('invalid_response');
  return ticket.ticket;
}
