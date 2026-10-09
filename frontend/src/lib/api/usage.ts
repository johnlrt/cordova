import { ApiFailure } from './client.ts';
import { getAccountJSON } from './account.ts';
import { matchesOpenApi } from './generated/contracts.ts';
import type { UsagePage } from './generated/contracts.ts';
export type { UsagePage, UsageRow } from './generated/contracts.ts';

export function decodeUsagePage(value: unknown): UsagePage {
  if (!matchesOpenApi(value, 'UsagePage')) throw new ApiFailure('invalid_response');
  const page = value as UsagePage;
  const ids = new Set<string>();
  for (const row of page.data) {
    if (ids.has(row.request_id)) throw new ApiFailure('invalid_response');
    ids.add(row.request_id);
  }
  return page;
}

export async function fetchRecentUsage(signal: AbortSignal): Promise<UsagePage> {
  return decodeUsagePage(await getAccountJSON('/api/usage?limit=5', signal));
}
