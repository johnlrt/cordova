<script lang="ts">
  import { onMount } from 'svelte';
  import { ApiFailure } from '$lib/api/client.ts';
  import { fetchNodes, linkNode } from '$lib/api/account.ts';
  import type { Node } from '$lib/api/generated/contracts.ts';
  import { language } from '$lib/state/language.svelte.ts';
  import { session } from '$lib/state/session.svelte.ts';

  let active = false;
  let generation = -1;
  let request: AbortController | undefined;
  let listRequest: AbortController | undefined;
  let removeSessionCleanup = () => {};
  let nodes = $state<Node[]>([]);
  let nextCursor = $state<string | null>(null);
  let loadingMore = $state(false);
  let userCode = $state('');
  let nodeName = $state('');
  let confirmedDevice = $state(false);
  let processing = $state(false);
  let loadingNodes = $state(true);
  let errorMessage = $state('');
  let listError = $state('');
  let success = $state(false);
  let search = $state('');
  let statusFilter = $state('all');
  const ru = $derived(language.value === 'ru');
  const statuses: Node['status'][] = ['enrolling', 'benchmarking', 'online', 'busy', 'warming', 'offline', 'draining', 'quarantined'];
  const filteredNodes = $derived(nodes.filter((node) => {
    const matchesSearch = `${node.name} ${node.id}`.toLowerCase().includes(search.trim().toLowerCase());
    return matchesSearch && (statusFilter === 'all' || node.status === statusFilter);
  }));

  function clearTransientState() {
    active = false;
    request?.abort();
    listRequest?.abort();
    request = undefined;
    listRequest = undefined;
    userCode = '';
    confirmedDevice = false;
    processing = false;
    nodes = [];
    nextCursor = null;
    loadingMore = false;
    errorMessage = '';
    listError = '';
    success = false;
  }

  onMount(() => {
    language.load();
    active = true;
    void session.ensureLoaded().then((loaded) => {
      if (!active) return;
      if (!loaded) { clearTransientState(); return; }
      generation = session.generation;
      removeSessionCleanup = session.addLiveCleanup(clearTransientState);
      void refreshNodes();
    });
    return () => {
      removeSessionCleanup();
      clearTransientState();
    };
  });

  function currentSession() {
    return active && session.generation === generation && session.session !== null;
  }

  function describeError(value: unknown): string {
    if (!(value instanceof ApiFailure)) return ru ? 'Сервис временно недоступен.' : 'The service is temporarily unavailable.';
    if (value.status === 401) {
      session.expireIfCurrent(generation);
      return ru ? 'Сессия завершена. Войдите снова.' : 'Your session ended. Sign in again.';
    }
    if (value.status === 410) return ru
      ? 'Срок действия кода истёк. Запустите вход ноды заново и используйте новый код.'
      : 'This code expired. Restart node sign-in and use the new code.';
    if (value.status === 409) return ru
      ? 'Этот код уже подтверждён или больше недоступен.'
      : 'This code was already approved or is no longer available.';
    if (value.status === 404) return ru
      ? 'Шлюз не нашёл эту ноду или не включил API управления нодами.'
      : 'The node was not found, or this gateway has not enabled node management.';
    if (value.status === 429) return ru
      ? `Слишком много попыток. Повторите позже${value.retryAfter !== null ? ` (через ${value.retryAfter} с)` : ''}.`
      : `Too many attempts. Retry later${value.retryAfter !== null ? ` (in ${value.retryAfter}s)` : ''}.`;
    if (value.code === 'invalid_request') return ru
      ? 'Проверьте код и имя ноды. Код состоит из трёх групп символов A–Z или 2–7.'
      : 'Check the code and node name. The code has three groups using letters A–Z or digits 2–7.';
    return ru ? 'Не удалось выполнить действие. Проверьте данные и повторите попытку.' : 'The action could not be completed. Check the details and try again.';
  }

  async function refreshNodes() {
    if (!currentSession()) return;
    listRequest?.abort();
    const controller = new AbortController();
    listRequest = controller;
    loadingNodes = true;
    listError = '';
    try {
      const page = await fetchNodes(50, undefined, controller.signal);
      if (controller.signal.aborted || !currentSession()) return;
      nodes = page.data;
      nextCursor = page.next_cursor;
    } catch (value) {
      if (!controller.signal.aborted && currentSession()) listError = describeError(value);
    } finally {
      if (listRequest === controller) listRequest = undefined;
      if (currentSession()) loadingNodes = false;
    }
  }

  async function loadMoreNodes() {
    if (!nextCursor || loadingMore || !currentSession()) return;
    listRequest?.abort();
    const controller = new AbortController();
    listRequest = controller;
    loadingMore = true;
    try {
      const page = await fetchNodes(50, nextCursor, controller.signal);
      if (controller.signal.aborted || !currentSession()) return;
      nodes = [...nodes, ...page.data];
      nextCursor = page.next_cursor;
    } catch (value) {
      if (!controller.signal.aborted && currentSession()) listError = describeError(value);
    } finally {
      if (listRequest === controller) listRequest = undefined;
      if (currentSession()) loadingMore = false;
    }
  }

  async function approve(event: SubmitEvent) {
    event.preventDefault();
    if (processing || !confirmedDevice || !currentSession()) return;
    const current = session.session;
    if (!current) return;
    const controller = new AbortController();
    request = controller;
    processing = true;
    errorMessage = '';
    success = false;
    try {
      await linkNode(userCode, nodeName, current.csrf_token, controller.signal);
      if (controller.signal.aborted || !currentSession()) return;
      userCode = '';
      confirmedDevice = false;
      success = true;
      await refreshNodes();
    } catch (value) {
      if (!controller.signal.aborted && currentSession()) errorMessage = describeError(value);
    } finally {
      if (request === controller) request = undefined;
      if (currentSession()) processing = false;
    }
  }

  function statusLabel(status: Node['status']) {
    const en: Record<Node['status'], string> = {
      enrolling: 'Connecting', benchmarking: 'Benchmarking', online: 'Online', busy: 'Busy',
      warming: 'Preparing model', offline: 'Offline', draining: 'Draining', quarantined: 'Quarantined'
    };
    const ruLabels: Record<Node['status'], string> = {
      enrolling: 'Подключается', benchmarking: 'Проверка', online: 'В сети', busy: 'Занята',
      warming: 'Готовит модель', offline: 'Не в сети', draining: 'Завершает работу', quarantined: 'Изолирована'
    };
    return (ru ? ruLabels : en)[status];
  }
</script>

<svelte:head><title>{ru ? 'Ноды · Cordova' : 'Nodes · Cordova'}</title></svelte:head>
<section class="account-content node-access-page">
  <p class="eyebrow">{ru ? 'УПРАВЛЕНИЕ НОДАМИ' : 'NODE MANAGEMENT'}</p>
  <h1>{ru ? 'Ваши ноды' : 'Your nodes'}</h1>
  <p class="account-intro">{ru ? 'Подключайте свои устройства и проверяйте их последнее подтверждённое состояние.' : 'Link your devices and review their latest confirmed state.'}</p>

  {#if errorMessage}<p class="key-error" role="alert">{errorMessage}</p>{/if}
  {#if success}<p class="key-message" role="status">{ru ? 'Вход подтверждён. Нода добавлена в список.' : 'Sign-in approved. The node was added to your list.'}</p>{/if}

  <section class="account-panel node-approval-panel" aria-labelledby="approval-heading">
    <div class="account-panel-heading"><div><p class="eyebrow">{ru ? 'ОДНОРАЗОВЫЙ КОД' : 'ONE-TIME CODE'}</p><h2 id="approval-heading">{ru ? 'Подключить ноду' : 'Link a node'}</h2></div></div>
    <ol class="node-approval-steps"><li>{ru ? 'На устройстве, которым вы владеете, запустите ' : 'On a device you control, run '}<code>cordova-node login</code>.</li><li>{ru ? 'Сверьте код, показанный в CLI, и введите его ниже до истечения срока.' : 'Check the code shown by the CLI and enter it below before it expires.'}</li></ol>
    <form class="node-approval-form" onsubmit={approve}>
      <label class="key-field">{ru ? 'Имя ноды' : 'Node name'}<input name="node-name" bind:value={nodeName} maxlength="64" required autocomplete="off" placeholder={ru ? 'Например, рабочий компьютер' : 'For example, workstation'} /></label>
      <label class="key-field">{ru ? 'Код из CLI ноды' : 'Code shown by the node CLI'}<input name="user-code" bind:value={userCode} oninput={(event) => { userCode = event.currentTarget.value.toUpperCase(); }} maxlength="15" pattern={'[A-Z2-7]{5}-[A-Z2-7]{5}-[A-Z2-7]{3}'} required autocomplete="off" autocapitalize="characters" spellcheck="false" placeholder="ABCDE-FGHIJ-KLM" aria-describedby="node-code-help" /></label>
      <small id="node-code-help" class="node-code-help">{ru ? 'Код остаётся на этой странице и не попадает в URL или хранилище браузера.' : 'The code stays on this page and is never put in the URL or browser storage.'}</small>
      <label class="privacy-accept"><input type="checkbox" bind:checked={confirmedDevice} /><span>{ru ? 'Я проверил(а) CLI-код и подтверждаю вход ноды на устройстве, которым управляю.' : 'I checked the CLI code and approve a node on a device I control.'}</span></label>
      <button class="primary" type="submit" disabled={processing || !confirmedDevice}>{processing ? (ru ? 'Подключаем…' : 'Linking…') : (ru ? 'Подтвердить и подключить' : 'Approve and link')}</button>
    </form>
    <p class="node-privacy-note">{ru ? 'Одобренная нода получает постоянный секрет устройства и позже может обрабатывать текст ваших запросов. Подтверждайте только устройство, которому доверяете.' : 'An approved node receives a persistent device secret and may later process your request text. Approve only a device you trust.'}</p>
  </section>

  <section class="account-panel node-list-panel" aria-labelledby="node-list-heading" aria-busy={loadingNodes}>
    <div class="account-panel-heading node-list-heading"><div><p class="eyebrow">{ru ? 'СОСТОЯНИЕ УСТРОЙСТВ' : 'DEVICE STATUS'}</p><h2 id="node-list-heading">{ru ? 'Список нод' : 'Node inventory'}</h2></div><button class="secondary" type="button" disabled={loadingNodes} onclick={() => void refreshNodes()}>{ru ? 'Обновить' : 'Refresh'}</button></div>
    {#if listError}<p class="key-error" role="alert">{listError}</p>{/if}
    <div class="node-list-filters">
      <label>{ru ? 'Поиск' : 'Search'}<input bind:value={search} type="search" placeholder={ru ? 'Имя или ID ноды' : 'Node name or ID'} /></label>
      <label>{ru ? 'Состояние' : 'Status'}<select bind:value={statusFilter}><option value="all">{ru ? 'Все состояния' : 'All statuses'}</option>{#each statuses as status}<option value={status}>{statusLabel(status)}</option>{/each}</select></label>
    </div>
    {#if loadingNodes}
      <p class="node-list-placeholder" role="status">{ru ? 'Загружаем список нод…' : 'Loading nodes…'}</p>
    {:else if !listError && nodes.length === 0}
      <div class="node-list-placeholder"><strong>{ru ? 'Нод пока нет' : 'No nodes yet'}</strong><p>{ru ? 'Подключите устройство по одноразовому коду выше.' : 'Link a device with the one-time code above.'}</p></div>
    {:else if !listError && filteredNodes.length === 0}
      <p class="node-list-placeholder">{ru ? 'Ноды не найдены. Измените поиск или фильтр.' : 'No nodes match this search. Change the search or filter.'}</p>
      {#if nextCursor}<button class="secondary node-load-more" type="button" disabled={loadingMore} onclick={() => void loadMoreNodes()}>{loadingMore ? (ru ? 'Загружаем…' : 'Loading…') : (ru ? 'Загрузить ещё ноды' : 'Load more nodes')}</button>{/if}
    {:else if !listError}
      <div class="table-scroll"><table class="node-table"><thead><tr><th>{ru ? 'Нода' : 'Node'}</th><th>{ru ? 'Состояние' : 'Status'}</th><th>{ru ? 'Оборудование' : 'Hardware'}</th><th>{ru ? 'Последний сигнал' : 'Last seen'}</th><th><span class="sr-only">{ru ? 'Действия' : 'Actions'}</span></th></tr></thead><tbody>
        {#each filteredNodes as node (node.id)}<tr><td data-label={ru ? 'Нода' : 'Node'}><a class="node-name-link" href={`/dashboard/nodes/${node.id}`}>{node.name}</a><small>#{node.id.slice(0, 8)}</small></td><td data-label={ru ? 'Состояние' : 'Status'}><span class={`node-status status-${node.status}`}>{statusLabel(node.status)}</span></td><td data-label={ru ? 'Оборудование' : 'Hardware'}>{node.hardware ? `${node.hardware.platform} · ${node.hardware.arch}` : (ru ? 'Не сообщено' : 'Not reported')}</td><td data-label={ru ? 'Последний сигнал' : 'Last seen'}>{node.last_seen_at ? new Date(node.last_seen_at).toLocaleString(ru ? 'ru' : 'en') : (ru ? 'Ещё не подключалась' : 'Not connected yet')}</td><td data-label={ru ? 'Действия' : 'Actions'}><a class="secondary node-detail-link" href={`/dashboard/nodes/${node.id}`}>{ru ? 'Открыть' : 'View'}</a></td></tr>{/each}
      </tbody></table></div>
      {#if nextCursor}<button class="secondary node-load-more" type="button" disabled={loadingMore} onclick={() => void loadMoreNodes()}>{loadingMore ? (ru ? 'Загружаем…' : 'Loading…') : (ru ? 'Загрузить ещё' : 'Load more')}</button>{/if}
    {/if}
  </section>
  <aside class="node-management-boundary"><strong>{ru ? 'Измерения и выплаты пока не доступны' : 'Measurements and payouts are not available yet'}</strong><p>{ru ? 'Шлюз показывает только полученные от ноды сведения. Рейтинг, тестовая ёмкость, скорость и начисления появятся после соответствующих этапов.' : 'The gateway shows only information reported by the node. Rank, tested capacity, throughput and earnings will appear after their server stages are implemented.'}</p></aside>
</section>
