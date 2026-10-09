#!/usr/bin/env python3
"""Dependency-free structural and fixture checks for the initial B0 contracts."""

from __future__ import annotations

import json
import re
import sys
from datetime import datetime
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
CONTRACTS = ROOT / "contracts"


class ContractError(Exception):
    pass


def unique_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ContractError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def load_json(path: Path) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=unique_object)
    except (OSError, json.JSONDecodeError) as error:
        raise ContractError(f"{path.relative_to(ROOT)}: {error}") from error


def pointer(document: Any, fragment: str) -> Any:
    if fragment == "" or fragment == "#":
        return document
    if not fragment.startswith("#/"):
        raise ContractError(f"unsupported reference fragment: {fragment}")
    value = document
    for token in fragment[2:].split("/"):
        token = token.replace("~1", "/").replace("~0", "~")
        try:
            value = value[int(token)] if isinstance(value, list) else value[token]
        except (KeyError, IndexError, ValueError, TypeError) as error:
            raise ContractError(f"unresolved reference: {fragment}") from error
    return value


def check_refs(document: Any, label: str) -> None:
    def visit(value: Any) -> None:
        if isinstance(value, dict):
            ref = value.get("$ref")
            if ref is not None:
                if not isinstance(ref, str) or not ref.startswith("#"):
                    raise ContractError(f"{label}: only local references are allowed, found {ref!r}")
                pointer(document, ref)
            for child in value.values():
                visit(child)
        elif isinstance(value, list):
            for child in value:
                visit(child)

    visit(document)


def matches_type(value: Any, expected: str) -> bool:
    return {
        "object": lambda: isinstance(value, dict),
        "array": lambda: isinstance(value, list),
        "string": lambda: isinstance(value, str),
        "integer": lambda: isinstance(value, int) and not isinstance(value, bool),
        "number": lambda: isinstance(value, (int, float)) and not isinstance(value, bool),
        "boolean": lambda: isinstance(value, bool),
        "null": lambda: value is None,
    }[expected]()


def validate(value: Any, schema: dict[str, Any], document: Any, path: str = "$", depth: int = 0) -> None:
    if depth > 64:
        raise ContractError(f"{path}: schema nesting limit exceeded")
    if "$ref" in schema:
        validate(value, pointer(document, schema["$ref"]), document, path, depth + 1)
        return
    if "not" in schema:
        try:
            validate(value, schema["not"], document, path, depth + 1)
        except ContractError:
            pass
        else:
            raise ContractError(f"{path}: value matches a forbidden schema")

    for keyword, minimum_matches in (("anyOf", 1), ("oneOf", 1)):
        if keyword in schema:
            matched = 0
            for choice in schema[keyword]:
                try:
                    validate(value, choice, document, path, depth + 1)
                    matched += 1
                except ContractError:
                    pass
            if (keyword == "oneOf" and matched != 1) or (keyword == "anyOf" and matched < minimum_matches):
                raise ContractError(f"{path}: does not match {keyword}")

    expected = schema.get("type")
    if expected is not None:
        allowed = expected if isinstance(expected, list) else [expected]
        if not any(matches_type(value, item) for item in allowed):
            raise ContractError(f"{path}: expected {allowed}, received {type(value).__name__}")
    if "const" in schema and value != schema["const"]:
        raise ContractError(f"{path}: value does not match const")
    if "enum" in schema and value not in schema["enum"]:
        raise ContractError(f"{path}: value is not in enum")

    if isinstance(value, dict):
        missing = set(schema.get("required", [])) - value.keys()
        if missing:
            raise ContractError(f"{path}: missing required fields {sorted(missing)}")
        properties = schema.get("properties", {})
        for name, child in value.items():
            if name in properties:
                validate(child, properties[name], document, f"{path}.{name}", depth + 1)
            elif schema.get("additionalProperties") is False:
                raise ContractError(f"{path}: unknown field {name!r}")
            elif isinstance(schema.get("additionalProperties"), dict):
                validate(child, schema["additionalProperties"], document, f"{path}.{name}", depth + 1)

    if isinstance(value, list):
        if len(value) < schema.get("minItems", 0) or len(value) > schema.get("maxItems", float("inf")):
            raise ContractError(f"{path}: array length is outside limits")
        if schema.get("uniqueItems") and len({json.dumps(item, sort_keys=True) for item in value}) != len(value):
            raise ContractError(f"{path}: array items must be unique")
        items = schema.get("items")
        if items is False and value:
            raise ContractError(f"{path}: additional array items are forbidden")
        if isinstance(items, dict):
            for index, child in enumerate(value):
                validate(child, items, document, f"{path}[{index}]", depth + 1)
        if "contains" in schema:
            matched = 0
            for index, child in enumerate(value):
                try:
                    validate(child, schema["contains"], document, f"{path}[{index}]", depth + 1)
                    matched += 1
                except ContractError:
                    pass
            if matched < schema.get("minContains", 1):
                raise ContractError(f"{path}: array does not contain enough matching items")
        for index, item_schema in enumerate(schema.get("prefixItems", [])):
            if index < len(value):
                validate(value[index], item_schema, document, f"{path}[{index}]", depth + 1)

    if isinstance(value, str):
        if len(value) < schema.get("minLength", 0) or len(value) > schema.get("maxLength", float("inf")):
            raise ContractError(f"{path}: string length is outside limits")
        if "pattern" in schema and re.search(schema["pattern"], value) is None:
            raise ContractError(f"{path}: string does not match pattern")
        if schema.get("format") == "uuid" and re.fullmatch(
            r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}", value
        ) is None:
            raise ContractError(f"{path}: invalid UUID")
        if schema.get("format") == "date-time":
            try:
                parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
            except ValueError as error:
                raise ContractError(f"{path}: invalid date-time") from error
            if parsed.tzinfo is None:
                raise ContractError(f"{path}: date-time must include a timezone")
        if "x-max-utf8-bytes" in schema and len(value.encode("utf-8")) > schema["x-max-utf8-bytes"]:
            raise ContractError(f"{path}: UTF-8 byte limit exceeded")

    if isinstance(value, (int, float)) and not isinstance(value, bool):
        if value < schema.get("minimum", float("-inf")) or value > schema.get("maximum", float("inf")):
            raise ContractError(f"{path}: number is outside limits")
        if value <= schema.get("exclusiveMinimum", float("-inf")) or value >= schema.get("exclusiveMaximum", float("inf")):
            raise ContractError(f"{path}: number is outside exclusive limits")

    for subschema in schema.get("allOf", []):
        validate(value, subschema, document, path, depth + 1)
    condition = schema.get("if")
    if condition is not None:
        try:
            validate(value, condition, document, path, depth + 1)
        except ContractError:
            pass
        else:
            if "then" in schema:
                validate(value, schema["then"], document, path, depth + 1)


def main() -> int:
    try:
        openapi = load_json(CONTRACTS / "openapi.yaml")
        websocket = load_json(CONTRACTS / "ws/v1.schema.json")
        tokenizer = load_json(CONTRACTS / "internal/tokenizer.schema.json")
        if openapi.get("openapi") != "3.1.0":
            raise ContractError("openapi.yaml: expected OpenAPI 3.1.0")
        expected_paths = {
            "/healthz", "/v1/models", "/v1/chat/completions",
            "/api/auth/register", "/api/auth/login", "/api/auth/recover", "/api/auth/logout",
            "/api/session", "/api/keys", "/api/keys/{id}", "/api/usage", "/api/billing/balance", "/api/requests/{id}",
            "/api/live-ticket", "/api/live", "/api/node/enrollments", "/api/node/enrollments/token",
            "/api/node/enrollments/approve", "/api/node/auth/challenge", "/api/node/auth/token", "/api/node/ws",
        }
        if set(openapi.get("paths", {})) != expected_paths:
            raise ContractError("openapi.yaml: route inventory differs from the implemented Stage 1 and M1 API")
        required_m1_schemas = {
            "Account", "RegisterRequest", "LoginRequest", "RecoverRequest", "SessionResponse",
            "APIKeyCreateRequest", "APIKeyCreateResponse", "APIKey", "APIKeyList", "UsageRow",
            "UsageList", "LiveTicketResponse", "Balance", "RequestStatus",
        }
        if not required_m1_schemas.issubset(openapi.get("components", {}).get("schemas", {})):
            raise ContractError("openapi.yaml: M1 account, key, usage or live-ticket schemas are incomplete")
        live_security = openapi["paths"]["/api/live"]["get"].get("security", [])
        if not any("SessionCookie" in requirement for requirement in live_security):
            raise ContractError("openapi.yaml: dashboard live socket must use a session cookie")
        if "/readyz" in openapi["paths"] or "/metrics" in openapi["paths"]:
            raise ContractError("openapi.yaml: private readiness/metrics endpoints must not be public API routes")
        for path, item in openapi["paths"].items():
            for method, operation in item.items():
                if method.lower() not in {"get", "post", "put", "patch", "delete"}:
                    continue
                if "operationId" not in operation or not operation.get("responses"):
                    raise ContractError(f"openapi.yaml: incomplete operation {method.upper()} {path}")
        check_refs(openapi, "openapi.yaml")
        check_refs(websocket, "ws/v1.schema.json")
        check_refs(tokenizer, "internal/tokenizer.schema.json")

        node_types = {
            branch["allOf"][1]["properties"]["type"]["const"]
            for branch in websocket["$defs"]["NodeVariant"]["oneOf"]
        }
        expected_node_to_server = {
            "hello", "heartbeat", "model.state", "model.accepted", "model.progress", "model.ready",
            "model.reject", "inference.accepted", "inference.delta", "inference.complete",
            "inference.cancelled", "benchmark.delta", "benchmark.result", "drain.ack", "goodbye",
        }
        expected_server_to_node = {
            "welcome", "heartbeat.ack", "catalog.get", "catalog.snapshot", "model.query", "model.load",
            "model.download", "inference.start", "inference.cancel", "benchmark.start", "drain",
        }
        expected_node_types = expected_node_to_server | expected_server_to_node
        live_types = {
            branch["allOf"][1]["properties"]["type"]["const"]
            for branch in websocket["$defs"]["LiveVariant"]["oneOf"]
        }
        expected_live_types = {"live.auth", "live.ready", "live.subscribe", "live.event", "live.resync", "live.ping", "live.pong"}
        error_type = websocket["$defs"]["ErrorVariant"]["allOf"][1]["properties"]["type"]["const"]
        if node_types != expected_node_types or live_types != expected_live_types or error_type != "error":
            raise ContractError("ws/v1.schema.json: type registry differs from shared contracts C3/C4")
        expected_directions = {
            "node": {"node-to-server": expected_node_to_server, "server-to-node": expected_server_to_node, "both": {"error"}},
            "live": {"front-to-server": {"live.auth", "live.subscribe"},
                     "server-to-front": {"live.ready", "live.event", "live.resync"},
                     "both": {"live.ping", "live.pong", "error"}},
        }
        directions = websocket["x-message-directions"]
        for channel, expected in expected_directions.items():
            actual = {name: set(types) for name, types in directions[channel].items()}
            if actual != expected:
                raise ContractError(f"ws/v1.schema.json: {channel} direction map differs from C3/C4")
            all_types = [message for types in actual.values() for message in types]
            if len(all_types) != len(set(all_types)):
                raise ContractError(f"ws/v1.schema.json: duplicate {channel} direction assignment")

        expected_tokenizer_defs = {
            "PrepareRequest", "PrepareResponse", "CountRequest", "CountResponse",
            "ErrorResponse", "Health", "Ready",
        }
        if not expected_tokenizer_defs.issubset(tokenizer.get("$defs", {})):
            raise ContractError("internal/tokenizer.schema.json: C9 schema definitions are incomplete")
        expected_tokenizer_routes = {
            "POST /internal/tokenizer/prepare",
            "POST /internal/tokenizer/count",
            "GET /healthz",
            "GET /readyz",
        }
        endpoint_map = tokenizer.get("x-endpoints", {})
        if set(endpoint_map) != expected_tokenizer_routes:
            raise ContractError("internal/tokenizer.schema.json: endpoint inventory differs from C9")
        for endpoint in endpoint_map.values():
            for field in ("request", "response"):
                if field in endpoint:
                    pointer(tokenizer, endpoint[field])
        auth = tokenizer.get("x-authentication", {})
        if auth.get("header") != "Authorization" or auth.get("credential-scope") != "tokenizer-helper-only" or auth.get("public-edge-exposed") is not False:
            raise ContractError("internal/tokenizer.schema.json: helper authentication boundary differs from C9")
        limits = tokenizer.get("x-limits", {})
        expected_limits = {
            "body-bytes": 1048576,
            "messages-per-prepare": 128,
            "message-text-bytes": 262144,
            "count-text-bytes": 1048576,
            "concurrent-operations-per-model": 2,
            "queued-operations": 100,
            "max-queue-wait-ms": 500,
            "operation-deadline-ms": 2000,
            "overload-status": 503,
        }
        if any(limits.get(name) != expected for name, expected in expected_limits.items()):
            raise ContractError("internal/tokenizer.schema.json: helper bounds differ from C9")

        fixture_index = load_json(CONTRACTS / "fixtures/index.json")
        for fixture in fixture_index:
            document_name, fragment = fixture["schema"].split("#", 1)
            documents = {
                "openapi.yaml": openapi,
                "ws/v1.schema.json": websocket,
                "internal/tokenizer.schema.json": tokenizer,
            }
            document = documents[document_name]
            value = load_json(CONTRACTS / "fixtures" / fixture["file"])
            schema = pointer(document, "#" + fragment)
            try:
                validate(value, schema, document)
                accepted = True
            except ContractError:
                accepted = False
            if accepted != fixture["valid"]:
                expectation = "valid" if fixture["valid"] else "invalid"
                raise ContractError(f"{fixture['file']}: expected fixture to be {expectation}")

        print(f"PASS: {len(expected_paths)} REST paths, {len(node_types) + 1} node WS types, {len(live_types) + 1} live WS types, {len(fixture_index)} fixtures")
        return 0
    except (ContractError, KeyError, TypeError) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
