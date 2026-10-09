export interface Health { status: 'ok'; stage: string; mode: 'mock' }
export interface Model {
  id: string;
  context_tokens: number;
  max_output_tokens: number;
  availability: string;
}

export class ApiFailure extends Error {
  readonly code: string;
  readonly status: number;
  readonly requestId: string;
  constructor(code: string, status = 0, requestId = '') {
    super(code);
    this.code = code;
    this.status = status;
    this.requestId = requestId;
  }
}

function record(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

export function decodeHealth(value: unknown): Health {
  if (!record(value) || value.status !== 'ok' || value.mode !== 'mock' || typeof value.stage !== 'string') {
    throw new ApiFailure('invalid_response');
  }
  return { status: 'ok', mode: 'mock', stage: value.stage };
}

export function decodeModels(value: unknown): Model[] {
  if (!record(value) || value.object !== 'list' || !Array.isArray(value.data) || value.data.length > 100) {
    throw new ApiFailure('invalid_response');
  }
  const seen = new Set<string>();
  return value.data.map((item: unknown) => {
    if (!record(item) || typeof item.id !== 'string' || !item.id || item.id.length > 128 ||
      item.object !== 'model' || typeof item.context_tokens !== 'number' ||
      !Number.isSafeInteger(item.context_tokens) || item.context_tokens <= 0 ||
      typeof item.max_output_tokens !== 'number' || !Number.isSafeInteger(item.max_output_tokens) ||
      item.max_output_tokens <= 0 || typeof item.availability !== 'string' ||
      seen.has(item.id)) throw new ApiFailure('invalid_response');
    seen.add(item.id);
    return { id: item.id, context_tokens: item.context_tokens,
      max_output_tokens: item.max_output_tokens, availability: item.availability };
  });
}

export async function requestJSON(path: '/healthz' | '/v1/models', signal: AbortSignal, key = ''): Promise<unknown> {
  const response = await fetch(path, {
    cache: 'no-store', signal: AbortSignal.any([signal, AbortSignal.timeout(5000)]),
    headers: { Accept: 'application/json', ...(key ? { Authorization: `Bearer ${key}` } : {}) }
  });
  let value: unknown;
  try { value = await response.json(); } catch { throw new ApiFailure('invalid_response', response.status); }
  if (!response.ok) {
    const code = record(value) && record(value.error) && typeof value.error.code === 'string'
      ? value.error.code : 'unavailable';
    const requestId = record(value) && typeof value.request_id === 'string' && /^[a-f0-9-]{36}$/.test(value.request_id)
      ? value.request_id : '';
    throw new ApiFailure(code, response.status, requestId);
  }
  if (path === '/v1/models' && response.headers.get('X-Cordova-Mode') !== 'mock') {
    throw new ApiFailure('invalid_response', response.status);
  }
  return value;
}

export function errorMessage(error: unknown): string {
  if (error instanceof ApiFailure) {
    if (error.status === 401) return 'Шлюз не принял ключ. Проверь его и подключись снова.';
    if (error.status === 429) return 'Канал занят. Повтори запрос немного позже.';
    if (error.code === 'invalid_response') return 'Ответ шлюза не соответствует API этого пилота.';
  }
  return 'Не удалось связаться со шлюзом. Проверь, что сервер запущен и адрес настроен правильно.';
}
