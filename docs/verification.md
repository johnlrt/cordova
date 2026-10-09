# B0 — проверка внешних предположений

Дата снимка: 2026-10-09. Это журнал фактической доступности среды, а не успешное прохождение `make verify-foundations`. В каждом блоке ниже отдельно указано, что подтверждено и что ещё нужно проверить на закреплённых артефактах или сервисах.

## Локальная среда

| Компонент | Наблюдение | Предел доказательства |
| --- | --- | --- |
| Хост | macOS 27.0.0, Darwin arm64 | Не подтверждает Linux/Windows и не соответствует RTX 3050 4 GB из README.md. Unified memory и GPU capacity не квалифицированы. |
| Go | В checkout указан `go 1.27.2`; установленный `go` — `go1.27.1 darwin/arm64` | Локальные проверки этого снимка реально выполнены Go 1.27.1; точный patch 1.27.2 не проверен. |
| llama.cpp runner | `llama-server --version`: `0.6.0`, build `11429`, commit `d81235049`, AppleClang 21.0.0.21000334, Darwin arm64 | Это локальный бинарник, а не выбранная и закреплённая версия из M0. Поведение tokenize/completion/очистки слота не проверено. |
| GGUF | В checkout нет файлов `*.gguf` | Артефакт, лицензия конкретного конверта, хеши, размер и воспроизводимость сборки не проверены. |
| Docker | `docker context ls` показывает `default` и активный `desktop-linux`; `docker info` для обоих завершается ошибкой отсутствующего Unix socket (`/var/run/docker.sock`, `/Users/abbas/.docker/run/docker.sock`) | Контейнеры не запущены; Redis/Kafka/wallet-RPC интеграции из этой среды недоступны. |
| Внешние клиенты | `redis-server` и `redis-cli` отсутствуют в `PATH`; временно собраны из официального Redis 8.10.2 (`download.redis.io`, SHA-256 `b9ffee226b5eecdba98a679260dad764b2a4ebd90dce4ad5ac9e9f3eef9c02b3`). Kafka CLI, Monero wallet CLI/RPC и SOCKS5-тестовый сервер не найдены. | Временная Redis-сборка не закреплена в проекте M0; optional modules не собраны и не использовались. Остальные внешние пробы не выполнялись. |

## Кандидат модели

Общий план выбирает `Qwen2.5-1.5B-Instruct`, затем требует закрепить точную upstream-ревизию, преобразование в GGUF, license notice, хеши артефакта/tokenizer/template и подписанный тестовый manifest. Официальная карточка модели указывает лицензию Apache-2.0; это подтверждает лицензионную метку кандидата, но не происхождение или условия конкретного GGUF-конверта.

Пока не загружены и не зафиксированы веса, tokenizer, chat template и конвертер, поэтому нет подтверждённых token-ID/BOS parity, контекстной границы, расхода памяти, скорости, отмены или очистки runner slot. Модель не допущена к каталогу.

## Интеграционные пробы B0

| Проба | Результат этого снимка | Нужное доказательство |
| --- | --- | --- |
| Redis Lua: точность выше `2^53`, повтор операции и wrong key type | Прошла на временной Redis 8.10.2 с UUID-scoped ключами; подтвердились retry, settlement, cancellation, recovery и journal replay | Проверена одна незакреплённая версия; точное поведение около `int64` не проверялось. |
| `WAITAOF` и connection-scoped fsync | Redis с AOF/`appendfsync always` вернул локальный fsync count `1` для `WAITAOF 1 0 1000`; financial integration suite прошла | Нет реплики и timeout/crash injection на каждой барьерной границе; production `WAITAOF 1 1` и закреплённый patch не проверены. |
| Kafka outbox повтор и consumer replay | Не запускалась: Kafka и Docker daemon недоступны | Реальные подтверждения broker и повторная доставка с теми же event IDs. |
| Токенизация, шаблон, BOS и слот | Не запускалась: нет GGUF и закреплённой runner-сборки | Сопоставление реальных token IDs/выходного текста, неизвестный шаблон fail-closed, отмена и повторное использование слота после очистки. |
| SOCKS5h удалённый DNS и fail-closed | Не запускалась: нет тестового proxy | Наблюдение DNS/соединений через прокси и подтверждение отсутствия прямого fallback. |
| Wallet prepare/relay ambiguity на stagenet | Не запускалась: wallet CLI/RPC недоступны | Реальные stagenet-ответы prepare/relay при timeout, restart и частичной трансляции; без mainnet. |

Следующее доступное действие для закрытия B0 — получить Go 1.27.2 в разрешённый toolchain cache, поднять изолированный локальный стек Redis/Kafka/wallet mock на работающем Docker engine, затем отдельно закрепить и проверить разрешённый GGUF-конверт. До этих результатов M0/B0 остаётся открытым.

## Проверка кода M1–M3 — 2026-10-09

### Go

| Проверка | Результат | Ограничение |
| --- | --- | --- |
| `go fmt ./...` | Прошла из `server/` | Выполнена установленным `go1.27.2`. |
| `go test -count=1 ./...` | Все Go packages прошли, включая coordinator/API route errors и mock C6 tiered router | Redis integration cases пропускаются, когда не задан `CORDOVA_TEST_REDIS_URL`. |
| `go test -race -count=1 ./...` | Все Go packages прошли под race detector, включая C6 selection filters, загрузку/прогрев моделей и fencing тесты | Redis integration cases пропускаются, когда не задан `CORDOVA_TEST_REDIS_URL`; C3 live sessions не реализованы. |
| `go vet ./...` | Прошла | Выполнена установленным `go1.27.2`. |
| `go build ./cmd/gateway ./cmd/mock-node` | Прошла | Компилирует Go gateway и mock-node; tiered router пока не подключён к gateway. |
| `make test-ledger-recovery` | Race tests ledger/coordinator, API idempotency/cancel, M1 request path и mock C6 routing прошли | Redis integration cases пропускаются, когда не задан `CORDOVA_TEST_REDIS_URL`; crash injection на каждой Redis write/barrier границе не выполнен. |
| `make test-ledger-redis` | Прошла с Redis 8.10.2, AOF/`appendfsync always` и локальным Unix socket URL; billing и M1 integration tests прошли под `-race` | Временный binary не закреплён проектом; replica barrier и crash injection не запускались. |

Воспроизводимый Go-вызов в текущей среде:

```sh
cd server
GOCACHE=/private/tmp/cordova-go-build go test -race -count=1 ./...
```

### Остальные проверки

- `make contracts-check` прошёл после обновления M1/M3 idempotency/error contracts: 15 REST paths, 27 node WS types, 8 live WS types и 30 fixtures; Redis-backed handler integration не запускался.
- `cd node && RUSTUP_TOOLCHAIN=stable cargo fmt --all -- --check && RUSTUP_TOOLCHAIN=stable cargo test --workspace --offline` прошёл: 14 Rust tests. Использован установленный `stable`, а не зафиксированный проектом Rust 1.96.0.
- `make test-ledger-redis` прошла на временной Redis-сборке; Kafka replay, tokenizer parity, SOCKS5h и wallet stagenet проверки не подтверждены.
- Docker daemon недоступен из-за отсутствующего `/Users/abbas/.docker/run/docker.sock`; изолированный стек не запускался.
