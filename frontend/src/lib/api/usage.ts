import { ApiFailure } from './client.ts';
import { getAccountJSON } from './account.ts';

export interface UsageRow {
  request_id: string;
  model_id: string;
  status: 'accepted' | 'running' | 'settled' | 'refunded' | 'cancelled' | 'recovering';
  input_tokens: number;
  output_tokens: number;
  charged_piconero: string;
  latency_ms: number | null;
  created_at: string;
  finished_at: string | null;
}

export interface UsagePage { data: UsageRow[]; next_cursor: string | null }

function record(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
function exact(value: Record<string, unknown>, keys: string[]) {
  return Object.keys(value).length === keys.length && keys.every((key) => Object.hasOwn(value, key));
}
function uuid(value: unknown): value is string {
  return typeof value === 'string' && /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(value);
}
function timestamp(value: unknown): value is string {
  return typeof value === 'string' && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(value) && Number.isFinite(Date.parse(value));
}
function count(value: unknown): value is number { return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0; }

export function decodeUsagePage(value: unknown): UsagePage {
  if (!record(value) || !exact(value, ['data', 'next_cursor']) || !Array.isArray(value.data) || value.data.length > 100 ||
      !(value.next_cursor === null || (typeof value.next_cursor === 'string' && value.next_cursor.length <= 2048))) {
    throw new ApiFailure('invalid_response');
  }
  const ids = new Set<string>();
  const data = value.data.map((item: unknown): UsageRow => {
    const keys = ['request_id', 'model_id', 'status', 'input_tokens', 'output_tokens', 'charged_piconero', 'latency_ms', 'created_at', 'finished_at'];
    if (!record(item) || !exact(item, keys) || !uuid(item.request_id) || typeof item.model_id !== 'string' ||
        !/^[a-z0-9][a-z0-9._-]{0,127}$/.test(item.model_id) ||
        !['accepted', 'running', 'settled', 'refunded', 'cancelled', 'recovering'].includes(String(item.status)) ||
        !count(item.input_tokens) || !count(item.output_tokens) ||
        typeof item.charged_piconero !== 'string' || !/^(0|[1-9][0-9]*)$/.test(item.charged_piconero) ||
        BigInt(item.charged_piconero) > 9_000_000_000_000_000_000n ||
        !(item.latency_ms === null || (typeof item.latency_ms === 'number' && Number.isSafeInteger(item.latency_ms) && item.latency_ms >= 0)) ||
        !timestamp(item.created_at) || !(item.finished_at === null || timestamp(item.finished_at)) || ids.has(item.request_id)) {
      throw new ApiFailure('invalid_response');
    }
    ids.add(item.request_id);
    return item as unknown as UsageRow;
  });
  return { data, next_cursor: value.next_cursor };
}

export async function fetchRecentUsage(signal: AbortSignal): Promise<UsagePage> {
  return decodeUsagePage(await getAccountJSON('/api/usage?limit=5', signal));
}
