import { ApiFailure } from '../api/client.ts';

export type LiveTopic = 'nodes' | 'usage' | 'billing';
export interface LiveEvent { event_id: string; topic: LiveTopic; resource_id: string; revision: number; action: 'invalidate' }
export interface LiveFrame {
  v: 1; type: 'live.ready' | 'live.event' | 'live.resync' | 'live.ping' | 'live.pong' | 'error';
  message_id: string; request_id: string | null; session_id: string | null; seq: number;
  sent_at: string; payload: Record<string, unknown>; error: null | { code: string; message: string; retryable: boolean };
}

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

function validatePayload(type: LiveFrame['type'], payload: Record<string, unknown>, error: LiveFrame['error']) {
  if (type === 'live.ready') {
    if (!exact(payload, ['session_id', 'account_id', 'heartbeat_interval_ms']) || !uuid(payload.session_id) ||
        !uuid(payload.account_id) || payload.heartbeat_interval_ms !== 15000 || error !== null) throw new ApiFailure('invalid_response');
  } else if (type === 'live.event') {
    if (!exact(payload, ['event_id', 'topic', 'resource_id', 'revision', 'action']) ||
        typeof payload.event_id !== 'string' || payload.event_id.length < 1 || payload.event_id.length > 128 ||
        !['nodes', 'usage', 'billing'].includes(String(payload.topic)) || !uuid(payload.resource_id) ||
        typeof payload.revision !== 'number' || !Number.isSafeInteger(payload.revision) || payload.revision < 1 ||
        payload.action !== 'invalidate' || error !== null) throw new ApiFailure('invalid_response');
  } else if (type === 'live.resync') {
    if (!exact(payload, ['reason']) || !['cursor_expired', 'gap', 'slow_consumer'].includes(String(payload.reason)) || error !== null) {
      throw new ApiFailure('invalid_response');
    }
  } else if (type === 'live.ping' || type === 'live.pong') {
    if (!exact(payload, ['time']) || !timestamp(payload.time) || error !== null) throw new ApiFailure('invalid_response');
  } else if (type === 'error') {
    const keys = ['operation_type', 'attempt_id', 'lease_id', 'command_id'];
    const optionalId = (value: unknown) => value === null || uuid(value);
    if (!exact(payload, keys) || typeof payload.operation_type !== 'string' || payload.operation_type.length > 64 ||
        !optionalId(payload.attempt_id) || !optionalId(payload.lease_id) || !optionalId(payload.command_id) ||
        !error || typeof error.code !== 'string' || !/^[a-z_]{1,64}$/.test(error.code) ||
        typeof error.message !== 'string' || error.message.length > 256 || typeof error.retryable !== 'boolean') {
      throw new ApiFailure('invalid_response');
    }
  }
}

export function decodeLiveFrame(raw: string): LiveFrame {
  if (new TextEncoder().encode(raw).byteLength > 1_048_576) throw new ApiFailure('invalid_response');
  let value: unknown;
  try { assertNoDuplicateKeys(raw); value = JSON.parse(raw); } catch { throw new ApiFailure('invalid_response'); }
  const keys = ['v', 'type', 'message_id', 'request_id', 'session_id', 'seq', 'sent_at', 'payload', 'error'];
  if (!record(value) || !exact(value, keys) || value.v !== 1 ||
      !['live.ready', 'live.event', 'live.resync', 'live.ping', 'live.pong', 'error'].includes(String(value.type)) ||
      !uuid(value.message_id) || !(value.request_id === null || uuid(value.request_id)) ||
      !(value.session_id === null || uuid(value.session_id)) || typeof value.seq !== 'number' ||
      !Number.isSafeInteger(value.seq) || value.seq < 1 || value.seq > 0xffffffff || !timestamp(value.sent_at) ||
      !record(value.payload)) throw new ApiFailure('invalid_response');
  let error: LiveFrame['error'] = null;
  if (value.error !== null) {
    if (!record(value.error) || !exact(value.error, ['code', 'message', 'retryable']) ||
        typeof value.error.code !== 'string' || !/^[a-z_]{1,64}$/.test(value.error.code) ||
        typeof value.error.message !== 'string' || value.error.message.length > 256 || typeof value.error.retryable !== 'boolean') {
      throw new ApiFailure('invalid_response');
    }
    error = { code: value.error.code, message: value.error.message, retryable: value.error.retryable };
  }
  const type = value.type as LiveFrame['type'];
  const payload = value.payload;
  validatePayload(type, payload, error);
  if ((type === 'error') !== (error !== null)) throw new ApiFailure('invalid_response');
  return {
    v: 1, type, message_id: value.message_id, request_id: value.request_id,
    session_id: value.session_id, seq: value.seq, sent_at: value.sent_at, payload, error
  };
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
