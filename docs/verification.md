# B0 — проверка внешних предположений

Дата снимка: 2026-10-09. Это журнал фактической доступности среды, а не успешное прохождение `make verify-foundations`. В каждом блоке ниже отдельно указано, что подтверждено и что ещё нужно проверить на закреплённых артефактах или сервисах.

## Локальная среда

| Компонент | Наблюдение | Предел доказательства |
| --- | --- | --- |
| Хост | macOS 27.0.0, Darwin arm64 | Не подтверждает Linux/Windows и не соответствует RTX 3050 4 GB из README.md. Объём unified memory не удалось прочитать в sandbox. |
| Go | В checkout указан `go 1.27.2`; установленный `go` — `go1.27.1 darwin/arm64` | Точный toolchain 1.27.2 недоступен из-за записи в cache. Проверки исходников выполнены позднее на 1.27.1 через временный modfile; подробности ниже. |
| llama.cpp runner | `llama-server` и `llama-cli`: версия `0.6.0`, build `11429`, commit `d81235049`, AppleClang 21.0.0.21000334, Darwin arm64 | Это локальный бинарник, а не выбранная и закреплённая версия из M0. Поведение tokenize/completion/очистки слота не проверено. |
| GGUF | В checkout нет файлов `*.gguf` | Артефакт, лицензия конкретного конверта, хеши, размер и воспроизводимость сборки не проверены. |
| Docker | Docker CLI `29.6.2`; активный `desktop-linux` и `default` contexts не подключаются: их Unix sockets отсутствуют | Контейнеры не запущены; Redis/Kafka/wallet-RPC интеграции из этой среды недоступны. |
| Внешние клиенты | Не найдены `redis-server`, `redis-cli`, Kafka CLI и Monero wallet CLI/RPC; не найден SOCKS5-тестовый сервер | Соответствующие интеграционные пробы не выполнялись. Отсутствие локальной команды не доказывает свойства удалённого сервиса. |

## Кандидат модели

Общий план выбирает `Qwen2.5-1.5B-Instruct`, затем требует закрепить точную upstream-ревизию, преобразование в GGUF, license notice, хеши артефакта/tokenizer/template и подписанный тестовый manifest. Официальная карточка модели указывает лицензию Apache-2.0; это подтверждает лицензионную метку кандидата, но не происхождение или условия конкретного GGUF-конверта.

Пока не загружены и не зафиксированы веса, tokenizer, chat template и конвертер, поэтому нет подтверждённых token-ID/BOS parity, контекстной границы, расхода памяти, скорости, отмены или очистки runner slot. Модель не допущена к каталогу.

## Интеграционные пробы B0

| Проба | Результат этого снимка | Нужное доказательство |
| --- | --- | --- |
| Redis Lua: точность выше `2^53`, около `int64`, повтор операции и wrong key type | Не запускалась: Redis и Docker daemon недоступны | Реальные Redis-ответы, включая ошибки, на закреплённой версии; Lua не должен преобразовывать деньги во float. |
| `WAITAOF` и connection-scoped fsync | Не запускалась | Команды на закреплённой версии Redis и подтверждённые counts/timeout cases на выделенном write connection. |
| Kafka outbox повтор и consumer replay | Не запускалась: Kafka и Docker daemon недоступны | Реальные подтверждения broker и повторная доставка с теми же event IDs. |
| Токенизация, шаблон, BOS и слот | Не запускалась: нет GGUF и закреплённой runner-сборки | Сопоставление реальных token IDs/выходного текста, неизвестный шаблон fail-closed, отмена и повторное использование слота после очистки. |
| SOCKS5h удалённый DNS и fail-closed | Не запускалась: нет тестового proxy | Наблюдение DNS/соединений через прокси и подтверждение отсутствия прямого fallback. |
| Wallet prepare/relay ambiguity на stagenet | Не запускалась: wallet CLI/RPC недоступны | Реальные stagenet-ответы prepare/relay при timeout, restart и частичной трансляции; без mainnet. |

Следующее доступное действие для закрытия B0 — получить Go 1.27.2 в разрешённый toolchain cache, поднять изолированный локальный стек Redis/Kafka/wallet mock на работающем Docker engine, затем отдельно закрепить и проверить разрешённый GGUF-конверт. До этих результатов M0/B0 остаётся открытым.

## Проверка кода M1–M3 — 2026-10-09

### Go

| Проверка | Результат | Ограничение |
| --- | --- | --- |
| `go fmt ./...` | Прошла из `server/` | Выполнена установленным `go1.27.1`; `go.mod` требует `1.27.2`. |
| `go test -run '^$' ./...` | Все Go-пакеты скомпилированы | Для обхода недоступного toolchain cache использовались `GOTOOLCHAIN=local`, временный modfile в `/tmp` с директивой 1.27.1 и локальный `GOCACHE`; файлы `go.mod`/`go.sum` проекта для этого не менялись. |
| `go vet ./...` | Прошла | Те же локальный toolchain и временный modfile. |
| `make test-ledger-recovery` | Прошли race-тесты доменной модели, billing reference ledger и request coordinator | Redis integration cases пропускаются, когда не задан `CORDOVA_TEST_REDIS_URL`; тесты не моделируют crash на каждой Redis write/barrier границе. |
| `go test -race -count=1 ./...` | Прошла после разрешения локальных loopback listeners для `httptest` | Тот же локальный toolchain и временный modfile. |
| `make test-ledger-redis` | Не запущена | Требует отдельный локальный Redis с AOF и `CORDOVA_TEST_REDIS_URL`; в этом окружении Redis CLI/server отсутствуют. |

Воспроизводимый Go-вызов с временным modfile в текущей среде (для suite с локальными listeners потребуется разрешённый loopback bind):

```sh
cd server
GOTOOLCHAIN=local GOCACHE=/tmp/cordova-go-build \
  GOFLAGS='-modfile=/tmp/cordova-server-m3.mod -mod=readonly' go test -race -count=1 ./...
```

### Остальные проверки

- `make contracts-check` прошёл: 15 REST paths, 27 node WS types, 8 live WS types и 29 fixtures; Redis-backed handler integration не запускался.
- `cd node && RUSTUP_TOOLCHAIN=stable cargo fmt --all -- --check && RUSTUP_TOOLCHAIN=stable cargo test --workspace --offline` прошёл: 14 Rust tests. Использован установленный `stable`, а не зафиксированный проектом Rust 1.96.0.
- `make test-ledger-redis`, Redis Lua runtime, `WAITAOF`, Kafka replay, tokenizer parity, SOCKS5h и wallet stagenet проверки не подтверждены.
- Docker daemon недоступен из-за отсутствующего `/Users/abbas/.docker/run/docker.sock`; изолированный стек не запускался.
