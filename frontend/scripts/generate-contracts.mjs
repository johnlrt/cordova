import { mkdtemp, readFile, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const frontendRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const repositoryRoot = resolve(frontendRoot, '..');
const outputPath = resolve(frontendRoot, 'src/lib/api/generated/contracts.ts');
const openApi = JSON.parse(await readFile(resolve(repositoryRoot, 'contracts/openapi.yaml'), 'utf8'));
const webSocket = JSON.parse(await readFile(resolve(repositoryRoot, 'contracts/ws/v1.schema.json'), 'utf8'));

const schemaRef = (branch) => branch.allOf?.find((item) => item.properties?.type)?.properties.type.const;
const inboundTypes = new Set([
  ...webSocket['x-message-directions'].live['server-to-front'],
  ...webSocket['x-message-directions'].live.both
]);
const liveBranches = webSocket.$defs.LiveVariant.oneOf.filter((branch) => inboundTypes.has(schemaRef(branch)));
if (liveBranches.length !== inboundTypes.size - 1) throw new Error('live direction registry does not match LiveVariant');
const outboundTypes = new Set([
  ...webSocket['x-message-directions'].live['front-to-server'],
  ...webSocket['x-message-directions'].live.both.filter((type) => type !== 'error')
]);
const clientBranches = webSocket.$defs.LiveVariant.oneOf.filter((branch) => outboundTypes.has(schemaRef(branch)));
if (clientBranches.length !== outboundTypes.size) throw new Error('live client direction registry does not match LiveVariant');
const errorBranch = structuredClone(webSocket.$defs.ErrorVariant);
errorBranch.allOf[1].properties.payload = { $ref: '#/$defs/LiveErrorPayload' };
errorBranch.allOf[1].properties.error = { $ref: '#/$defs/LiveError' };
const liveInvariants = structuredClone(webSocket.allOf ?? []);
const liveServerFrameSchema = { allOf: [{ oneOf: [...liveBranches, errorBranch] }, ...liveInvariants] };
const liveClientFrameSchema = { allOf: [{ oneOf: clientBranches }, ...liveInvariants] };

const openApiRoots = [
  'AccountErrorResponse', 'AccountSafeErrorFixtures', 'SessionView', 'LiveTicket', 'RegistrationResult', 'UsagePage',
  'Node', 'ApiKey', 'APIKeyList', 'APIKeyCreateResponse', 'Balance'
];
const openApiNames = dependencyNames(openApi.components.schemas, openApiRoots);
const wsRefs = collectRefs([liveServerFrameSchema, liveClientFrameSchema]);
const wsRoots = [...wsRefs].map((ref) => ref.startsWith('#/$defs/') ? ref.slice('#/$defs/'.length) : null).filter(Boolean);
const wsNames = dependencyNames(webSocket.$defs, [...wsRoots, 'LiveVariant']);
const openApiRuntimeDocument = {
  components: {
    schemas: Object.fromEntries([...openApiNames].sort().map((name) => [name, openApi.components.schemas[name]]))
  }
};
const webSocketRuntimeDocument = {
  $defs: Object.fromEntries([...wsNames].sort().map((name) => [name, webSocket.$defs[name]]))
};

const aliases = [];
for (const name of [...openApiNames].sort()) {
  aliases.push(`export type ${name} = ${schemaToType(openApi.components.schemas[name], openApi, 'openapi')};`);
}
for (const name of [...wsNames].sort()) {
  aliases.push(`type Ws${name} = ${schemaToType(webSocket.$defs[name], webSocket, 'websocket')};`);
}
aliases.push(`export type LiveServerFrame = ${schemaToType(liveServerFrameSchema, webSocket, 'websocket')};`);
aliases.push(`export type LiveClientFrame = ${schemaToType(liveClientFrameSchema, webSocket, 'websocket')};`);
aliases.push(`export type LiveEvent = Extract<LiveServerFrame, { type: 'live.event' }>['payload'];`);
aliases.push(`export type LiveTopic = LiveEvent['topic'];`);
aliases.push(`export type LiveAuth = Extract<LiveClientFrame, { type: 'live.auth' }>;`);
aliases.push(`export type LiveSubscribe = Extract<LiveClientFrame, { type: 'live.subscribe' }>;`);

const generated = `/* Generated from contracts/openapi.yaml and contracts/ws/v1.schema.json. Do not edit. */
${aliases.join('\n\n')}

export const openApiContract = ${JSON.stringify(openApiRuntimeDocument)} as const;
export const webSocketContract = ${JSON.stringify(webSocketRuntimeDocument)} as const;
export const liveServerFrameSchema = ${JSON.stringify(liveServerFrameSchema)} as const;
export const liveClientFrameSchema = ${JSON.stringify(liveClientFrameSchema)} as const;

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
    if (schema.format === 'date-time' && (!/^\\d{4}-\\d{2}-\\d{2}T\\d{2}:\\d{2}:\\d{2}(?:\\.\\d+)?(?:Z|[+-]\\d{2}:\\d{2})$/.test(value) || !Number.isFinite(Date.parse(value)))) throw new Error('invalid_datetime');
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
`;

if (process.argv.includes('--check')) {
  const temporaryDirectory = await mkdtemp(resolve(tmpdir(), 'cordova-contracts-'));
  try {
    const temporaryFile = resolve(temporaryDirectory, 'contracts.ts');
    await writeFile(temporaryFile, generated, 'utf8');
    const current = await readFile(outputPath, 'utf8').catch(() => '');
    const regenerated = await readFile(temporaryFile, 'utf8');
    if (current !== regenerated) {
      console.error('Generated frontend contracts are out of date. Run npm run contracts:generate.');
      process.exitCode = 1;
    } else {
      console.log('PASS: generated frontend contracts match OpenAPI and WebSocket sources.');
    }
  } finally { await rm(temporaryDirectory, { recursive: true, force: true }); }
} else {
  await import('node:fs/promises').then(({ mkdir }) => mkdir(dirname(outputPath), { recursive: true }));
  await writeFile(outputPath, generated, 'utf8');
  console.log('Generated frontend contract types and validators.');
}

function collectRefs(value, result = new Set()) {
  if (Array.isArray(value)) for (const child of value) collectRefs(child, result);
  else if (value && typeof value === 'object') {
    if (typeof value.$ref === 'string') result.add(value.$ref);
    for (const child of Object.values(value)) collectRefs(child, result);
  }
  return result;
}

function dependencyNames(definitions, roots) {
  const pending = [...roots];
  const names = new Set();
  while (pending.length) {
    const name = pending.pop();
    if (!name || names.has(name) || !definitions[name]) continue;
    names.add(name);
    for (const ref of collectRefs(definitions[name])) {
      const prefix = definitions === openApi.components.schemas ? '#/components/schemas/' : '#/$defs/';
      if (ref.startsWith(prefix)) pending.push(ref.slice(prefix.length));
    }
  }
  return names;
}

function schemaToType(schema, document, kind, depth = 0) {
  if (depth > 30) return 'unknown';
  if (schema.$ref) {
    const prefix = kind === 'openapi' ? '#/components/schemas/' : '#/$defs/';
    const name = schema.$ref.startsWith(prefix) ? schema.$ref.slice(prefix.length) : null;
    return name ? (kind === 'websocket' ? `Ws${name}` : name) : 'unknown';
  }
  if ('const' in schema) return JSON.stringify(schema.const);
  if (Array.isArray(schema.enum)) return schema.enum.map((item) => JSON.stringify(item)).join(' | ');
  for (const composition of ['oneOf', 'anyOf']) {
    if (Array.isArray(schema[composition])) return schema[composition].map((child) => schemaToType(child, document, kind, depth + 1)).join(' | ');
  }
  if (Array.isArray(schema.allOf)) return schema.allOf.map((child) => schemaToType(child, document, kind, depth + 1)).join(' & ');
  if (Array.isArray(schema.type)) return [...new Set(schema.type.map((type) => schemaToType({ type }, document, kind, depth + 1)))].join(' | ');
  if (schema.type === 'array') return `Array<${schemaToType(schema.items ?? {}, document, kind, depth + 1)}>`;
  if (schema.type === 'object' || schema.properties) {
    const properties = schema.properties ?? {};
    const required = new Set(schema.required ?? []);
    const entries = Object.entries(properties).map(([key, child]) => `${JSON.stringify(key)}${required.has(key) ? '' : '?'}: ${schemaToType(child, document, kind, depth + 1)};`);
    if (schema.additionalProperties && typeof schema.additionalProperties === 'object') entries.push(`[key: string]: ${schemaToType(schema.additionalProperties, document, kind, depth + 1)};`);
    return entries.length ? `{ ${entries.join(' ')} }` : 'Record<string, unknown>';
  }
  return ({ string: 'string', integer: 'number', number: 'number', boolean: 'boolean', null: 'null' })[schema.type] ?? 'unknown';
}
