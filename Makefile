GO ?= go

.PHONY: contracts-check test-ledger-recovery test-ledger-redis bootstrap-dev config-check verify-infra-foundations up down smoke network-check test-protocol test-edge

contracts-check:
	sh scripts/contracts-check.sh

bootstrap-dev:
	bash scripts/bootstrap-dev.sh

config-check:
	bash scripts/config-check.sh

verify-infra-foundations: config-check
	python3 scripts/network-check.py --config-only

up: config-check
	docker compose -f infra/compose.yaml -f infra/compose.dev.yaml up --build --wait

down:
	docker compose -f infra/compose.yaml -f infra/compose.dev.yaml down

smoke:
	python3 scripts/smoke.py

network-check:
	python3 scripts/network-check.py

test-protocol:
	cd server && $(GO) test -race -count=1 ./internal/api ./internal/m1 ./internal/mocknode

test-ledger-recovery:
	cd server && $(GO) test -race -count=1 -v ./internal/domain ./internal/billing ./internal/request ./internal/api ./internal/m1 ./internal/mocknode

test-ledger-redis:
	@test -n "$${CORDOVA_TEST_REDIS_URL:-}" || { echo "CORDOVA_TEST_REDIS_URL must point to an isolated Redis instance with AOF enabled" >&2; exit 2; }
	cd server && $(GO) test -race -count=1 -v ./internal/billing ./internal/m1

test-edge:
	python3 scripts/test-edge.py
