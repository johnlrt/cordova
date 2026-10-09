import { ApiFailure } from '../api/client.ts';
import { matchesLiveServerFrame } from '../api/generated/contracts.ts';
import type { LiveEvent, LiveServerFrame } from '../api/generated/contracts.ts';
export type { LiveEvent, LiveTopic } from '../api/generated/contracts.ts';
export type LiveFrame = LiveServerFrame;

function assertNoDuplicateKeys(source: string) {
  let index = 0;
  const whitespace = () => { while (/\s/.test(source[index] ?? '')) index++; };
  const parseString = (): string => {
    const start = index++;
    while (index < source.length) {
      if (source[index] === '\\') { index += 2; continue; }
      if (source[index++] === '"') return JSON.parse(source.slice(start, index)) as string;
    }
    throw new Error('invalid_json');
  };
  const parseValue = (depth: number): void => {
    if (depth > 64) throw new Error('invalid_json');
    whitespace();
    if (source[index] === '"') { parseString(); return; }
    if (source[index] === '{') {
      index++;
      whitespace();
      const seen = new Set<string>();
      if (source[index] === '}') { index++; return; }
      while (index < source.length) {
        whitespace();
        if (source[index] !== '"') throw new Error('invalid_json');
        const key = parseString();
        if (seen.has(key)) throw new Error('duplicate_key');
        seen.add(key);
        whitespace();
        if (source[index++] !== ':') throw new Error('invalid_json');
        parseValue(depth + 1);
        whitespace();
        const separator = source[index++];
        if (separator === '}') return;
        if (separator !== ',') throw new Error('invalid_json');
      }
      throw new Error('invalid_json');
    }
    if (source[index] === '[') {
      index++;
      whitespace();
      if (source[index] === ']') { index++; return; }
      while (index < source.length) {
        parseValue(depth + 1);
        whitespace();
        const separator = source[index++];
        if (separator === ']') return;
        if (separator !== ',') throw new Error('invalid_json');
      }
      throw new Error('invalid_json');
    }
    const start = index;
    while (index < source.length && !/[\s,}\]]/.test(source[index])) index++;
    if (start === index) throw new Error('invalid_json');
  };
  parseValue(0);
  whitespace();
  if (index !== source.length) throw new Error('invalid_json');
}

export function decodeLiveFrame(raw: string): LiveFrame {
  if (new TextEncoder().encode(raw).byteLength > 1_048_576) throw new ApiFailure('invalid_response');
  let value: unknown;
  try { assertNoDuplicateKeys(raw); value = JSON.parse(raw); } catch { throw new ApiFailure('invalid_response'); }
  if (!matchesLiveServerFrame(value)) throw new ApiFailure('invalid_response');
  return value;
}

export function liveEvent(frame: LiveFrame): LiveEvent {
  if (frame.type !== 'live.event') throw new ApiFailure('invalid_response');
  return frame.payload as unknown as LiveEvent;
}

export function isFirstReadyFrame(frame: LiveFrame, authRequestId: string, accountId: string): boolean {
  return frame.type === 'live.ready' && frame.seq === 1 && frame.request_id === authRequestId &&
    frame.session_id === frame.payload.session_id && frame.payload.account_id === accountId;
}

export function isNextSequence(frame: LiveFrame, previous: number): boolean {
  return frame.seq === previous + 1;
}
