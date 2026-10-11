<script lang="ts">
  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { ApiFailure } from '$lib/api/client.ts';
  import { fetchNode, prepareNodeModel, renameNode, revokeNode } from '$lib/api/account.ts';
  import type { Node } from '$lib/api/generated/contracts.ts';
  import { startNodeLive } from '$lib/live/nodes.ts';
  import { language } from '$lib/state/language.svelte.ts';
  import { session } from '$lib/state/session.svelte.ts';

  let active = $state(false);
  let generation = $state(-1);
  let request: AbortController | undefined;
  let removeSessionCleanup = () => {};
  let stopNodeLive = () => {};
  let pendingNodeRefresh = false;
  let loadedNodeID = '';
  let revokeDialog = $state<HTMLDialogElement>();
  let node = $state<Node | null>(null);
  let name = $state('');
  let loading = $state(true);
  let processing = $state(false);
  let errorMessage = $state('');
  let successMessage = $state('');
  let showRevoke = $state(false);
  let confirmedRevoke = $state(false);
  const ru = $derived(language.value === 'ru');

  function clearTransientState() {
    active = false;
    stopNodeLive();
    stopNodeLive = () => {};
    request?.abort();
    request = undefined;
    node = null;
    name = '';
    processing = false;
    errorMessage = '';
    successMessage = '';
    showRevoke = false;
    confirmedRevoke = false;
    pendingNodeRefresh = false;
  }

  onMount(() => {
    language.load();
    active = true;
    void session.ensureLoaded().then((loaded) => {
      if (!active) return;
      if (!loaded) { clearTransientState(); return; }
      generation = session.generation;
      removeSessionCleanup = session.addLiveCleanup(clearTransientState);
      stopNodeLive = startNodeLive((resourceIDs) => {
        const id = page.params.id;
        if (!id || !currentSession() || (resourceIDs !== null && !resourceIDs.includes(id))) return;
        if (processing) {
          pendingNodeRefresh = true;
          return;
        }
        void loadNode(id);
      });
    });
    return () => {
      removeSessionCleanup();
      stopNodeLive();
      stopNodeLive = () => {};
      clearTransientState();
    };
  });

  function currentSession() {
    return active && session.generation === generation && session.session !== null;
  }

  function finishProcessing() {
    if (!currentSession()) return;
    processing = false;
    if (!pendingNodeRefresh || !node) return;
    pendingNodeRefresh = false;
    void loadNode(node.id);
  }

  $effect(() => {
    if (showRevoke && revokeDialog && !revokeDialog.open) revokeDialog.showModal();
  });

  function closeRevoke() {
    showRevoke = false;
    confirmedRevoke = false;
  }

  $effect(() => {
    const id = page.params.id;
    const currentGeneration = session.generation;
    if (!id || !currentSession() || (id === loadedNodeID && node?.id === id)) return;
    loadedNodeID = id;
    void loadNode(id, currentGeneration);
  });

  async function loadNode(id: string, currentGeneration = generation) {
    request?.abort();
    const controller = new AbortController();
    request = controller;
    loading = true;
    errorMessage = '';
    successMessage = '';
    try {
      const value = await fetchNode(id, controller.signal);
      if (controller.signal.aborted || !currentSession() || session.generation !== currentGeneration) return;
      node = value;
      name = value.name;
    } catch (value) {
      if (!controller.signal.aborted && currentSession() && session.generation === currentGeneration) {
        node = null;
        errorMessage = describeError(value);
      }
    } finally {
      if (request === controller) request = undefined;
      if (currentSession() && session.generation === currentGeneration) loading = false;
    }
  }

  function describeError(value: unknown): string {
    if (!(value instanceof ApiFailure)) return ru ? 'Сервис временно недоступен.' : 'The service is temporarily unavailable.';
    if (value.status === 401) {
      session.expireIfCurrent(generation);
      return ru ? 'Сессия завершена. Войдите снова.' : 'Your session ended. Sign in again.';
    }
    if (value.status === 404) return ru ? 'Нода не найдена.' : 'Node not found.';
    if (value.status === 429) return ru ? 'Слишком много запросов. Повторите позже.' : 'Too many requests. Retry later.';
    if (value.code === 'invalid_request') return ru ? 'Проверьте имя ноды.' : 'Check the node name.';
    return ru ? 'Не удалось выполнить действие. Повторите попытку.' : 'The action could not be completed. Try again.';
  }

  async function saveName(event: SubmitEvent) {
    event.preventDefault();
    if (!node || processing || !currentSession()) return;
    const current = session.session;
    if (!current) return;
    const controller = new AbortController();
    request = controller;
    processing = true;
    errorMessage = '';
    successMessage = '';
    try {
      node = await renameNode(node.id, name, current.csrf_token, controller.signal);
      if (controller.signal.aborted || !currentSession()) return;
      name = node.name;
      successMessage = ru ? 'Имя ноды обновлено.' : 'Node name updated.';
    } catch (value) {
      if (!controller.signal.aborted && currentSession()) errorMessage = describeError(value);
    } finally {
      if (request === controller) request = undefined;
      finishProcessing();
    }
  }

  async function confirmRevoke() {
    if (!node || !confirmedRevoke || processing || !currentSession()) return;
    const current = session.session;
    if (!current) return;
    const controller = new AbortController();
    request = controller;
    processing = true;
    errorMessage = '';
    try {
      await revokeNode(node.id, current.csrf_token, controller.signal);
      if (controller.signal.aborted || !currentSession()) return;
      node = null;
      showRevoke = false;
      await goto('/dashboard/nodes', { replaceState: true });
    } catch (value) {
      if (!controller.signal.aborted && currentSession()) errorMessage = describeError(value);
    } finally {
      if (request === controller) request = undefined;
      finishProcessing();
    }
  }

  async function prepareModel(modelID: string) {
    if (!node || processing || !currentSession()) return;
    const current = session.session;
    if (!current) return;
    const controller = new AbortController();
    request = controller;
    processing = true;
    errorMessage = '';
    successMessage = '';
    try {
      const result = await prepareNodeModel(node.id, modelID, current.csrf_token, controller.signal);
      if (controller.signal.aborted || !currentSession()) return;
      successMessage = result.status === 'ready'
        ? (ru ? `Модель ${modelID} готова и прошла проверку ёмкости.` : `Model ${modelID} is ready and capacity-verified.`)
        : result.status === 'benchmarking'
          ? (ru ? `Модель ${modelID} загружена; сервер проверяет её ёмкость.` : `Model ${modelID} is loaded; the server is checking its capacity.`)
          : (ru ? `Подготовка ${modelID} началась. Обновите сведения через несколько секунд.` : `Preparation for ${modelID} started. Refresh the node details in a few seconds.`);
      if (result.status === 'ready') node = await fetchNode(node.id, controller.signal);
    } catch (value) {
      if (!controller.signal.aborted && currentSession()) {
        if (value instanceof ApiFailure && value.code === 'model_not_found') {
          errorMessage = ru ? 'Этой модели нет в подписанном каталоге.' : 'This model is not in the signed catalog.';
        } else if (value instanceof ApiFailure && value.code === 'model_warming') {
          errorMessage = ru ? 'Нода уже готовит модель. Повторите позже.' : 'The node is already preparing a model. Try again later.';
        } else if (value instanceof ApiFailure && value.code === 'no_capacity') {
          errorMessage = ru ? 'Нода сейчас не может подготовить модель: проверьте подключение и ресурсы.' : 'The node cannot prepare this model right now. Check its connection and resources.';
        } else {
          errorMessage = describeError(value);
        }
      }
    } finally {
      if (request === controller) request = undefined;
      finishProcessing();
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

  function xmr(piconero: string | null) {
    if (piconero === null) return ru ? 'Пока недоступно' : 'Not available yet';
    const value = BigInt(piconero);
    const whole = value / 1_000_000_000_000n;
    const fraction = (value % 1_000_000_000_000n).toString().padStart(12, '0').replace(/0+$/, '');
    return `${whole}${fraction ? `.${fraction}` : ''} XMR`;
  }

  function bytes(value: number | null) {
    if (value === null) return ru ? 'Ещё не проверено' : 'Not tested yet';
    return `${(value / (1024 ** 3)).toFixed(2)} GiB`;
  }
</script>

<svelte:head><title>{ru ? 'Сведения о ноде · Cordova' : 'Node details · Cordova'}</title></svelte:head>
<section class="account-content node-detail-page" aria-busy={loading}>
  <a class="node-back-link" href="/dashboard/nodes">← {ru ? 'Все ноды' : 'All nodes'}</a>
  {#if loading}
    <p class="node-list-placeholder" role="status">{ru ? 'Загружаем сведения о ноде…' : 'Loading node details…'}</p>
  {:else if errorMessage && !node}
    <p class="key-error" role="alert">{errorMessage}</p>
  {:else if node}
    <p class="eyebrow">{ru ? 'УСТРОЙСТВО' : 'DEVICE'}</p>
    <div class="node-detail-title"><div><h1>{node.name}</h1><p class="account-intro">#{node.id}</p></div><span class={`node-status status-${node.status}`}>{statusLabel(node.status)}</span></div>
    {#if errorMessage}<p class="key-error" role="alert">{errorMessage}</p>{/if}
    {#if successMessage}<p class="key-message" role="status">{successMessage}</p>{/if}

    <div class="node-detail-grid">
      <section class="account-panel"><p class="eyebrow">{ru ? 'СВЕДЕНИЯ' : 'OVERVIEW'}</p><h2>{ru ? 'Подключение' : 'Connection'}</h2>
        <dl class="node-facts">
          <div><dt>{ru ? 'Последний сигнал' : 'Last seen'}</dt><dd>{node.last_seen_at ? new Date(node.last_seen_at).toLocaleString(ru ? 'ru' : 'en') : (ru ? 'Ещё не подключалась' : 'Not connected yet')}</dd></div>
          <div><dt>{ru ? 'Рейтинг' : 'Rank'}</dt><dd>{node.rank === null ? (ru ? 'Ещё не рассчитан' : 'Not calculated yet') : `${node.rank} / 100`}<small>{node.rank_updated_at ? new Date(node.rank_updated_at).toLocaleString(ru ? 'ru' : 'en') : (ru ? 'Рейтинг недоступен до этапа оценки' : 'Unavailable until the ranking stage')}</small></dd></div>
          <div><dt>{ru ? 'Проверенная ёмкость' : 'Verified capacity'}</dt><dd>{bytes(node.verified_capacity_bytes)}</dd></div>
          <div><dt>{ru ? 'Измеренная скорость' : 'Measured throughput'}</dt><dd>{node.measured_tokens_per_second === null ? (ru ? 'Ещё не измерена' : 'Not measured yet') : `${node.measured_tokens_per_second.toFixed(2)} tokens/s`}</dd></div>
          <div><dt>{ru ? 'Доступность' : 'Uptime ratio'}</dt><dd>{node.uptime_ratio === null ? (ru ? 'Недостаточно данных' : 'Not enough data') : `${(node.uptime_ratio * 100).toFixed(1)}%`}</dd></div>
          <div><dt>{ru ? 'Успешные запросы' : 'Success ratio'}</dt><dd>{node.success_ratio === null ? (ru ? 'Недостаточно данных' : 'Not enough data') : `${(node.success_ratio * 100).toFixed(1)}%`}</dd></div>
        </dl>
      </section>

      <section class="account-panel"><p class="eyebrow">{ru ? 'ОБОРУДОВАНИЕ' : 'HARDWARE'}</p><h2>{ru ? 'Отчёт ноды' : 'Node report'}</h2>
        {#if node.hardware}
          <dl class="node-facts">
            <div><dt>{ru ? 'Платформа' : 'Platform'}</dt><dd>{node.hardware.platform} · {node.hardware.arch}</dd></div>
            <div><dt>{ru ? 'Вычислительный backend' : 'Compute backend'}</dt><dd>{node.hardware.backend}</dd></div>
            <div><dt>{ru ? 'Графика (сообщено)' : 'GPU (reported)'}</dt><dd>{node.hardware.gpu_name ?? (ru ? 'Нет' : 'None')}</dd></div>
            <div><dt>{ru ? 'Общая VRAM (сообщено)' : 'Total VRAM (reported)'}</dt><dd>{bytes(node.hardware.reported_total_vram_bytes)}</dd></div>
            <div><dt>{ru ? 'Свободная VRAM (сообщено)' : 'Free VRAM (reported)'}</dt><dd>{bytes(node.hardware.reported_free_vram_bytes)}</dd></div>
            <div><dt>{ru ? 'Оперативная память (сообщено)' : 'RAM (reported)'}</dt><dd>{bytes(node.hardware.ram_bytes)}</dd></div>
          </dl>
        {:else}<p class="node-list-placeholder">{ru ? 'Нода ещё не сообщила сведения об оборудовании.' : 'The node has not reported hardware details yet.'}</p>{/if}
      </section>

      <section class="account-panel"><p class="eyebrow">{ru ? 'МОДЕЛИ' : 'MODELS'}</p><h2>{ru ? 'Состояние моделей' : 'Model state'}</h2>
        {#if node.models.length === 0}<p class="node-list-placeholder">{ru ? 'Нет текущих отчётов о моделях.' : 'No current model reports.'}</p>
        {:else}<ul class="node-model-list">{#each node.models as model (model.model_id)}<li><div><strong>{model.model_id}</strong><small>{new Date(model.updated_at).toLocaleString(ru ? 'ru' : 'en')}</small></div><span>{model.state}</span><small>{model.active_slots} / {model.max_slots} {ru ? 'слотов' : 'slots'}</small><button class="secondary" type="button" aria-label={`${ru ? 'Подготовить модель' : 'Prepare model'} ${model.model_id}`} disabled={processing || node.status === 'offline' || node.status === 'draining' || node.status === 'quarantined'} onclick={() => void prepareModel(model.model_id)}>{processing ? (ru ? 'Подготовка…' : 'Preparing…') : (model.state === 'loaded' ? (ru ? 'Проверить готовность' : 'Check readiness') : (ru ? 'Подготовить' : 'Prepare'))}</button></li>{/each}</ul>{/if}
      </section>

      <section class="account-panel"><p class="eyebrow">{ru ? 'НАЧИСЛЕНИЯ' : 'EARNINGS'}</p><h2>{ru ? 'Выплаты ноды' : 'Node earnings'}</h2>
        <dl class="node-facts"><div><dt>{ru ? 'Ожидают подтверждения' : 'Pending'}</dt><dd>{xmr(node.earnings_pending_piconero)}</dd></div><div><dt>{ru ? 'Доступно' : 'Available'}</dt><dd>{xmr(node.earnings_available_piconero)}</dd></div><div><dt>{ru ? 'Удержание до' : 'Held until'}</dt><dd>{node.hold_until ? new Date(node.hold_until).toLocaleString(ru ? 'ru' : 'en') : (ru ? 'Нет активной информации' : 'No active hold information')}</dd></div></dl>
      </section>
    </div>

    <section class="account-panel node-settings-panel"><p class="eyebrow">{ru ? 'НАСТРОЙКИ ДОСТУПА' : 'ACCESS SETTINGS'}</p><h2>{ru ? 'Переименовать ноду' : 'Rename node'}</h2>
      <form class="node-rename-form" onsubmit={saveName}><label class="key-field">{ru ? 'Имя ноды' : 'Node name'}<input bind:value={name} maxlength="64" required autocomplete="off" /></label><button class="primary" type="submit" disabled={processing || name.trim() === node.name}>{ru ? 'Сохранить имя' : 'Save name'}</button></form>
    </section>
    <section class="node-revoke-panel"><div><h2>{ru ? 'Отозвать эту ноду' : 'Revoke this node'}</h2><p>{ru ? 'Отзыв отключит текущую сессию и запретит устройству входить снова с сохранённым секретом.' : 'Revoking disconnects the current session and prevents this device from signing in again with its stored secret.'}</p></div><button class="danger" type="button" onclick={() => { showRevoke = true; confirmedRevoke = false; }}>{ru ? 'Отозвать ноду' : 'Revoke node'}</button></section>
  {/if}

  {#if showRevoke && node}
    <dialog class="node-confirm-dialog" bind:this={revokeDialog} onclose={closeRevoke} aria-labelledby="revoke-heading">
      <p class="eyebrow">{ru ? 'ПОДТВЕРДИТЕ ОТЗЫВ' : 'CONFIRM REVOCATION'}</p><h2 id="revoke-heading">{ru ? `Отозвать ${node.name}?` : `Revoke ${node.name}?`}</h2>
      <p>{ru ? 'Текущая сессия будет закрыта. Если запросы выполняются, они могут завершиться ошибкой. Для повторного подключения потребуется новый вход с устройством.' : 'The current session will be closed. Any active requests may fail. To reconnect, start a new sign-in on the device.'}</p>
      <label class="privacy-accept"><input type="checkbox" bind:checked={confirmedRevoke} /><span>{ru ? 'Я понимаю, что это устройство потеряет доступ.' : 'I understand this device will lose access.'}</span></label>
      <div class="node-dialog-actions"><button class="secondary" type="button" onclick={() => revokeDialog?.close()}>{ru ? 'Отмена' : 'Cancel'}</button><button class="danger" type="button" disabled={processing || !confirmedRevoke} onclick={() => void confirmRevoke()}>{processing ? (ru ? 'Отзываем…' : 'Revoking…') : (ru ? 'Подтвердить отзыв' : 'Confirm revoke')}</button></div>
    </dialog>
  {/if}
</section>
