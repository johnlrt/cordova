export type Language = 'en' | 'ru';

export const messages = {
  en: {
    shell: {
      pages: { overview: 'Overview', nodes: 'Node', models: 'Models', docs: 'Documentation' },
      descriptions: {
        overview: 'Your network at a glance', nodes: 'Single-host configuration',
        models: 'Your gateway catalog', docs: 'Setup and API capabilities'
      },
      skip: 'Skip to content', brand: 'Cordova — overview', workspace: 'Local pilot', oneWorkspace: 'One workspace',
      workArea: 'WORKSPACE', next: 'COMING LATER', keys: 'API key management', payments: 'Payments', later: 'Later',
      oneHost: 'One host, one channel', hostRam: 'Ultra · 64 GB RAM', i7Client: 'i7 connects as a client',
      pilot: 'Cordova Pilot', stage: 'Mock API · M1', breadcrumb: 'Workspace', dashboard: 'NETWORK DASHBOARD',
      gateway: { online: 'Gateway online', checking: 'Checking gateway', offline: 'No connection' },
      refresh: 'Refresh gateway status', disconnect: 'Disconnect key', connect: 'Connect gateway',
      mockTitle: 'Mock pilot.', mockText: 'Gateway connectivity and synthetic mock-node responses are active; real AI inference and payments are not connected yet.',
      how: 'How it works', offline: 'The gateway is unavailable or returned an unsupported response. Start the server and check the API settings. Host specifications shown here were provided by the team.',
      footer: 'Local capacity. Transparent status.', docsLink: 'Documentation', language: 'Language',
      languageEnglish: 'English', languageRussian: 'Russian'
    },
    overview: {
      heroEyebrow: 'YOUR LOCAL NETWORK', headlineOne: 'Power close by.', headlineTwo: 'Everything in view.',
      intro: 'One host manages requests and serves synthetic responses through a mock node. Your laptop connects as a regular client; real model inference remains a later step.',
      seeNode: 'View node', channel: 'GENERATION CHANNEL', gateway: 'Gateway connection', online: 'Online', checking: 'Checking…', offline: 'Offline',
      confirmed: 'Confirmed by API', lastCheck: 'Last connection check', gatewayLatency: 'Gateway latency',
      httpNotModel: 'HTTP latency, not model speed', channels: 'Generation channels', configuredCapacity: 'Configured pilot capacity',
      hostMemory: 'Host memory', ownerReported: 'Reported by node owner', yourNode: 'Your node', oneExecutor: 'One executor for the local pilot',
      details: 'Details', futureNode: 'Gateway and planned AI node', modelMissing: 'No model connected',
      mockNote: 'The API is in mock mode. The node and accelerators have not been tested.', latencyChart: 'Gateway latency',
      liveChecks: 'Live checks in this session', collecting: 'Collecting measurements', chartAfterTwo: 'The chart appears after two successful checks',
      chartAlt: 'Gateway latency chart; exact values are listed below', measurements: 'Measurements', time: 'Time', result: 'Result',
      noConnection: 'No connection', projectPath: 'Path to real AI', projectStatus: 'Current project status',
      stage1: 'M1 / mock node + interface', gatewayAndUi: 'Gateway and interface', apiCatalogHealth: 'API, catalog and connection check',
      modelOnUltra: 'Model on Ultra', measureCapacity: 'Run it and measure capacity', fullRequest: 'Complete request path',
      nodeChannelClient: 'Node, channel and i7 client'
    },
    nodes: {
      eyebrow: 'PILOT INFRASTRUCTURE', heading: 'One host. One channel.',
      intro: 'Request routing and planned generation run on truekant’s computer.', teamConfig: 'Team-provided configuration',
      mainHost: 'Cordova main host', gatewayOnline: 'Gateway online', gatewayUnconfirmed: 'Gateway not confirmed',
      processor: 'Processor', memory: 'Memory', graphics: 'Graphics / accelerator', dataSource: 'Data source', owner: 'Host owner',
      gateway: 'Gateway', healthOk: 'Health API is responding.', waitHealth: 'Waiting for a successful API check.',
      model: 'Model', modelUnconnected: 'Not connected yet. Intel GPU/NPU performance has not been measured.',
      channel: 'Channel 01', singleLimit: 'Limit: one request at a time. Live channel occupancy is not reported by the API.',
      requestFlow: 'How a request flows', client: 'i7 client', clientInfo: 'Sends a request and receives the response. The model does not run locally.',
      goGateway: 'Go gateway on Ultra', gatewayInfo: 'Checks the key and admits one request to the executor.',
      localNode: 'Local node and runner', nextGeneration: 'Next step: generation on the same host, with cancellation and slot release.',
      measure: 'What to measure', measureIntro: 'The pilot is fixed at one channel. Choose a model based on measurements from the host.',
      memoryRun: 'Memory use during load and generation', speed: 'Time to first response and generation speed',
      accelerator: 'Support for the chosen accelerator', cancel: 'Request cancellation and slot reuse',
      ramNote: '64 GB RAM is a host specification, not a benchmark result.'
    },
    models: {
      eyebrow: 'API CATALOG', heading: 'Models on your gateway', intro: 'The catalog loads from the server after the API key is checked.',
      refresh: 'Refresh catalog', loading: 'Loading catalog', checking: 'Checking the key and gateway response.',
      connectionFailed: 'Could not connect', checkKey: 'Check key', protected: 'PROTECTED CATALOG', connectKey: 'Connect an API key',
      keyInfo: 'The gateway will verify the key and return available models.',
      keyMemory: 'The key stays in this tab’s memory until you disconnect or reload the page.',
      connectGateway: 'Connect gateway', emptyTitle: 'The catalog is empty', emptyInfo: 'The gateway accepted the key, but has not published any models yet.',
      apiModel: 'MODEL FROM API', demo: 'A demo executor for connection checks. No real AI model has been loaded.',
      context: 'Catalog context', maxOutput: 'Maximum response length', apiStatus: 'API entry status', ready: 'Ready',
      synthetic: 'Mock response counts are synthetic; no charges are made.'
    },
    docs: {
      eyebrow: 'QUICK START', heading: 'From gateway to your app', intro: 'What this pilot can do today and what comes next.',
      checkConnection: 'Check the connection', runGateway: 'Start the gateway on the server. The status above shows the result of',
      localAddress: 'If you open the interface on the i7, its server should route API requests to the Ultra. The address',
      belongsToComputer: 'points to the computer where that server is running.', connectKey: 'Connect an API key',
      obtainKey: 'The host owner can find the key in', enterDialog: 'Enter it in the connection dialog. The key is not saved in browser storage.',
      openConnection: 'Open connection', getCatalog: 'Get the model catalog', powershell: 'PowerShell example to run on the server',
      copied: 'Copied', copy: 'Copy', copyError: 'Could not copy. Select and copy the command manually.',
      supported: 'Supported today', currentApi: 'Current API', health: 'Gateway status, no key required',
      catalog: 'Catalog, requires a Bearer key', chat: 'Text messages, JSON or SSE; mock responses',
      busy: 'When the single channel is busy, a chat request returns 429. There is no queue, real model generation or payment flow. M1 account and usage metadata are available in the dashboard.',
      privacy: 'Privacy for future requests',
      privacyInfo: 'When a real node is connected, the host running generation can access the request text. truekant owns this pilot. Do not send data you are unwilling to share with the host.'
    },
    dialog: {
      close: 'Close connection dialog', title: 'Connect to the gateway',
      intro: 'Enter the API key provided by the Ultra host owner. It is used to load the model catalog.',
      key: 'API key', invalidKey: 'The key must contain between 32 and 4096 characters.', hide: 'Hide', show: 'Show',
      memory: 'The key stays in this tab’s memory. Disconnecting or reloading the page clears it.',
      cancel: 'Cancel', connecting: 'Connecting…', connect: 'Connect'
    },
    errors: {
      unauthorized: 'The gateway rejected the key. Check it and try connecting again.',
      busy: 'The channel is busy. Try your request again shortly.',
      invalid: 'The gateway response does not match this pilot’s API.',
      unavailable: 'Could not reach the gateway. Make sure the server is running and the API address is correct.'
    }
  },
  ru: {
    shell: {
      pages: { overview: 'Обзор', nodes: 'Нода', models: 'Модели', docs: 'Документация' },
      descriptions: {
        overview: 'Ваша сеть в одном месте', nodes: 'Конфигурация единственного хоста',
        models: 'Каталог вашего шлюза', docs: 'Подключение и возможности API'
      },
      skip: 'Перейти к содержимому', brand: 'Cordova — обзор', workspace: 'Локальный пилот', oneWorkspace: 'Одно пространство',
      workArea: 'РАБОЧЕЕ ПРОСТРАНСТВО', next: 'СЛЕДУЮЩИЕ ЭТАПЫ', keys: 'Управление ключами', payments: 'Платежи', later: 'Позже',
      oneHost: 'Один хост, один канал', hostRam: 'Ultra · 64 ГБ RAM', i7Client: 'i7 подключается как клиент',
      pilot: 'Пилот Cordova', stage: 'Mock API · M1', breadcrumb: 'Рабочее пространство', dashboard: 'ПАНЕЛЬ СЕТИ',
      gateway: { online: 'Шлюз на связи', checking: 'Проверяем шлюз', offline: 'Нет связи' },
      refresh: 'Обновить состояние шлюза', disconnect: 'Отключить ключ', connect: 'Подключить шлюз',
      mockTitle: 'Пилот в mock-режиме.', mockText: 'Шлюз и синтетические ответы mock-ноды работают; настоящие AI-инференс и платежи пока не подключены.',
      how: 'Как это работает', offline: 'Шлюз недоступен или вернул неподдерживаемый ответ. Запустите сервер и проверьте настройку API. Показанные характеристики хоста — конфигурация команды.',
      footer: 'Локальная мощность. Прозрачный статус.', docsLink: 'Документация', language: 'Язык',
      languageEnglish: 'Английский', languageRussian: 'Русский'
    },
    overview: {
      heroEyebrow: 'ВАША ЛОКАЛЬНАЯ СЕТЬ', headlineOne: 'Мощность рядом.', headlineTwo: 'Всё под контролем.',
      intro: 'Один хост управляет запросами и выдаёт синтетические ответы через mock-ноду. Ноутбук подключается как обычный клиент; настоящая генерация модели появится на следующем этапе.',
      seeNode: 'Посмотреть ноду', channel: 'КАНАЛ ГЕНЕРАЦИИ', gateway: 'Связь со шлюзом', online: 'На связи', checking: 'Проверка…', offline: 'Нет связи',
      confirmed: 'Подтверждено через API', lastCheck: 'Последняя проверка соединения', gatewayLatency: 'Отклик шлюза',
      httpNotModel: 'HTTP-задержка, не скорость модели', channels: 'Каналы генерации', configuredCapacity: 'Настроенная ёмкость пилота',
      hostMemory: 'Память хоста', ownerReported: 'Заявлено владельцем ноды', yourNode: 'Ваша нода', oneExecutor: 'Один исполнитель для локального пилота',
      details: 'Подробнее', futureNode: 'Шлюз и будущая AI-нода', modelMissing: 'Модель не подключена',
      mockNote: 'API работает в mock-режиме. Нода и ускорители ещё не проверены.', latencyChart: 'Задержка шлюза',
      liveChecks: 'Живые проверки этой сессии', collecting: 'Собираем измерения', chartAfterTwo: 'График появится после двух успешных проверок',
      chartAlt: 'График задержки шлюза; численные значения доступны ниже', measurements: 'Измерения', time: 'Время', result: 'Результат',
      noConnection: 'Нет связи', projectPath: 'Путь к настоящему AI', projectStatus: 'Текущее состояние проекта',
      stage1: 'M1 / mock-нода + интерфейс', gatewayAndUi: 'Шлюз и интерфейс', apiCatalogHealth: 'API, каталог и проверка связи',
      modelOnUltra: 'Модель на Ultra', measureCapacity: 'Запуск и измерение мощности', fullRequest: 'Полный путь запроса',
      nodeChannelClient: 'Нода, канал и клиент i7'
    },
    nodes: {
      eyebrow: 'ИНФРАСТРУКТУРА ПИЛОТА', heading: 'Один хост. Один канал.',
      intro: 'Распределение запросов и будущая генерация — на компьютере truekant.', teamConfig: 'Конфигурация команды',
      mainHost: 'Основной хост Cordova', gatewayOnline: 'Шлюз на связи', gatewayUnconfirmed: 'Шлюз не подтверждён',
      processor: 'Процессор', memory: 'Оперативная память', graphics: 'Графика / ускоритель', dataSource: 'Источник данных', owner: 'Владелец хоста',
      gateway: 'Шлюз', healthOk: 'Health API успешно отвечает.', waitHealth: 'Ожидаем успешную проверку API.',
      model: 'Модель', modelUnconnected: 'Пока не подключена. Работа Intel GPU/NPU ещё не измерена.',
      channel: 'Канал 01', singleLimit: 'Лимит: один запрос за раз. Живая занятость пока не передаётся API.',
      requestFlow: 'Как проходит запрос', client: 'Клиент на i7', clientInfo: 'Отправляет запрос и получает ответ. Модель локально не запускается.',
      goGateway: 'Go-шлюз на Ultra', gatewayInfo: 'Проверяет ключ и допускает один запрос к исполнителю.',
      localNode: 'Локальная нода и runner', nextGeneration: 'Следующий этап: генерация на том же хосте с отменой и освобождением слота.',
      measure: 'Что нужно измерить', measureIntro: 'В пилоте зафиксирован один канал. Модель выберем по реальным измерениям хоста.',
      memoryRun: 'Память при загрузке и генерации', speed: 'Время до первого ответа и скорость',
      accelerator: 'Поддержка выбранного ускорителя', cancel: 'Отмена запроса и повторное использование слота',
      ramNote: '64 ГБ RAM — характеристика хоста, а не результат бенчмарка.'
    },
    models: {
      eyebrow: 'КАТАЛОГ API', heading: 'Модели вашего шлюза', intro: 'Список загружается с сервера после проверки API-ключа.',
      refresh: 'Обновить каталог', loading: 'Загружаем каталог', checking: 'Проверяем ключ и ответ шлюза.',
      connectionFailed: 'Не удалось подключиться', checkKey: 'Проверить ключ', protected: 'ЗАЩИЩЁННЫЙ КАТАЛОГ', connectKey: 'Подключите API-ключ',
      keyInfo: 'Шлюз проверит ключ и вернёт доступные модели.',
      keyMemory: 'Ключ остаётся в памяти этой вкладки до отключения или перезагрузки.',
      connectGateway: 'Подключить шлюз', emptyTitle: 'Каталог пока пуст', emptyInfo: 'Шлюз принял ключ, но ещё не опубликовал ни одной модели.',
      apiModel: 'МОДЕЛЬ ИЗ API', demo: 'Демонстрационный исполнитель для проверки подключения. Настоящая AI-модель ещё не загружена.',
      context: 'Контекст каталога', maxOutput: 'Макс. длина ответа', apiStatus: 'Статус записи API', ready: 'Готова',
      synthetic: 'Счётчики mock-ответов синтетические; списания не выполняются.'
    },
    docs: {
      eyebrow: 'БЫСТРЫЙ СТАРТ', heading: 'От шлюза к вашему приложению', intro: 'Возможности текущего пилота и следующий шаг.',
      checkConnection: 'Проверьте соединение', runGateway: 'Запустите шлюз на сервере. Индикатор выше показывает результат запроса',
      localAddress: 'Если интерфейс открыт на i7, его сервер должен направлять API-запросы к Ultra. Адрес',
      belongsToComputer: 'относится к компьютеру, на котором работает соответствующий сервер.', connectKey: 'Подключите API-ключ',
      obtainKey: 'Владелец хоста может взять ключ из', enterDialog: 'Введите его в окне подключения. Ключ не сохраняется в браузерном хранилище.',
      openConnection: 'Открыть подключение', getCatalog: 'Получить каталог моделей', powershell: 'Пример команды PowerShell на сервере',
      copied: 'Скопировано', copy: 'Копировать', copyError: 'Не удалось скопировать. Выделите команду вручную.',
      supported: 'Что поддерживается', currentApi: 'Текущий API', health: 'Состояние шлюза без ключа',
      catalog: 'Каталог с Bearer-ключом', chat: 'Текстовые сообщения, JSON или SSE; mock-ответы',
      busy: 'Когда единственный канал занят, chat-запрос получает 429. Очереди, настоящей генерации модели и платежей нет. Аккаунт и usage-метаданные M1 доступны в панели.',
      privacy: 'Приватность будущих запросов',
      privacyInfo: 'После подключения настоящей ноды текст запросов будет доступен хосту, выполняющему генерацию. Владелец этого пилота — truekant. Не передавайте данные, которые не готовы раскрыть владельцу хоста.'
    },
    dialog: {
      close: 'Закрыть окно подключения', title: 'Подключение к шлюзу',
      intro: 'Введите API-ключ, выданный владельцем хоста Ultra. Он нужен для загрузки каталога моделей.',
      key: 'API-ключ', invalidKey: 'Ключ должен содержать от 32 до 4096 символов.', hide: 'Скрыть', show: 'Показать',
      memory: 'Ключ хранится только в памяти вкладки. Перезагрузка или отключение удалит его.',
      cancel: 'Отмена', connecting: 'Подключаем…', connect: 'Подключить'
    },
    errors: {
      unauthorized: 'Шлюз не принял ключ. Проверь его и подключись снова.',
      busy: 'Канал занят. Повтори запрос немного позже.',
      invalid: 'Ответ шлюза не соответствует API этого пилота.',
      unavailable: 'Не удалось связаться со шлюзом. Проверь, что сервер запущен и адрес настроен правильно.'
    }
  }
} as const;
