<script lang="ts">
  import { onMount } from 'svelte';
  import { language } from '$lib/state/language.svelte.ts';
  import { resources } from '$lib/state/resources.svelte.ts';
  import { session } from '$lib/state/session.svelte.ts';
  import { LiveConnection, type LiveStatus } from '$lib/live/connection.ts';

  let liveStatus = $state<LiveStatus>('disconnected');
  let active = true;
  let live: LiveConnection | undefined;
  const ru = $derived(language.value === 'ru');
  const liveLabel = $derived(ru
    ? ({ connecting: 'Подключаем live-канал', connected: 'Live-канал подключён', disconnected: 'Нет live-соединения', hidden: 'Пауза: вкладка скрыта' }[liveStatus])
    : ({ connecting: 'Connecting live updates', connected: 'Live updates connected', disconnected: 'Live updates disconnected', hidden: 'Paused while tab is hidden' }[liveStatus]));

  onMount(() => {
    language.load();
    active = true;
    const unregisterCleanup = session.addLiveCleanup(() => { live?.stop(); resources.clear(); });
    void bootstrap();
    return () => {
      active = false;
      unregisterCleanup();
      live?.stop();
      resources.clear();
    };
  });

  async function bootstrap() {
    const current = await session.ensureLoaded();
    if (!active || !current) return;
    resources.clear();
    void resources.refreshUsage();
    const generation = session.generation;
    live = new LiveConnection(current, generation, {
      onStatus: (status) => { if (active) liveStatus = status; },
      onInvalidate: (event) => {
        if (!active) return;
        resources.markStale(event.topic);
        if (event.topic === 'usage') void resources.refreshUsage();
      },
      onResync: () => {
        if (!active) return;
        resources.clear();
        void resources.refreshUsage();
      },
      onPoll: () => { resources.markStale('usage'); void resources.refreshUsage(); }
    }, () => session.generation === generation && session.session?.account.id === current.account.id,
    () => session.expireIfCurrent(generation));
    live.start();
  }

  function time(value: string) {
    return new Intl.DateTimeFormat(ru ? 'ru-RU' : 'en', {
      year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', timeZoneName: 'short'
    }).format(new Date(value));
  }

  function statusLabel(status: string) {
    if (!ru) return status;
    return ({ accepted: 'принят', running: 'выполняется', settled: 'завершён', refunded: 'возврат', cancelled: 'отменён', recovering: 'сверка' } as Record<string, string>)[status] ?? status;
  }

  function usageErrorText() {
    const error = resources.usageError;
    if (!error) return ru ? 'Не удалось загрузить историю' : 'Could not load request history';
    if (error.status === 429) return ru
      ? `Слишком много запросов. Повторите позже${error.retryAfter !== null ? ` (через ${error.retryAfter} с)` : ''}.`
      : `Too many requests. Retry later${error.retryAfter !== null ? ` (in ${error.retryAfter}s)` : ''}.`;
    if (error.code === 'storage_unavailable') return ru ? 'Хранилище временно недоступно.' : 'Storage is temporarily unavailable.';
    if (error.code === 'wallet_unavailable') return ru ? 'Кошелёк временно недоступен.' : 'The wallet service is temporarily unavailable.';
    if (error.code === 'no_capacity' || error.code === 'model_warming') return ru ? 'Исполнитель прогревается или сейчас нет свободной мощности.' : 'The executor is warming or has no available capacity.';
    if (error.status >= 500) return ru ? 'Сервис временно недоступен. Попробуйте ещё раз.' : 'The service is temporarily unavailable. Try again.';
    return ru ? 'Проверьте соединение и повторите запрос.' : 'Check the connection and retry the request.';
  }
</script>

<svelte:head><title>{ru ? 'Обзор · Cordova' : 'Overview · Cordova'}</title></svelte:head>
<section class="account-content">
  <p class="eyebrow">{ru ? 'АККАУНТ' : 'ACCOUNT'}</p>
  <h1>{ru ? 'Добро пожаловать в Cordova' : 'Welcome to Cordova'}</h1>
  <p class="account-intro">{ru ? 'Здесь отображаются только данные, подтверждённые аккаунтным API.' : 'This dashboard displays only data confirmed by the account API.'}</p>
  <aside class="account-status" aria-live="polite"><span class="status-dot" class:online={liveStatus === 'connected'}></span><div><strong>{liveLabel}</strong><p>{ru ? 'События только помечают данные как устаревшие; подтверждённое состояние загружается через REST.' : 'Live events only mark snapshots stale; confirmed account state is fetched through REST.'}</p></div></aside>
  <section class="account-panel usage-panel" aria-labelledby="usage-heading">
    <div class="account-panel-heading"><div><p class="eyebrow">{ru ? 'ИСТОРИЯ ЗАПРОСОВ' : 'REQUEST HISTORY'}</p><h2 id="usage-heading">{ru ? 'Последние запросы' : 'Recent requests'}</h2></div>{#if resources.stale}<span class="badge amber">{ru ? 'Обновляем' : 'Refreshing'}</span>{/if}</div>
    {#if resources.usageStatus === 'loading'}
      <p class="usage-message" role="status">{ru ? 'Загружаем подтверждённую историю…' : 'Loading authoritative request history…'}</p>
    {:else if resources.usageStatus === 'error' && !resources.usage}
      <div class="usage-empty" role="status"><strong>{resources.usageError?.status === 404 ? (ru ? 'История пока не подключена на сервере' : 'Usage API is not available on this server yet') : usageErrorText()}</strong><p>{resources.usageError?.requestId ? `${ru ? 'ID запроса поддержки' : 'Support request ID'}: ${resources.usageError.requestId}` : (ru ? 'Фронтенд не подставляет тестовые данные. Повторите запрос, когда endpoint будет доступен.' : 'The frontend does not substitute demo data here. Retry when the endpoint is available.')}</p><button class="secondary" onclick={() => void resources.refreshUsage()}>{ru ? 'Повторить' : 'Retry'}</button></div>
    {:else if resources.usage?.data.length}
      <div class="usage-table-wrap"><table class="usage-table"><thead><tr><th>{ru ? 'Модель' : 'Model'}</th><th>{ru ? 'Состояние' : 'Status'}</th><th>{ru ? 'Токены' : 'Tokens'}</th><th>{ru ? 'Время · локальная зона' : 'Time · local zone'}</th></tr></thead><tbody>
        {#each resources.usage.data as row (row.request_id)}<tr><td><strong>{row.model_id}</strong><small>#{row.request_id.slice(0, 8)}</small></td><td><span class="badge neutral">{statusLabel(row.status)}</span></td><td>{row.input_tokens.toLocaleString(ru ? 'ru-RU' : 'en')} in / {row.output_tokens.toLocaleString(ru ? 'ru-RU' : 'en')} out</td><td>{time(row.created_at)}</td></tr>{/each}
      </tbody></table></div>
    {:else}
      <div class="usage-empty"><strong>{ru ? 'Запросов пока нет' : 'No requests yet'}</strong><p>{ru ? 'Когда появится usage API, здесь отобразятся статусы и метаданные. Тексты промптов не загружаются.' : 'When the usage API is available, request status and metadata will appear here. Prompt text is never loaded.'}</p></div>
    {/if}
  </section>
  <div class="account-grid"><article class="account-panel"><p class="eyebrow">{ru ? 'ПИЛОТ' : 'PILOT'}</p><h2>{ru ? 'Один хост. Один канал генерации.' : 'One host. One generation channel.'}</h2><p>{ru ? 'Пилот показывает mock-состояние шлюза и синтетический каталог. Он не заявляет о настоящем inference или измеренной мощности ускорителя.' : 'The pilot reports mock gateway health and a synthetic model catalog. It does not claim real inference or measured accelerator capacity.'}</p><a class="text-link" href="/pilot">{ru ? 'Открыть пилот ↗' : 'View pilot dashboard ↗'}</a></article><article class="account-panel"><p class="eyebrow">{ru ? 'ПРИВАТНОСТЬ' : 'PRIVACY'}</p><h2>{ru ? 'Узлы видят текст запроса' : 'Nodes can see request text'}</h2><p>{ru ? 'Не отправляйте секреты или данные, которые нельзя раскрыть обслуживающему узлу. Центральный сервер и доверенный tokenizer также обрабатывают текст в памяти.' : 'Do not submit secrets or data you cannot disclose to the serving node. The central server and trusted tokenizer also process text in memory.'}</p><a class="text-link" href="/">{ru ? 'Полное предупреждение ↗' : 'Read the full notice ↗'}</a></article></div>
</section>
