/* Generated from contracts/openapi.yaml and contracts/ws/v1.schema.json. Do not edit. */
export type Account = { "id": string; "username": string; "created_at": string; "totp_enabled": boolean; "privacy_notice_version": string; };

export type AccountErrorResponse = { "error": { "message": string; "type": "invalid_request_error" | "authentication_error" | "permission_error" | "rate_limit_error" | "server_error"; "param": string | null; "code": "invalid_request" | "unsupported_parameter" | "unauthenticated" | "forbidden" | "csrf_failed" | "not_found" | "conflict" | "insufficient_funds" | "spend_cap_exceeded" | "rate_limited" | "model_not_found" | "model_warming" | "no_capacity" | "node_failed" | "timeout" | "storage_unavailable" | "tokenizer_unavailable" | "wallet_unavailable" | "replay_unavailable" | "verification_required" | "enrollment_pending"; }; "request_id": Id; };

export type AccountSafeErrorFixtures = Array<AccountErrorResponse>;

export type ApiKey = { "id": Id; "name": string; "prefix": string; "permissions": Array<"chat:write" | "models:read" | "usage:read">; "model_ids": Array<string>; "daily_spend_cap_piconero": Money; "created_at": Timestamp; "last_used_at": string | null; "revoked_at": string | null; };

export type Balance = { "available_piconero": Piconero; "reserved_piconero": Piconero; "earnings_pending_piconero": Piconero; "earnings_available_piconero": Piconero; "payout_reserved_piconero": Piconero; "currency": "XMR"; "updated_at": string; };

export type Hardware = { "platform": "linux" | "windows" | "macos"; "arch": "x86_64" | "aarch64"; "backend": "cuda" | "metal" | "vulkan" | "cpu"; "gpu_name": string | null; "reported_total_vram_bytes": number; "reported_free_vram_bytes": number; "ram_bytes": number; };

export type Id = string;

export type LiveTicket = { "ticket": string; "expires_at": Timestamp; };

export type Money = string;

export type Node = { "id": Id; "name": string; "status": "enrolling" | "benchmarking" | "online" | "busy" | "warming" | "offline" | "draining" | "quarantined"; "rank": number | null; "rank_updated_at": string | null; "hardware": Hardware | null; "verified_capacity_bytes": number | null; "measured_tokens_per_second": number | null; "latency_ms": number | null; "uptime_ratio": number | null; "success_ratio": number | null; "last_seen_at": string | null; "models": Array<NodeModel>; "earnings_pending_piconero": Money | null; "earnings_available_piconero": Money | null; "hold_until": string | null; };

export type NodeLinkRequest = { "user_code": string; "name": string; };

export type NodeModel = { "model_id": string; "state": "loaded" | "on_disk" | "absent" | "downloading" | "loading" | "failed"; "active_slots": number; "max_slots": number; "verified_capacity_bytes": number; "updated_at": Timestamp; };

export type NodePage = { "data": Array<Node>; "next_cursor": string | null; };

export type NodeRenameRequest = { "name": string; };

export type Piconero = string;

export type RegistrationResult = { "account": Account; "csrf_token": string; "expires_at": Timestamp; "recovery_codes": Array<string>; };

export type SessionView = { "account": Account; "csrf_token": string; "expires_at": Timestamp; };

export type Timestamp = string;

export type UsagePage = { "data": Array<UsageRow>; "next_cursor": string | null; };

export type UsageRow = { "request_id": string; "api_key_id": string; "model_id": "cordova-chat-small-v1"; "status": "accepted" | "running" | "settled" | "refunded" | "cancelled" | "recovering" | "completed"; "input_tokens": number; "output_tokens": number; "charged_piconero": Piconero; "latency_ms": number; "created_at": string; "finished_at": string | null; };

type WsEnvelope = { "v": 1; "type": string; "message_id": WsId; "request_id": string | null; "session_id": string | null; "seq": number; "sent_at": WsTimestamp; "payload": Record<string, unknown>; "error": null | WsNodeError; };

type WsId = string;

type WsLiveError = { "code": "invalid_message" | "unauthenticated" | "stale_session" | "replayed_message" | "internal_error"; "message": string; "retryable": boolean; };

type WsLiveErrorPayload = { "operation_type": string; "attempt_id": null; "lease_id": null; "command_id": null; };

type WsLiveEventPayload = { "event_id": string; "topic": "nodes" | "usage" | "billing"; "resource_id": WsId; "revision": number; "action": "invalidate"; };

type WsLiveResyncPayload = { "reason": "cursor_expired" | "gap" | "slow_consumer"; };

type WsLiveSubscribePayload = { "topics": Array<"nodes" | "usage" | "billing">; "last_event_id": string | null; };

type WsLiveTimePayload = { "time": WsTimestamp; };

type WsLiveVariant = WsEnvelope & { "type"?: "live.auth"; "session_id"?: null; "payload"?: { "ticket": string; }; "error"?: null; } | WsEnvelope & { "type"?: "live.ready"; "payload"?: { "session_id": WsId; "account_id": WsId; "heartbeat_interval_ms": 15000; }; "error"?: null; } | WsEnvelope & { "type"?: "live.subscribe"; "payload"?: WsLiveSubscribePayload; "error"?: null; } | WsEnvelope & { "type"?: "live.event"; "payload"?: WsLiveEventPayload; "error"?: null; } | WsEnvelope & { "type"?: "live.resync"; "payload"?: WsLiveResyncPayload; "error"?: null; } | WsEnvelope & { "type"?: "live.ping"; "payload"?: WsLiveTimePayload; "error"?: null; } | WsEnvelope & { "type"?: "live.pong"; "payload"?: WsLiveTimePayload; "error"?: null; };

type WsNodeError = { "code": "unsupported_version" | "invalid_message" | "unauthenticated" | "replayed_message" | "stale_session" | "stale_lease" | "model_not_found" | "catalog_mismatch" | "insufficient_memory" | "no_capacity" | "download_failed" | "load_timeout" | "inference_failed" | "timeout" | "cancelled" | "output_limit" | "rate_limited" | "internal_error"; "message": string; "retryable": boolean; };

type WsTimestamp = string;

export type LiveServerFrame = WsEnvelope & { "type"?: "live.ready"; "payload"?: { "session_id": WsId; "account_id": WsId; "heartbeat_interval_ms": 15000; }; "error"?: null; } | WsEnvelope & { "type"?: "live.event"; "payload"?: WsLiveEventPayload; "error"?: null; } | WsEnvelope & { "type"?: "live.resync"; "payload"?: WsLiveResyncPayload; "error"?: null; } | WsEnvelope & { "type"?: "live.ping"; "payload"?: WsLiveTimePayload; "error"?: null; } | WsEnvelope & { "type"?: "live.pong"; "payload"?: WsLiveTimePayload; "error"?: null; } | WsEnvelope & { "type"?: "error"; "payload"?: WsLiveErrorPayload; "error"?: WsLiveError; } & unknown;

export type LiveClientFrame = WsEnvelope & { "type"?: "live.auth"; "session_id"?: null; "payload"?: { "ticket": string; }; "error"?: null; } | WsEnvelope & { "type"?: "live.subscribe"; "payload"?: WsLiveSubscribePayload; "error"?: null; } | WsEnvelope & { "type"?: "live.ping"; "payload"?: WsLiveTimePayload; "error"?: null; } | WsEnvelope & { "type"?: "live.pong"; "payload"?: WsLiveTimePayload; "error"?: null; } & unknown;

export type LiveEvent = Extract<LiveServerFrame, { type: 'live.event' }>['payload'];

export type LiveTopic = LiveEvent['topic'];

export type LiveAuth = Extract<LiveClientFrame, { type: 'live.auth' }>;

export type LiveSubscribe = Extract<LiveClientFrame, { type: 'live.subscribe' }>;

export const openApiContract = {"components":{"schemas":{"Account":{"type":"object","additionalProperties":false,"required":["id","username","created_at","totp_enabled","privacy_notice_version"],"properties":{"id":{"type":"string","format":"uuid"},"username":{"type":"string","minLength":3,"maxLength":64},"created_at":{"type":"string","format":"date-time"},"totp_enabled":{"type":"boolean"},"privacy_notice_version":{"type":"string","minLength":1,"maxLength":64}}},"AccountErrorResponse":{"description":"C2 safe browser/API error envelope; messages are safe fixed descriptions.","type":"object","required":["error","request_id"],"properties":{"error":{"type":"object","required":["message","type","param","code"],"properties":{"message":{"type":"string","maxLength":512},"type":{"type":"string","enum":["invalid_request_error","authentication_error","permission_error","rate_limit_error","server_error"]},"param":{"type":["string","null"],"maxLength":128},"code":{"type":"string","enum":["invalid_request","unsupported_parameter","unauthenticated","forbidden","csrf_failed","not_found","conflict","insufficient_funds","spend_cap_exceeded","rate_limited","model_not_found","model_warming","no_capacity","node_failed","timeout","storage_unavailable","tokenizer_unavailable","wallet_unavailable","replay_unavailable","verification_required","enrollment_pending"]}},"additionalProperties":false},"request_id":{"$ref":"#/components/schemas/Id"}},"additionalProperties":false},"AccountSafeErrorFixtures":{"type":"array","minItems":21,"maxItems":21,"items":{"$ref":"#/components/schemas/AccountErrorResponse"}},"ApiKey":{"type":"object","required":["id","name","prefix","permissions","model_ids","daily_spend_cap_piconero","created_at","last_used_at","revoked_at"],"properties":{"id":{"$ref":"#/components/schemas/Id"},"name":{"type":"string","minLength":1,"maxLength":64},"prefix":{"type":"string","minLength":1,"maxLength":32},"permissions":{"type":"array","minItems":1,"uniqueItems":true,"items":{"type":"string","enum":["chat:write","models:read","usage:read"]}},"model_ids":{"type":"array","minItems":1,"uniqueItems":true,"items":{"type":"string","pattern":"^[a-z0-9]+(?:-[a-z0-9]+)*$","maxLength":128}},"daily_spend_cap_piconero":{"$ref":"#/components/schemas/Money"},"created_at":{"$ref":"#/components/schemas/Timestamp"},"last_used_at":{"type":["string","null"],"format":"date-time"},"revoked_at":{"type":["string","null"],"format":"date-time"}},"additionalProperties":false},"Balance":{"type":"object","additionalProperties":false,"required":["available_piconero","reserved_piconero","earnings_pending_piconero","earnings_available_piconero","payout_reserved_piconero","currency","updated_at"],"properties":{"available_piconero":{"$ref":"#/components/schemas/Piconero"},"reserved_piconero":{"$ref":"#/components/schemas/Piconero"},"earnings_pending_piconero":{"$ref":"#/components/schemas/Piconero"},"earnings_available_piconero":{"$ref":"#/components/schemas/Piconero"},"payout_reserved_piconero":{"$ref":"#/components/schemas/Piconero"},"currency":{"const":"XMR","type":"string"},"updated_at":{"type":"string","format":"date-time"}}},"Hardware":{"type":"object","required":["platform","arch","backend","gpu_name","reported_total_vram_bytes","reported_free_vram_bytes","ram_bytes"],"properties":{"platform":{"type":"string","enum":["linux","windows","macos"]},"arch":{"type":"string","enum":["x86_64","aarch64"]},"backend":{"type":"string","enum":["cuda","metal","vulkan","cpu"]},"gpu_name":{"type":["string","null"],"maxLength":128},"reported_total_vram_bytes":{"type":"integer","minimum":0},"reported_free_vram_bytes":{"type":"integer","minimum":0},"ram_bytes":{"type":"integer","minimum":1}},"additionalProperties":false},"Id":{"type":"string","format":"uuid","pattern":"^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"},"LiveTicket":{"type":"object","required":["ticket","expires_at"],"properties":{"ticket":{"type":"string","minLength":32,"maxLength":512},"expires_at":{"$ref":"#/components/schemas/Timestamp"}},"additionalProperties":false},"Money":{"type":"string","pattern":"^(?:0|[1-8][0-9]{0,18}|9(?:0{0,18}))$","description":"Canonical piconero decimal string capped at 9000000000000000000."},"Node":{"type":"object","required":["id","name","status","rank","rank_updated_at","hardware","verified_capacity_bytes","measured_tokens_per_second","latency_ms","uptime_ratio","success_ratio","last_seen_at","models","earnings_pending_piconero","earnings_available_piconero","hold_until"],"properties":{"id":{"$ref":"#/components/schemas/Id"},"name":{"type":"string","minLength":1,"maxLength":64},"status":{"type":"string","enum":["enrolling","benchmarking","online","busy","warming","offline","draining","quarantined"]},"rank":{"type":["integer","null"],"minimum":0,"maximum":100,"description":"Null until the M4A rank projection has produced a verified score."},"rank_updated_at":{"type":["string","null"],"format":"date-time","description":"Null until a rank score exists."},"hardware":{"anyOf":[{"$ref":"#/components/schemas/Hardware"},{"type":"null"}],"description":"Null until the node reports hardware over its authenticated C3 session."},"verified_capacity_bytes":{"type":["integer","null"],"minimum":0,"description":"Null until a server capacity challenge verifies a lower bound."},"measured_tokens_per_second":{"type":["number","null"],"minimum":0,"description":"Null until a server-side benchmark records throughput."},"latency_ms":{"type":["integer","null"],"minimum":0},"uptime_ratio":{"type":["number","null"],"minimum":0,"maximum":1,"description":"Null until a sufficiently sampled reliability window exists."},"success_ratio":{"type":["number","null"],"minimum":0,"maximum":1,"description":"Null until a sufficiently sampled request window exists."},"last_seen_at":{"type":["string","null"],"format":"date-time"},"models":{"type":"array","items":{"$ref":"#/components/schemas/NodeModel"}},"earnings_pending_piconero":{"anyOf":[{"$ref":"#/components/schemas/Money"},{"type":"null"}],"description":"Null until M3 node-level earnings are available."},"earnings_available_piconero":{"anyOf":[{"$ref":"#/components/schemas/Money"},{"type":"null"}],"description":"Null until M3 node-level earnings are available."},"hold_until":{"type":["string","null"],"format":"date-time"}},"additionalProperties":false},"NodeLinkRequest":{"type":"object","required":["user_code","name"],"properties":{"user_code":{"type":"string","pattern":"^[A-Z2-7]{5}-[A-Z2-7]{5}-[A-Z2-7]{3}$"},"name":{"type":"string","minLength":1,"maxLength":64}},"additionalProperties":false},"NodeModel":{"type":"object","required":["model_id","state","active_slots","max_slots","verified_capacity_bytes","updated_at"],"properties":{"model_id":{"type":"string","pattern":"^[a-z0-9]+(?:-[a-z0-9]+)*$","maxLength":128},"state":{"type":"string","enum":["loaded","on_disk","absent","downloading","loading","failed"]},"active_slots":{"type":"integer","minimum":0},"max_slots":{"type":"integer","minimum":0},"verified_capacity_bytes":{"type":"integer","minimum":0},"updated_at":{"$ref":"#/components/schemas/Timestamp"}},"additionalProperties":false},"NodePage":{"type":"object","required":["data","next_cursor"],"properties":{"data":{"type":"array","maxItems":100,"items":{"$ref":"#/components/schemas/Node"}},"next_cursor":{"type":["string","null"],"maxLength":512}},"additionalProperties":false},"NodeRenameRequest":{"type":"object","required":["name"],"properties":{"name":{"type":"string","minLength":1,"maxLength":64}},"additionalProperties":false},"Piconero":{"type":"string","maxLength":19,"pattern":"^(0|[1-9][0-9]{0,18})$","description":"Canonical decimal piconero string, bounded by the server integer range."},"RegistrationResult":{"type":"object","required":["account","csrf_token","expires_at","recovery_codes"],"properties":{"account":{"$ref":"#/components/schemas/Account"},"csrf_token":{"type":"string","minLength":16,"maxLength":512},"expires_at":{"$ref":"#/components/schemas/Timestamp"},"recovery_codes":{"type":"array","minItems":1,"maxItems":32,"uniqueItems":true,"items":{"type":"string","minLength":8,"maxLength":128}}},"additionalProperties":false},"SessionView":{"type":"object","required":["account","csrf_token","expires_at"],"properties":{"account":{"$ref":"#/components/schemas/Account"},"csrf_token":{"type":"string","minLength":16,"maxLength":512},"expires_at":{"$ref":"#/components/schemas/Timestamp"}},"additionalProperties":false},"Timestamp":{"type":"string","format":"date-time"},"UsagePage":{"type":"object","required":["data","next_cursor"],"properties":{"data":{"type":"array","maxItems":100,"items":{"$ref":"#/components/schemas/UsageRow"}},"next_cursor":{"type":["string","null"],"maxLength":2048}},"additionalProperties":false},"UsageRow":{"type":"object","additionalProperties":false,"required":["request_id","api_key_id","model_id","status","input_tokens","output_tokens","charged_piconero","latency_ms","created_at","finished_at"],"properties":{"request_id":{"type":"string","format":"uuid"},"api_key_id":{"type":"string","format":"uuid","description":"Credential identity for key-scoped usage queries."},"model_id":{"type":"string","const":"cordova-chat-small-v1"},"status":{"type":"string","enum":["accepted","running","settled","refunded","cancelled","recovering","completed"]},"input_tokens":{"type":"integer","minimum":0},"output_tokens":{"type":"integer","minimum":0},"charged_piconero":{"$ref":"#/components/schemas/Piconero"},"latency_ms":{"type":"integer","minimum":0},"created_at":{"type":"string","format":"date-time"},"finished_at":{"type":["string","null"],"format":"date-time"}}}}}} as const;
export const webSocketContract = {"$defs":{"Envelope":{"type":"object","additionalProperties":false,"required":["v","type","message_id","request_id","session_id","seq","sent_at","payload","error"],"properties":{"v":{"const":1},"type":{"type":"string"},"message_id":{"$ref":"#/$defs/Id"},"request_id":{"type":["string","null"],"format":"uuid","pattern":"^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"},"session_id":{"type":["string","null"],"format":"uuid","pattern":"^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"},"seq":{"type":"integer","minimum":1,"maximum":4294967295},"sent_at":{"$ref":"#/$defs/Timestamp"},"payload":{"type":"object"},"error":{"anyOf":[{"type":"null"},{"$ref":"#/$defs/NodeError"}]}}},"Id":{"type":"string","format":"uuid","pattern":"^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"},"LiveError":{"type":"object","additionalProperties":false,"required":["code","message","retryable"],"properties":{"code":{"enum":["invalid_message","unauthenticated","stale_session","replayed_message","internal_error"]},"message":{"type":"string","minLength":1,"maxLength":256},"retryable":{"type":"boolean"}}},"LiveErrorPayload":{"type":"object","additionalProperties":false,"required":["operation_type","attempt_id","lease_id","command_id"],"properties":{"operation_type":{"type":"string","minLength":1,"maxLength":64},"attempt_id":{"type":"null"},"lease_id":{"type":"null"},"command_id":{"type":"null"}}},"LiveEventPayload":{"type":"object","additionalProperties":false,"required":["event_id","topic","resource_id","revision","action"],"properties":{"event_id":{"type":"string","minLength":1,"maxLength":256},"topic":{"enum":["nodes","usage","billing"]},"resource_id":{"$ref":"#/$defs/Id"},"revision":{"type":"integer","minimum":1},"action":{"const":"invalidate"}}},"LiveResyncPayload":{"type":"object","additionalProperties":false,"required":["reason"],"properties":{"reason":{"enum":["cursor_expired","gap","slow_consumer"]}}},"LiveSubscribePayload":{"type":"object","additionalProperties":false,"required":["topics","last_event_id"],"properties":{"topics":{"type":"array","minItems":1,"uniqueItems":true,"items":{"enum":["nodes","usage","billing"]}},"last_event_id":{"type":["string","null"],"maxLength":256}}},"LiveTimePayload":{"type":"object","additionalProperties":false,"required":["time"],"properties":{"time":{"$ref":"#/$defs/Timestamp"}}},"LiveVariant":{"oneOf":[{"allOf":[{"$ref":"#/$defs/Envelope"},{"properties":{"type":{"const":"live.auth"},"session_id":{"type":"null"},"payload":{"type":"object","additionalProperties":false,"required":["ticket"],"properties":{"ticket":{"type":"string","minLength":32}}},"error":{"type":"null"}}}]},{"allOf":[{"$ref":"#/$defs/Envelope"},{"properties":{"type":{"const":"live.ready"},"payload":{"type":"object","additionalProperties":false,"required":["session_id","account_id","heartbeat_interval_ms"],"properties":{"session_id":{"$ref":"#/$defs/Id"},"account_id":{"$ref":"#/$defs/Id"},"heartbeat_interval_ms":{"const":15000}}},"error":{"type":"null"}}}]},{"allOf":[{"$ref":"#/$defs/Envelope"},{"properties":{"type":{"const":"live.subscribe"},"payload":{"$ref":"#/$defs/LiveSubscribePayload"},"error":{"type":"null"}}}]},{"allOf":[{"$ref":"#/$defs/Envelope"},{"properties":{"type":{"const":"live.event"},"payload":{"$ref":"#/$defs/LiveEventPayload"},"error":{"type":"null"}}}]},{"allOf":[{"$ref":"#/$defs/Envelope"},{"properties":{"type":{"const":"live.resync"},"payload":{"$ref":"#/$defs/LiveResyncPayload"},"error":{"type":"null"}}}]},{"allOf":[{"$ref":"#/$defs/Envelope"},{"properties":{"type":{"const":"live.ping"},"payload":{"$ref":"#/$defs/LiveTimePayload"},"error":{"type":"null"}}}]},{"allOf":[{"$ref":"#/$defs/Envelope"},{"properties":{"type":{"const":"live.pong"},"payload":{"$ref":"#/$defs/LiveTimePayload"},"error":{"type":"null"}}}]}]},"NodeError":{"type":"object","additionalProperties":false,"required":["code","message","retryable"],"properties":{"code":{"enum":["unsupported_version","invalid_message","unauthenticated","replayed_message","stale_session","stale_lease","model_not_found","catalog_mismatch","insufficient_memory","no_capacity","download_failed","load_timeout","inference_failed","timeout","cancelled","output_limit","rate_limited","internal_error"]},"message":{"type":"string","minLength":1,"maxLength":256},"retryable":{"type":"boolean"}}},"Timestamp":{"type":"string","format":"date-time"}}} as const;
export const liveServerFrameSchema = {"allOf":[{"oneOf":[{"allOf":[{"$ref":"#/$defs/Envelope"},{"properties":{"type":{"const":"live.ready"},"payload":{"type":"object","additionalProperties":false,"required":["session_id","account_id","heartbeat_interval_ms"],"properties":{"session_id":{"$ref":"#/$defs/Id"},"account_id":{"$ref":"#/$defs/Id"},"heartbeat_interval_ms":{"const":15000}}},"error":{"type":"null"}}}]},{"allOf":[{"$ref":"#/$defs/Envelope"},{"properties":{"type":{"const":"live.event"},"payload":{"$ref":"#/$defs/LiveEventPayload"},"error":{"type":"null"}}}]},{"allOf":[{"$ref":"#/$defs/Envelope"},{"properties":{"type":{"const":"live.resync"},"payload":{"$ref":"#/$defs/LiveResyncPayload"},"error":{"type":"null"}}}]},{"allOf":[{"$ref":"#/$defs/Envelope"},{"properties":{"type":{"const":"live.ping"},"payload":{"$ref":"#/$defs/LiveTimePayload"},"error":{"type":"null"}}}]},{"allOf":[{"$ref":"#/$defs/Envelope"},{"properties":{"type":{"const":"live.pong"},"payload":{"$ref":"#/$defs/LiveTimePayload"},"error":{"type":"null"}}}]},{"allOf":[{"$ref":"#/$defs/Envelope"},{"properties":{"type":{"const":"error"},"payload":{"$ref":"#/$defs/LiveErrorPayload"},"error":{"$ref":"#/$defs/LiveError"}}}]}]},{"if":{"required":["type"],"properties":{"type":{"not":{"enum":["hello","live.auth"]}}}},"then":{"required":["session_id"],"properties":{"session_id":{"$ref":"#/$defs/Id"}}}}]} as const;
export const liveClientFrameSchema = {"allOf":[{"oneOf":[{"allOf":[{"$ref":"#/$defs/Envelope"},{"properties":{"type":{"const":"live.auth"},"session_id":{"type":"null"},"payload":{"type":"object","additionalProperties":false,"required":["ticket"],"properties":{"ticket":{"type":"string","minLength":32}}},"error":{"type":"null"}}}]},{"allOf":[{"$ref":"#/$defs/Envelope"},{"properties":{"type":{"const":"live.subscribe"},"payload":{"$ref":"#/$defs/LiveSubscribePayload"},"error":{"type":"null"}}}]},{"allOf":[{"$ref":"#/$defs/Envelope"},{"properties":{"type":{"const":"live.ping"},"payload":{"$ref":"#/$defs/LiveTimePayload"},"error":{"type":"null"}}}]},{"allOf":[{"$ref":"#/$defs/Envelope"},{"properties":{"type":{"const":"live.pong"},"payload":{"$ref":"#/$defs/LiveTimePayload"},"error":{"type":"null"}}}]}]},{"if":{"required":["type"],"properties":{"type":{"not":{"enum":["hello","live.auth"]}}}},"then":{"required":["session_id"],"properties":{"session_id":{"$ref":"#/$defs/Id"}}}}]} as const;

type Schema = Record<string, unknown>;
type ContractDocument = Record<string, unknown>;

function pointer(document: ContractDocument, reference: string): unknown {
  if (!reference.startsWith('#/')) throw new Error('external_reference');
  let value: unknown = document;
  for (const part of reference.slice(2).split('/')) {
    const key = part.replace(/~1/g, '/').replace(/~0/g, '~');
    if (typeof value !== 'object' || value === null || !(key in value)) throw new Error('missing_reference');
    value = (value as Record<string, unknown>)[key];
  }
  return value;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function matchesType(value: unknown, type: string): boolean {
  switch (type) {
    case 'object': return isRecord(value);
    case 'array': return Array.isArray(value);
    case 'string': return typeof value === 'string';
    case 'integer': return typeof value === 'number' && Number.isSafeInteger(value);
    case 'number': return typeof value === 'number' && Number.isFinite(value);
    case 'boolean': return typeof value === 'boolean';
    case 'null': return value === null;
    default: return false;
  }
}

function validate(value: unknown, schema: Schema, document: ContractDocument, depth = 0): void {
  if (depth > 64) throw new Error('schema_depth');
  if (typeof schema.$ref === 'string') {
    const target = pointer(document, schema.$ref);
    if (!isRecord(target)) throw new Error('invalid_reference');
    validate(value, target, document, depth + 1);
    return;
  }
  if (schema.not !== undefined) {
    if (matchesSchema(value, schema.not as Schema, document, depth + 1)) throw new Error('forbidden_value');
  }
  for (const key of ['anyOf', 'oneOf'] as const) {
    const choices = schema[key];
    if (Array.isArray(choices)) {
      const count = choices.filter((choice) => matchesSchema(value, choice as Schema, document, depth + 1)).length;
      if ((key === 'oneOf' && count !== 1) || (key === 'anyOf' && count < 1)) throw new Error('union_mismatch');
    }
  }
  const expected = schema.type;
  if (typeof expected === 'string' && !matchesType(value, expected)) throw new Error('type_mismatch');
  if (Array.isArray(expected) && !expected.some((type) => typeof type === 'string' && matchesType(value, type))) throw new Error('type_mismatch');
  if ('const' in schema && value !== schema.const) throw new Error('const_mismatch');
  if (Array.isArray(schema.enum) && !schema.enum.includes(value)) throw new Error('enum_mismatch');

  if (isRecord(value)) {
    const required = Array.isArray(schema.required) ? schema.required : [];
    for (const key of required) if (typeof key === 'string' && !Object.hasOwn(value, key)) throw new Error('required_missing');
    const properties = isRecord(schema.properties) ? schema.properties : {};
    for (const [key, item] of Object.entries(value)) {
      const child = properties[key];
      if (child !== undefined) validate(item, child as Schema, document, depth + 1);
      else if (schema.additionalProperties === false) throw new Error('unknown_property');
      else if (isRecord(schema.additionalProperties)) validate(item, schema.additionalProperties, document, depth + 1);
    }
  }
  if (Array.isArray(value)) {
    if (typeof schema.minItems === 'number' && value.length < schema.minItems) throw new Error('array_too_short');
    if (typeof schema.maxItems === 'number' && value.length > schema.maxItems) throw new Error('array_too_long');
    if (schema.uniqueItems === true && new Set(value.map((item) => JSON.stringify(item))).size !== value.length) throw new Error('duplicate_item');
    if (isRecord(schema.items)) for (const item of value) validate(item, schema.items, document, depth + 1);
  }
  if (typeof value === 'string') {
    if (typeof schema.minLength === 'number' && value.length < schema.minLength) throw new Error('string_too_short');
    if (typeof schema.maxLength === 'number' && value.length > schema.maxLength) throw new Error('string_too_long');
    if (typeof schema.pattern === 'string' && !new RegExp(schema.pattern).test(value)) throw new Error('pattern_mismatch');
    if (schema.format === 'uuid' && !/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(value)) throw new Error('invalid_uuid');
    if (schema.format === 'date-time' && (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(value) || !Number.isFinite(Date.parse(value)))) throw new Error('invalid_datetime');
  }
  if (typeof value === 'number') {
    if (typeof schema.minimum === 'number' && value < schema.minimum) throw new Error('below_minimum');
    if (typeof schema.maximum === 'number' && value > schema.maximum) throw new Error('above_maximum');
    if (typeof schema.exclusiveMinimum === 'number' && value <= schema.exclusiveMinimum) throw new Error('below_exclusive_minimum');
    if (typeof schema.exclusiveMaximum === 'number' && value >= schema.exclusiveMaximum) throw new Error('above_exclusive_maximum');
  }
  if (Array.isArray(schema.allOf)) for (const child of schema.allOf) validate(value, child as Schema, document, depth + 1);
  if (schema.if !== undefined && matchesSchema(value, schema.if as Schema, document, depth + 1) && schema.then !== undefined) {
    validate(value, schema.then as Schema, document, depth + 1);
  }
}

function matchesSchema(value: unknown, schema: Schema, document: ContractDocument, depth: number): boolean {
  try { validate(value, schema, document, depth); return true; }
  catch { return false; }
}

function schemaAt(document: ContractDocument, reference: string): Schema {
  const schema = pointer(document, reference);
  if (!isRecord(schema)) throw new Error('invalid_schema');
  return schema;
}

export function matchesOpenApi(value: unknown, name: keyof typeof openApiContract.components.schemas): boolean {
  return matchesSchema(value, { $ref: '#/components/schemas/' + name }, openApiContract as unknown as ContractDocument, 0);
}

export function matchesWebSocketSchema(value: unknown, reference: string): boolean {
  try { return matchesSchema(value, schemaAt(webSocketContract as unknown as ContractDocument, reference), webSocketContract as unknown as ContractDocument, 0); }
  catch { return false; }
}

export function matchesLiveServerFrame(value: unknown): value is LiveServerFrame {
  return matchesSchema(value, liveServerFrameSchema as unknown as Schema, webSocketContract as unknown as ContractDocument, 0);
}

export function matchesLiveClientFrame(value: unknown): value is LiveClientFrame {
  return matchesSchema(value, liveClientFrameSchema as unknown as Schema, webSocketContract as unknown as ContractDocument, 0);
}
