import { ApiFailure } from './client.ts';
import { matchesOpenApi } from './generated/contracts.ts';
import type { AccountErrorResponse, LiveTicket, Node, NodeModelPreparation, NodePage, RegistrationResult, SessionView } from './generated/contracts.ts';
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
  method?: 'GET' | 'POST' | 'PATCH'; body?: Record<string, unknown>; csrfToken?: string; signal?: AbortSignal; expectedStatus?: number | number[];
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
  if (options.expectedStatus !== undefined && !(Array.isArray(options.expectedStatus)
    ? options.expectedStatus.includes(response.status)
    : response.status === options.expectedStatus)) throw new ApiFailure('invalid_response', response.status);
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

export async function approveNodeEnrollment(userCode: string, csrfToken: string, signal?: AbortSignal): Promise<void> {
  const normalizedCode = userCode.trim().toUpperCase();
  if (!/^[A-Z2-7]{5}-[A-Z2-7]{5}-[A-Z2-7]{3}$/.test(normalizedCode)) {
    throw new ApiFailure('invalid_request');
  }
  if (!csrfToken) throw new ApiFailure('csrf_failed', 403);
  await accountRequest('/api/node/enrollments/approve', {
    method: 'POST', body: { user_code: normalizedCode }, csrfToken, signal, expectedStatus: 204
  });
}

function nodeIdPath(nodeID: string): string {
  if (!/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(nodeID)) {
    throw new ApiFailure('invalid_request');
  }
  return encodeURIComponent(nodeID);
}

export async function fetchNodes(limit = 50, cursor?: string, signal?: AbortSignal): Promise<NodePage> {
  if (!Number.isInteger(limit) || limit < 1 || limit > 100 || (cursor !== undefined && (!cursor || cursor.length > 512))) {
    throw new ApiFailure('invalid_request');
  }
  const query = new URLSearchParams({ limit: String(limit) });
  if (cursor) query.set('cursor', cursor);
  const value = await accountRequest(`/api/nodes?${query}`, { signal, expectedStatus: 200 });
  if (!matchesOpenApi(value, 'NodePage')) throw new ApiFailure('invalid_response');
  return value as NodePage;
}

export async function fetchNode(nodeID: string, signal?: AbortSignal): Promise<Node> {
  const value = await accountRequest(`/api/nodes/${nodeIdPath(nodeID)}`, { signal, expectedStatus: 200 });
  if (!matchesOpenApi(value, 'Node')) throw new ApiFailure('invalid_response');
  return value as Node;
}

export async function linkNode(userCode: string, name: string, csrfToken: string, signal?: AbortSignal): Promise<Node> {
  const normalizedCode = userCode.trim().toUpperCase();
  const normalizedName = name.trim();
  if (!/^[A-Z2-7]{5}-[A-Z2-7]{5}-[A-Z2-7]{3}$/.test(normalizedCode) || !normalizedName || normalizedName.length > 64) {
    throw new ApiFailure('invalid_request');
  }
  if (!csrfToken) throw new ApiFailure('csrf_failed', 403);
  const value = await accountRequest('/api/nodes/link', {
    method: 'POST', body: { user_code: normalizedCode, name: normalizedName }, csrfToken, signal, expectedStatus: 201
  });
  if (!matchesOpenApi(value, 'Node')) throw new ApiFailure('invalid_response');
  return value as Node;
}

export async function renameNode(nodeID: string, name: string, csrfToken: string, signal?: AbortSignal): Promise<Node> {
  const normalizedName = name.trim();
  if (!normalizedName || normalizedName.length > 64) throw new ApiFailure('invalid_request');
  if (!csrfToken) throw new ApiFailure('csrf_failed', 403);
  const value = await accountRequest(`/api/nodes/${nodeIdPath(nodeID)}`, {
    method: 'PATCH', body: { name: normalizedName }, csrfToken, signal, expectedStatus: 200
  });
  if (!matchesOpenApi(value, 'Node')) throw new ApiFailure('invalid_response');
  return value as Node;
}

export async function revokeNode(nodeID: string, csrfToken: string, signal?: AbortSignal): Promise<void> {
  if (!csrfToken) throw new ApiFailure('csrf_failed', 403);
  await accountRequest(`/api/nodes/${nodeIdPath(nodeID)}/revoke`, {
    method: 'POST', body: {}, csrfToken, signal, expectedStatus: 204
  });
}

export async function prepareNodeModel(nodeID: string, modelID: string, csrfToken: string,
  signal?: AbortSignal): Promise<NodeModelPreparation> {
  if (!/^[a-z0-9]+(?:-[a-z0-9]+)*$/.test(modelID) || modelID.length > 128) throw new ApiFailure('invalid_request');
  if (!csrfToken) throw new ApiFailure('csrf_failed', 403);
  const value = await accountRequest(`/api/nodes/${nodeIdPath(nodeID)}/models/${encodeURIComponent(modelID)}/prepare`, {
    method: 'POST', body: {}, csrfToken, signal, expectedStatus: [200, 202]
  });
  if (!matchesOpenApi(value, 'NodeModelPreparation')) throw new ApiFailure('invalid_response');
  return value as NodeModelPreparation;
}
