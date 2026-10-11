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
| `WAITAOF` и connection-scoped fsync | Redis с AOF/`appendfsync always` вернул локальный fsync count `1` для `WAITAOF 1 0 1000`; интеграционная fault-injection suite прошла после потери worker-ответа до/после барьера | Fault hooks не убивают Redis-процесс посреди записи; нет реплики, production `WAITAOF 1 1` и закреплённый patch не проверены. |
| Kafka outbox повтор и consumer replay | Не запускалась: Kafka и Docker daemon недоступны | Реальные подтверждения broker и повторная доставка с теми же event IDs. |
| Токенизация, шаблон, BOS и слот | Не запускалась: нет GGUF и закреплённой runner-сборки | Сопоставление реальных token IDs/выходного текста, неизвестный шаблон fail-closed, отмена и повторное использование слота после очистки. |
| SOCKS5h удалённый DNS и fail-closed | Не запускалась: нет тестового proxy | Наблюдение DNS/соединений через прокси и подтверждение отсутствия прямого fallback. |
| Wallet prepare/relay ambiguity на stagenet | Не запускалась: wallet CLI/RPC недоступны | Реальные stagenet-ответы prepare/relay при timeout, restart и частичной трансляции; без mainnet. |

Следующее доступное действие для закрытия B0 — получить Go 1.27.2 в разрешённый toolchain cache, поднять изолированный локальный стек Redis/Kafka/wallet mock на работающем Docker engine, затем отдельно закрепить и проверить разрешённый GGUF-конверт. До этих результатов M0/B0 остаётся открытым.

## Проверка кода M1–M3 — 2026-10-09

### Go

| Проверка | Результат | Ограничение |
| --- | --- | --- |
| `go fmt ./...` | Прошла из `server/` | Выполнена установленным `go1.27.1 darwin/arm64`; проектный `go1.27.2` не установлен. |
| `go test ./...` | Все Go packages прошли после последнего fault-injection изменения | Redis integration cases пропускаются без `CORDOVA_TEST_REDIS_URL`; они отдельно прошли на временном Redis. |
| `go test -race -count=1 ./...` | Ранее прошли все Go packages; в этом снимке `make test-ledger-redis` повторно прошёл billing и M1 под `-race`, включая C3 enrollment, handshake, heartbeat, model state и session fencing на Redis | Полный race suite повторно не запускался после последнего C3 изменения; Redis integration cases в полном suite требуют `CORDOVA_TEST_REDIS_URL`. |
| `go vet ./...` | Прошла | Выполнена установленным `go1.27.1 darwin/arm64`; проектный `go1.27.2` не установлен. |
| `go build ./cmd/gateway ./cmd/mock-node` | Прошла | Компилирует Go gateway и mock-node; tiered router пока не подключён к gateway. |
| `make test-ledger-recovery` | Race tests ledger/coordinator, API idempotency/cancel, M1 request path и mock C6 routing прошли ранее | Цель не запускает Redis integration cases. |
| `make test-ledger-redis` | Прошла под `-race` с временным Redis 8.10.2, AOF/`appendfsync always` и изолированным Unix socket; billing и M1 suites прошли, включая новые fault-injection сценарии на atomic append/`WAITAOF` границах | Временный binary не закреплён проектом; Redis process-kill и replica barrier не запускались. |

Воспроизводимый Go-вызов в текущей среде:

```sh
cd server
GOCACHE=/private/tmp/cordova-go-build go test -race -count=1 ./...
```

### Остальные проверки

- `make contracts-check` прошёл после объединения изменений OpenAPI из `main` и C3 enrollment/auth/WS paths: 21 REST paths, 27 node WS types, 8 live WS types и 56 fixtures.
- Redis integration на временном Redis 8.10.2 с AOF/`appendfsync always` проверила enrollment, owner approval session/CSRF, challenge/token exchange, однократность JWT, C3 hello/welcome/catalog/heartbeat/model-state и reconnect fencing. Это не проверка полного запуска Go gateway вместе с Rust node.
- `cd node && RUSTUP_TOOLCHAIN=stable cargo fmt --all -- --check && RUSTUP_TOOLCHAIN=stable cargo test --workspace --offline` прошёл: 14 Rust tests. Использован установленный `stable`, а не зафиксированный проектом Rust 1.96.0.
- `make test-ledger-redis` прошла на временной Redis-сборке; Kafka replay, tokenizer parity, SOCKS5h и wallet stagenet проверки не подтверждены.
- `CORDOVA_TEST_REDIS_SERVER=/private/tmp/redis-8.10.2/src/redis-server make test-ledger-redis-process` прошла под `-race`: тест завершил собственный Redis после Lua append до `WAITAOF`, перезапустил его на том же AOF-каталоге, затем подтвердил orphan refund, нулевой reserved balance и journal replay. Это не проверяет убийство процесса посреди Redis-команды или replica fsync.
- Docker daemon недоступен из-за отсутствующего `/Users/abbas/.docker/run/docker.sock`; изолированный стек не запускался.

## Проверка C9 tokenizer runtime — 2026-10-11

### Среда и команды

| Проверка | Результат | Ограничение |
| --- | --- | --- |
| Go toolchain | `go1.27.2 windows/amd64` | Проверена локальная Windows-сборка; другие ОС и GPU не проверялись. |
| `go fmt ./...` | Прошла из `server/` | Форматирование исходников helper/runtime. |
| `go test ./...` | Все Go packages прошли, включая `cmd/tokenizer` и `internal/tokenizer` | Runtime HTTP-поведение покрыто локальными тестовыми серверами; llama.cpp и GGUF не запускались. |
| `go vet ./...` | Прошла из `server/` | Статическая проверка Go-кода. |
| `python scripts/contracts-check.py` | Прошла: 25 REST paths, 27 node WS types, 8 live WS types и 64 fixtures | Проверяет wire contracts/fixtures, не интеграцию native runner. |

### Граница доказательства

Реализованы исполняемый `cmd/tokenizer`, проверка подписи каталога, размера и SHA-256 локального GGUF, соответствия commit runner, однослотового процесса, загруженного пути и хеша chat template; unit tests подтверждают режимы `/apply-template` и `/tokenize`. Фактические token IDs, parity с Rust node, поведение закреплённой версии llama.cpp, отмена на реальном runner, очистка его слота, память и пропускная способность не проверялись: в checkout нет квалифицированных GGUF и runner release. Хеш tokenizer остаётся утверждением подписанного каталога до parity gate. B3/M2 и допуск оплаты остаются закрытыми до `make test-tokenizer-parity` на каждом принятом артефакте и закреплённом runner.

## Проверка SOCKS5h transport slice — 2026-10-11

| Проверка | Результат | Ограничение |
| --- | --- | --- |
| Go `fmt`, `test`, `vet` | Прошли локально на `go1.27.2 windows/amd64` | Для flaky warming assertion дополнительно прошли 20 повторов без `-race`; локальный Go сообщает, что race detector требует включённого cgo. |
| GitHub Actions `go test -race -count=1 ./...` | Прошёл в PR-run для PR #22 | Отдельный push-run до тестовой синхронизации поймал race в проверке фонового model warm; её download теперь блокируется до конца утверждения и локально прошла повторный прогон. |
| GitHub Actions Rust format, tests, clippy и dependency audit | Прошли в PR-run для PR #22, включая два новых SOCKS5h теста | Локальный Windows checkout не имеет Rust toolchain; фактический внешний SOCKS/DNS-сервер и другие ОС не использовались. |

Новые Rust-тесты проверяют, что нода передаёт непреобразованный hostname как SOCKS5 domain target, выполняет username/password negotiation и не соединяется с прямой целью при недоступном прокси. Полный B4 gate остаётся открытым до transport smoke, который докажет поведение reconnect, отозванных credentials, credential store и поддерживаемых платформ.

### Обработка отказа node-auth — 2026-10-11

Клиент завершает сессию после постоянного отказа авторизации (`401` на challenge, token или WebSocket handshake, а также WS close/error `unauthenticated`), а `408`, `429` и `5xx` оставляет для повторного подключения с backoff. GitHub Actions на Linux прошёл Rust format, workspace tests, clippy и dependency audit для коммита `8bd41d7`, включая HTTP-статусы, WebSocket 401 и close-frame `unauthenticated`. Реальный отзыв node-secret, проверка системного хранилища credentials и transport smoke на поддерживаемых ОС не выполнялись, поэтому B4 остаётся открытым.

## Выбор Kafka для локального Compose — 2026-10-11

Выбран официальный `apache/kafka:4.3.1`, опубликованный Apache Kafka как поддерживаемый релиз. В `contracts/versions.lock.json` записан multi-architecture index digest `sha256:77e3df9054047a88b520d0cc46e16696d3b22022e1d580aeccd2632df6532837`, полученный со страницы официального образа; Compose закрепляет этим digest и брокер, и одноразовый topic initializer. Локальная схема настроена на KRaft, private SASL/PLAIN, отдельные ACL gateway для usage topic и consumer group, отключённый auto-create и 30-дневное хранение usage.

CI Compose-запуск обнаружил требование StandardAuthorizer: внутренний controller listener представляет KRaft-запросы как `User:ANONYMOUS`, которого нужно допустить как super user. В локальном одновузловом варианте controller listener привязан только к loopback Kafka-контейнера; gateway видит только SASL broker listener, поэтому не получает controller-доступ. После этой правки нужно повторно пройти CI. Это не production-конфигурация: межхостовый TLS/SCRAM, трёхузловой кластер и репликация пока не реализованы. Источники: [KRaft StandardAuthorizer guidance](https://cwiki.apache.org/confluence/spaces/KAFKA/pages/195728007/KIP-801%2BImplement%2Ban%2BAuthorizer%2Bthat%2Bstores%2Bmetadata%2Bin%2B__cluster_metadata), [поддерживаемые релизы Apache Kafka](https://kafka.apache.org/downloads/), [официальный usage guide Docker-образа](https://github.com/apache/kafka/blob/trunk/docker/examples/README.md), [digest образа Apache Kafka 4.3.1](https://hub.docker.com/layers/apache/kafka/4.3.1).

Повторный M1 запуск подтвердил корректность topic/group/cluster ACL, но producer получал `UNKNOWN_SERVER_ERROR`. Broker log указал на отказ загрузки `/tmp/snappy-*.so` с `Operation not permitted`: franz-go сжимал usage batch через Snappy по умолчанию, а Kafka не могла загрузить Snappy JNI из защищённого `/tmp`. Gateway producer явно отключает сжатие для этих небольших metadata events; Kafka сохраняет прежний `noexec` tmpfs. M1 live usage, Go, frontend, contracts и Rust проверки прошли на commit `8cd608b` (GitHub Actions run `38127169519`). Источники: [franz-go compression options](https://pkg.go.dev/github.com/twmb/franz-go/pkg/kgo#ProducerBatchCompression), [Docker Compose tmpfs options](https://docs.docker.com/reference/compose-file/services/#tmpfs), [Xerial Snappy loader](https://github.com/xerial/snappy-java/blob/main/src/main/java/org/xerial/snappy/SnappyLoader.java).
