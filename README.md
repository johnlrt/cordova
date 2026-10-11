# Cordova — локальный пилот

Запускаемый Go API для будущей сети AI-нод. **Реализован M1: HTTPS edge → Go gateway → приватная WSS mock-нода → SSE, с аккаунтами и usage-метаданными в Redis/Kafka и account dashboard.** Настоящая AI-модель пока не подключена; генерация и usage остаются синтетическими.

Текущий пилот: сервер друга с Ultra 5 125 и 64 ГБ RAM будет выполнять и распределение, и локальную генерацию. Ноутбук пользователя с i7 — клиент. Доступа к MacBook нет. Один канал означает одну одновременно выполняемую генерацию; текущий шлюз уже ограничен одним запросом. Подробнее: [схема размещения](docs/deployment-pilot.md).

Работают:

- `GET /healthz` — проверка процесса.
- `GET /v1/models` — каталог за API-ключом.
- `POST /v1/chat/completions` — обычный JSON или поток SSE в формате text Chat Completions.
- Валидация, ограничения размера и конкурентности, отмена при отключении клиента, безопасные ошибки.
- Скрипты запуска, тесты, GitHub CI и понятная точка подключения будущего исполнителя.
- Тёмный адаптивный интерфейс: обзор, конфигурация ноды, каталог и документация, с переключателем English/Russian в верхней панели. Выбор языка сохраняется в браузере. Проверка здоровья и каталог получают реальные ответы mock-шлюза.
- Экраны входа, регистрации, восстановления и account dashboard подключены к аккаунтам, API keys, usage и live-обновлениям Go gateway.

Ответы генерации помечены `X-Cordova-Mode: mock`; текст начинается с `[MOCK]`. Поле `usage` содержит **синтетические количества слов**, а не токены модели. Денежных списаний и выплат нет. M1 не подключает Rust-ноду к реальному inference.

## Запуск M1 локально

Требуются Docker Compose v2, OpenSSL, Python 3 и `make`:

```sh
make bootstrap-dev
make config-check
make up
make smoke
make network-check
```

Откройте [https://localhost:8443](https://localhost:8443). Перед входом добавьте `.local/dev-ca.crt` в доверенные сертификаты браузера или ОС. Секреты и сертификаты остаются в `.local/`, которая исключена из Git и Docker build context. В dashboard можно зарегистрировать аккаунт, сохранить recovery-коды и создать тестовый API key; secret ключа показывается один раз.

`make down` останавливает контейнеры, сохраняя локальные Redis/Kafka volumes. Наружу опубликован только `127.0.0.1:8443`; Redis, Kafka, gateway и mock-нода остаются в приватных Compose-сетях. Никакие wallet или Docker socket не монтируются.

Для локальных проверок M1 используйте `make contracts-check`, `make test-protocol`, `make smoke`, `make network-check` и `make test-edge`. Подробные границы интеграции перечислены в [M1 implementation notes](docs/stage-2.md).

## Windows: standalone HTTP/SSE fallback

Требуется Go **1.27.2** либо готовый Windows bundle из Releases. В PowerShell из корня проекта:

```powershell
./scripts/start.ps1
```

Скрипт соберёт сервер, если готового бинарника нет, и создаст случайный ключ в `.local/api-key.txt`. Уже созданный ключ сохраняется. Если политика PowerShell запрещает скрипт, используйте прямые команды из [инструкции запуска](docs/stage-1.md).

После обновления исходников остановите старый сервер и используйте `./scripts/start.ps1 -Rebuild` с установленным Go: обычный запуск может использовать ранее собранный бинарник. Первый Windows bundle также содержит старый лимит из первого этапа; новые исходники требуют пересборки.

Во втором PowerShell:

```powershell
./scripts/smoke.ps1
```

Ожидается `PASS: health, authenticated model catalog and complete mock SSE response.`

Для просмотра частей ответа по мере поступления:

```powershell
$demoKey = (Get-Content -LiteralPath '.local/api-key.txt' -Raw).Trim()
$payload = '{"model":"cordova-chat-small-v1","messages":[{"role":"user","content":"Hello"}],"stream":true,"stream_options":{"include_usage":true}}'
$payload | curl.exe -N http://127.0.0.1:8080/v1/chat/completions -H "Authorization: Bearer $demoKey" -H 'Content-Type: application/json' --data-binary '@-'
```

## Linux/macOS

С установленным Go 1.27.2:

```sh
sh scripts/start.sh
```

Адрес по умолчанию — `http://127.0.0.1:8080`. После сборки фронтенда скрипт автоматически подключает интерфейс на `/`. Без сборки доступен только API. Публичное HTTPS-демо появится в отдельном этапе.

## Интерфейс на Windows

Требуются Node.js **24.x** и Go 1.27.2. Остановите старый сервер, затем из корня проекта:

```powershell
Set-Location frontend
npm.cmd ci
npm.cmd run build
Set-Location ..
./scripts/start.ps1 -Rebuild
```

Откройте `http://127.0.0.1:8080/`. Кнопка «Подключить шлюз» принимает существующий ключ из `.local/api-key.txt` для загрузки каталога; ключ хранится только в памяти до отключения или перезагрузки страницы. Полная инструкция, dev-режим и ограничения: [frontend](docs/frontend-pilot.md).

## Rust-нода и следующий этап

В [инструкции Rust-ноды](docs/node-setup.md) описаны CLI и настройка llama.cpp. По умолчанию gateway использует синтетическую WSS mock-ноду. Опциональный C3-путь уже включает enrollment, Rust WebSocket transport, подписанный каталог, tokenizer helper и session-fenced routing/model preparation. Для него нужны `CORDOVA_INFERENCE_MODE=c3`, узлы с одобренными C3 credentials и настроенный подписанный каталог/tokenizer. Настоящая модель пока не допущена: отсутствуют квалифицированные GGUF и подтверждённый helper/node token parity, а Linux/Windows/macOS hardware и M2 release gates не пройдены. Текущие границы описаны в [M1 implementation notes](docs/stage-2.md) и [verification log](docs/verification.md).

## Другу, который продолжит работу

1. Получить доступ к приватному репозиторию и клонировать его.
2. Прочитать [M1 implementation notes](docs/stage-2.md), [схему пилота](docs/deployment-pilot.md), затем исходный `PLAN.md`.
3. Выполнить `cd server`, `go test ./...`, `go vet ./...`.
4. Делать изменения в своей ветке и присылать Pull Request.

Следующий этап — пройти реальные model/tokenizer parity, Redis-backed C3 routing, reconnect-fencing и supported-OS smoke gates до включения настоящего inference. Пилот по-прежнему ограничен одним одновременным запросом. Intel graphics/NPU на Ultra пока не проверены и не считаются используемым ускорителем.

Полный замысел: [общая архитектура](PLAN.md), [backend/Rust](PLAN-back.md), [frontend](PLAN-front.md), [инфраструктура](PLAN-infra.md). Эти планы описывают дальнейшую работу, а не уже готовые возможности.
