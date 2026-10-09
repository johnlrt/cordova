# M3 — учёт запросов и восстановление

Дата снимка: 2026-10-09. Этот этап реализует основу C5 из `PLAN.md` и B7 из `PLAN-back.md`; наличие кода не означает прохождение внешних M0/M2 проверок или завершение M3.

**Статус:** accounting domain, Redis adapter и interface-based coordinator реализованы; M3 acceptance пока не пройден.

## Реализовано

- Эталонная Go-модель ledger считает резерв, фактическую стоимость, долю владельца и возврат целочисленно, хранит сбалансированные append-only проводки и сверяет hash chain, денежные остатки и дневные лимиты.
- Lifecycle поддерживает резерв до отправки, полный refund неудачной попытки, один retry на другого владельца, settlement и отмену с проверенным префиксом вывода. Координатор закрывает retry-окно, если второй владелец недоступен. Повтор операций идемпотентен; гонки terminal операций разрешаются через compare-and-set.
- Redis-адаптер хранит денежные суммы десятичными строками. Один Lua вызов проверяет типы ключей, состояние, ревизию, проводки и spend cap до первой записи, затем вместе обновляет ledger, балансы, tombstone, outbox, request metadata и cap counters. Lua не преобразует денежные значения через `tonumber`.
- Записи Redis выполняются на выделенном соединении с последующим `WAITAOF`; при неопределённой устойчивости ledger открывает circuit. `Recover` сначала проверяет journal replay, возвращает orphan reservations, закрывает незавершённое retry-окно, сверяет cap counters и только потом открывает readiness.
- `internal/request` содержит coordinator, который считает input до резерва, резервирует до dispatch, повторяет один сбой до первого SSE chunk только на другом owner, считает полный накопленный текст для output и подтверждает settlement до успеха. На отмене списывается только префикс, flush которого подтвердил writer. Реальные HTTP handlers пока не используют coordinator.
- Gateway запускает ledger recovery до открытия listener и держит readiness закрытой, пока ledger не готов. Добавлены owner/key-scoped `GET /api/requests/{id}` и session-only `GET /api/billing/balance`; ответ статуса скрывает неизвестную сумму как `null` во время recovery. `/api/usage` в M1 показывает синтетические записи с нулевой стоимостью.
- Добавлена Redis integration suite для точности выше `2^53`, повторов, retry, orphan recovery, сохранения settlement и отказа до мутации при неверном Redis key type. `make test-ledger-redis` запускает её на отдельном локальном Redis с AOF.
- Reference tests cover concurrent cap reservation, competing terminal operations, crash snapshots before and after orphan recovery, abandoned retry windows, preserved settlement, and verified-prefix cancellation. They do not inject a process/connection crash at every Redis write/barrier boundary; that remains part of the live Redis gate.

## Что ещё не закрыто

- Chat endpoint пока не вызывает RedisLedger и не создает request metadata из C2: у mock provider только синтетические word counts, поэтому их нельзя использовать для списаний. Координатор принимает интерфейсы для trusted tokenizer/router/ledger, но production adapters и DI wiring в chat path отсутствуют. Нужны C9 tokenizer, C3 lease-safe router/session fencing, key cap и подписанная цена каталога.
- REST handlers статуса/баланса отражены в OpenAPI и fixtures, но сквозной тест через HTTP auth и Redis-backed ledger ещё не выполнен.
- В репозитории нет работающего Redis здесь, и `CORDOVA_TEST_REDIS_URL` не задан. Lua и `WAITAOF` integration suite собрана, но не выполнена на реальном Redis; crash injection на каждой финансовой границе, репликационный barrier и ошибочные runtime key types не подтверждены live-ответами.
- C3 session fencing и lease cleanup для реальных node sessions отсутствуют. Redis integration suite содержит проверку request metadata и ledger при restart, но в этой среде она не запущена и в любом случае не докажет восстановление подключённых реальных нод.
- Не подключены отмена активного запроса при отзыве API key, fencing позднего completion и shutdown coordination с активными leases. Reference tests подтверждают coordinator-level retry/cancel правила, но не эти runtime races.
- `PLAN.md` ставит финальный M3 gate после M2. Реальный tokenizer/model parity, routing priorities и cross-platform node transport не прошли свои M0/M2 критерии; текущий mock не заменяет эти доказательства.

## Проверки

См. актуальные команды и ограничения среды в [verification.md](verification.md). `make test-ledger-recovery` запускает race-тесты domain/reference ledger/coordinator. Для Redis gate используйте отдельный Redis с включённым AOF и `CORDOVA_TEST_REDIS_URL=redis://127.0.0.1:<port>/<db>`; тест использует случайный key prefix и не очищает database. Цели Makefile теперь определены; Redis цель завершается ошибкой, если URL не задан.
